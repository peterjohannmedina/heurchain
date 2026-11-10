package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

// Permissive email check — RFC 5322 is too strict for what we need; we only
// want to reject obvious garbage.
var emailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

// ProvisionRequest carries the inputs to create a tenant + initial API key.
// Name and Email are required.
type ProvisionRequest struct {
	Name  string
	Email string
}

func (r *ProvisionRequest) validate() error {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return errors.New("name is required")
	}
	if len(r.Name) > 200 {
		return errors.New("name exceeds 200 characters")
	}
	r.Email = strings.TrimSpace(r.Email)
	if r.Email == "" {
		return errors.New("email is required")
	}
	if !emailPattern.MatchString(r.Email) {
		return errors.New("email is not well-formed")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Core functions
// ---------------------------------------------------------------------------

// provisionTenant creates a new tenant + one API key. Returns the raw API key
// (shown once, never stored). The key is bcrypt-hashed before storage.
func provisionTenant(ctx context.Context, db *sql.DB, pr ProvisionRequest) (tenantID, rawKey string, err error) {
	if err := pr.validate(); err != nil {
		return "", "", err
	}

	tenantID = uuid.New().String()

	b := make([]byte, 16)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate key: %w", err)
	}
	rawKey = "hc_" + hex.EncodeToString(b)

	hash, err := bcrypt.GenerateFromPassword([]byte(rawKey), bcrypt.DefaultCost)
	if err != nil {
		return "", "", fmt.Errorf("hash key: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)

	_, err = db.ExecContext(ctx,
		`INSERT INTO tenants (id, name, email, status, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		tenantID, pr.Name, pr.Email, "active", now,
	)
	if err != nil {
		return "", "", fmt.Errorf("insert tenant: %w", err)
	}

	_, err = db.ExecContext(ctx,
		`INSERT INTO api_keys (tenant_id, key_hash, name, created_at) VALUES (?, ?, ?, ?)`,
		tenantID, string(hash), "default", now,
	)
	if err != nil {
		return "", "", fmt.Errorf("insert api key: %w", err)
	}

	return tenantID, rawKey, nil
}

// errKeyNotFound is returned when the target api_key row does not exist, so
// callers can map it to 404 without string-matching the message.
var errKeyNotFound = errors.New("api key not found")

// revokeKey marks an api_key as revoked and calls revokeAuthCache.
func revokeKey(ctx context.Context, db *sql.DB, rdb *redis.Client, keyID int64, rawKey string) error {
	// Read the owning tenant BEFORE the update so the auth-cache epoch can be
	// bumped even when the caller did not supply the raw key.
	//
	// A missing key ID is an error, not a no-op: reporting {"revoked": true}
	// for a key that never existed hides typo'd IDs and would let an operator
	// believe a credential was killed when nothing happened.
	var tenantID string
	err := db.QueryRowContext(ctx,
		`SELECT tenant_id FROM api_keys WHERE id = ?`, keyID).Scan(&tenantID)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: id %d", errKeyNotFound, keyID)
	}
	if err != nil {
		return fmt.Errorf("lookup key tenant: %w", err)
	}

	res, err := db.ExecContext(ctx,
		`UPDATE api_keys SET revoked_at = CURRENT_TIMESTAMP WHERE id = ?`,
		keyID,
	)
	if err != nil {
		return fmt.Errorf("revoke key: %w", err)
	}
	// Guard against a concurrent delete between the SELECT and the UPDATE.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("%w: id %d", errKeyNotFound, keyID)
	}

	// Targeted invalidation when the raw key is known.
	if rawKey != "" {
		revokeAuthCache(ctx, rdb, rawKey)
	}

	// The cache is keyed by sha256(rawKey), and api_keys stores only a bcrypt
	// hash — so without the raw key there is no way to address that entry
	// directly. Revoking by key ID alone (the /admin/tenants/{t}/keys/{id}
	// path, where raw_key is optional) would otherwise leave the revoked key
	// authenticating from cache for up to authCacheTTL. Bumping the tenant's
	// epoch invalidates every cached entry for that tenant at once, which is
	// the only correct option here: a revoked credential must stop working
	// immediately, and re-bcrypting the tenant's other live keys once is a
	// cheap price for that.
	bumpAuthEpoch(ctx, rdb, tenantID)
	return nil
}

// ---------------------------------------------------------------------------
// Admin auth helper
// ---------------------------------------------------------------------------

func (s *Server) checkAdminAuth(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}
	expectedUser := os.Getenv("ADMIN_USER")
	expectedPass := os.Getenv("ADMIN_PASS")
	if expectedUser == "" || expectedPass == "" {
		return false
	}
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(expectedUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(expectedPass)) == 1
	return userOK && passOK
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// tenantStatusGuard blocks requests when tenants.status != 'active'.
// Runs before the auth middleware-equivalent inside handlers, so it must
// re-validate the tenant id header itself. Returns 403 for suspended/cancelled
// tenants so callers know the difference from 401 (bad key).
func (s *Server) tenantStatusGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if handleCORSPreflight(w, r) {
			return
		}
		// The guard's 403/500 rejections precede the handler's own CORS setup,
		// so a cross-origin client could not read them.
		setCORSHeaders(w)
		tenantID := r.Header.Get(s.config.Auth.TenantHeader)
		if tenantID == "" || !validID.MatchString(tenantID) {
			jsonError(w, "missing or invalid tenant id", http.StatusBadRequest)
			return
		}
		var status string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT status FROM tenants WHERE id = ?`, tenantID).Scan(&status)
		if err == sql.ErrNoRows {
			jsonError(w, "tenant not found", http.StatusForbidden)
			return
		}
		if err != nil {
			jsonError(w, "tenant status check failed", http.StatusInternalServerError)
			return
		}
		if status != "active" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":         "tenant " + status,
				"tenant_status": status,
			})
			return
		}
		next(w, r)
	}
}

