package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

var errUnauthorized = errors.New("unauthorized")

const authCacheTTL = 5 * time.Minute

type cachedAuth struct {
	KeyID    int64  `json:"key_id"`
	TenantID string `json:"tenant_id"`
	Epoch    int64  `json:"epoch"`
}

func authCacheKey(rawKey string) string {
	h := sha256.Sum256([]byte(rawKey))
	return fmt.Sprintf("hc:auth:%x", h)
}

func authEpochKey(tenantID string) string {
	return "hc:auth:epoch:" + tenantID
}

// authEpoch returns the tenant's auth-cache epoch (0 when unset). Cached
// entries minted under an older epoch are ignored, so bumping the epoch
// invalidates every cached key for the tenant at once.
func authEpoch(ctx context.Context, rdb *redis.Client, tenantID string) int64 {
	v, err := rdb.Get(ctx, authEpochKey(tenantID)).Int64()
	if err != nil {
		return 0
	}
	return v
}

// bumpAuthEpoch invalidates all cached auth entries for a tenant. MUST be
// called whenever any of the tenant's keys is revoked or rotated — without
// it, a revoked key keeps authenticating from cache for up to authCacheTTL.
func bumpAuthEpoch(ctx context.Context, rdb *redis.Client, tenantID string) {
	if err := rdb.Incr(ctx, authEpochKey(tenantID)).Err(); err != nil {
		log.Printf("auth epoch bump failed tenant=%s: %v (cached keys may persist up to %s)", tenantID, err, authCacheTTL)
	}
}

// validateAPIKey checks rawKey against bcrypt-hashed keys in SQLite for the given tenant.
// Cache: on success, the key_id is stored in Redis for authCacheTTL to skip bcrypt on
// subsequent requests. Failures are never cached (prevents timing-based enumeration).
func validateAPIKey(ctx context.Context, rdb *redis.Client, db *sql.DB, tenantID, rawKey string) (int64, error) {
	ck := authCacheKey(rawKey)
	epoch := authEpoch(ctx, rdb, tenantID)

	// Fast path — cache hit skips bcrypt entirely. Entries from an older
	// epoch (i.e. minted before the last rotation/revocation) are ignored.
	if cached, err := rdb.Get(ctx, ck).Result(); err == nil {
		var ca cachedAuth
		if json.Unmarshal([]byte(cached), &ca) == nil && ca.TenantID == tenantID && ca.Epoch == epoch {
			return ca.KeyID, nil
		}
	}

	// Slow path — bcrypt validation against SQLite.
	// Read all candidates first and close rows BEFORE any further db calls,
	// because main.go sets MaxOpenConns=1 (modernc.org/sqlite single-writer).
	// Holding rows open while calling db.ExecContext would deadlock.
	rows, err := db.QueryContext(ctx,
		`SELECT id, key_hash FROM api_keys WHERE tenant_id = ? AND revoked_at IS NULL`,
		tenantID)
	if err != nil {
		return 0, fmt.Errorf("query keys: %w", err)
	}
	type keyRow struct {
		id   int64
		hash string
	}
	var candidates []keyRow
	for rows.Next() {
		var k keyRow
		if err := rows.Scan(&k.id, &k.hash); err == nil {
			candidates = append(candidates, k)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("scan keys: %w", err)
	}

	for _, k := range candidates {
		if bcrypt.CompareHashAndPassword([]byte(k.hash), []byte(rawKey)) == nil {
			// Fire-and-forget: don't block the auth path on the single SQLite writer.
			// Bare goroutine with no handler recover() above it, so a panic here
			// would take down the whole process.
			go func(keyID int64) {
				defer func() {
					if rec := recover(); rec != nil {
						log.Printf("auth: PANIC recovered updating last_used_at key=%d: %v", keyID, rec)
					}
				}()
				_, _ = db.ExecContext(context.Background(),
					"UPDATE api_keys SET last_used_at = CURRENT_TIMESTAMP WHERE id = ?",
					keyID)
			}(k.id)
			if b, err := json.Marshal(cachedAuth{KeyID: k.id, TenantID: tenantID, Epoch: epoch}); err == nil {
				_ = rdb.Set(ctx, ck, b, authCacheTTL).Err()
			}
			return k.id, nil
		}
	}
	return 0, errUnauthorized
}

// revokeAuthCache removes a key's cache entry immediately when the key is revoked.
// Call this whenever api_keys.revoked_at is set.
func revokeAuthCache(ctx context.Context, rdb *redis.Client, rawKey string) {
	_ = rdb.Del(ctx, authCacheKey(rawKey)).Err()
}