// rpmGuard enforces the per-tenant requests-per-minute limit from
// rate_limit.default_rpm. A configured value of 0 means unlimited. The guard
// fails open on Redis errors so a transient backend issue does not take down
// the API; tenantStatusGuard already gates unknown/suspended tenants. On breach
// it returns 429 with a Retry-After header and a {limit, current} body.
func (s *Server) rpmGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if handleCORSPreflight(w, r) {
			return
		}
		// This guard's 429 is the status a client is supposed to back off on;
		// without CORS headers it arrives as an unreadable cross-origin error,
		// so a well-behaved client has no way to read Retry-After.
		setCORSHeaders(w)
		tenantID := r.Header.Get(s.config.Auth.TenantHeader)
		if tenantID == "" || !validID.MatchString(tenantID) {
			jsonError(w, "missing or invalid tenant id", http.StatusBadRequest)
			return
		}

		// Authenticate BEFORE consuming quota. The counter is keyed on the
		// caller-supplied tenant header, so incrementing it ahead of auth let
		// anyone who merely knew a tenant id exhaust that tenant's RPM budget
		// with junk keys and 429 its legitimate agents — a denial of service
		// requiring no credentials at all. Every route wrapped by rpmGuard is
		// API-key authenticated, so the key is always available here, and the
		// result is Redis-cached so the handler's own validateAPIKey call is a
		// cache hit rather than a second bcrypt.
		//
		// Only a definitively-invalid key rejects. Backend errors fall through
		// to the handler, preserving this guard's existing fail-open posture.
		apiKey := r.Header.Get(s.config.Auth.APIKeyHeader)
		if _, err := validateAPIKey(r.Context(), s.rdb, s.db, tenantID, apiKey); err != nil {
			if errors.Is(err, errUnauthorized) {
				jsonError(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r) // backend trouble — let the handler decide
			return
		}

		limitRPM := s.config.RateLimit.DefaultRPM
		if limitRPM <= 0 {
			next(w, r) // unlimited
			return
		}

		// Sliding window, approximated with two fixed sub-windows.
		//
		// The previous implementation used one hard-aligned 60s bucket
		// (now.Unix()/60), which let a caller spend its full allowance in the
		// last instant of one bucket and its full allowance again in the first
		// instant of the next — up to 2*RPM-1 requests inside a second, and a
		// sustained ~2x the configured rate by timing bursts on the minute
		// boundary.
		//
		// Here the current sub-window's count is added to a time-weighted share
		// of the previous one, so the effective window slides with the caller
		// rather than resetting on a wall-clock edge. This is the standard
		// approximation: one extra Redis read, no sorted sets, no per-request
		// trimming, and it cannot be gamed by alignment.
		now := time.Now()
		const windowSec = 60
		elapsed := now.Unix() % windowSec // seconds into the current sub-window
		bucket := now.Unix() / windowSec
		curKey := fmt.Sprintf("hc:rpm:%s:%d", tenantID, bucket)
		prevKey := fmt.Sprintf("hc:rpm:%s:%d", tenantID, bucket-1)

		pipe := s.rdb.Pipeline()
		incr := pipe.Incr(r.Context(), curKey)
		// TTL covers this sub-window plus the next, so the previous count is
		// still readable while it carries weight.
		pipe.Expire(r.Context(), curKey, 2*windowSec*time.Second)
		prevGet := pipe.Get(r.Context(), prevKey)
		if _, err := pipe.Exec(r.Context()); err != nil && err != redis.Nil {
			// Fail open on Redis trouble, as before: a rate limiter must not
			// become an outage. Logged because silently unlimited is worse than
			// noisy — this previously failed open with no signal at all.
			log.Printf("rpmGuard: redis error tenant=%s, failing open: %v", tenantID, err)
			next(w, r)
			return
		}

		count := incr.Val()
		prevCount, _ := strconv.ParseInt(prevGet.Val(), 10, 64)

		// Weight the previous sub-window by the fraction of it still inside the
		// sliding window.
		weighted := float64(prevCount)*(float64(windowSec-elapsed)/float64(windowSec)) + float64(count)

		if weighted > float64(limitRPM) {
			// Retry-After: when enough of the previous window will have aged out
			// for this request to fit. Falls back to the remainder of the
			// current sub-window when the current count alone is already over.
			retryAfter := int64(windowSec - elapsed)
			if count <= int64(limitRPM) && prevCount > 0 {
				need := (weighted - float64(limitRPM)) / float64(prevCount) * float64(windowSec)
				if secs := int64(need) + 1; secs < retryAfter {
					retryAfter = secs
				}
			}
			if retryAfter < 1 {
				retryAfter = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error":   "rate limit exceeded",
				"limit":   limitRPM,
				"current": int64(weighted),
			})
			return
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// POST /admin/tenants
//
//	Body: {
//	  "name": "Acme Inc",                         // required
//	  "email": "ops@acme.example",                // required
//	}
//
// Auth: admin basic auth
// Response: {"tenant_id": "...", "api_key": "...", "name", "email"}
func (s *Server) handleAdminCreateTenant(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.checkAdminAuth(r) {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if !decodeJSONBody(w, r, &req, 0) {
		return
	}

	pr := ProvisionRequest{Name: req.Name, Email: req.Email}
	tenantID, rawKey, err := provisionTenant(r.Context(), s.db, pr)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	respondJSON(w, map[string]interface{}{
		"tenant_id": tenantID,
		"api_key":   rawKey,
		"name":      pr.Name,
		"email":     pr.Email,
	})
}

// DELETE /admin/tenants/{id}/keys/{key_id}
// Auth: admin basic auth
// Body: {"raw_key": "..."} — needed to clear auth cache
// Response: {"revoked": true}
func (s *Server) handleAdminRevokeKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.checkAdminAuth(r) {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// Parse /admin/tenants/{id}/keys/{key_id}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 5 || parts[0] != "admin" || parts[1] != "tenants" || parts[3] != "keys" {
		jsonError(w, "invalid path", http.StatusBadRequest)
		return
	}
	keyID, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		jsonError(w, "invalid key id", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		RawKey string `json:"raw_key"`
	}
	if !decodeJSONBody(w, r, &req, 0) {
		return
	}

	if err := revokeKey(r.Context(), s.db, s.rdb, keyID, req.RawKey); err != nil {
		// An unknown key ID is a client error, not a server fault.
		if errors.Is(err, errKeyNotFound) {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	respondJSON(w, map[string]interface{}{"revoked": true})
}

// GET /admin/tenants
// Auth: admin basic auth
// Response: {"tenants": [{id, status, created_at}]}
func (s *Server) handleAdminListTenants(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.checkAdminAuth(r) {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `SELECT id, status, created_at FROM tenants ORDER BY created_at DESC`)
	if err != nil {
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type tenantResp struct {
		ID        string `json:"id"`
		Status    string `json:"status"`
		CreatedAt string `json:"created_at"`
	}

	var tenants []tenantResp
	for rows.Next() {
		var t tenantResp
		if err := rows.Scan(&t.ID, &t.Status, &t.CreatedAt); err != nil {
			continue
		}
		tenants = append(tenants, t)
	}

	respondJSON(w, map[string]interface{}{"tenants": tenants})
}
