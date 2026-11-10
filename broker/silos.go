package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Cryptographic client silos.
//
// A silo is a per-client compartment inside a tenant (firm): its content is
// encrypted under a per-silo DEK and lives in a dedicated Redis keyspace
// (tenant:{id}:silo:{name}:...) that the legacy tenant-wide paths never scan.
// silo_grants on api_keys are the enforcement plane; sessions/engagements are
// ergonomics on top (see enforce.go).
// ---------------------------------------------------------------------------

const quarantineSilo = "_quarantine"

type siloRow struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Isolation   string `json:"isolation"`
	Crypto      string `json:"crypto"`
	ShardMode   string `json:"shard_mode"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
}

func siloVecKey(tenantID, siloName, docID string) string {
	return fmt.Sprintf("tenant:%s:silo:%s:vec:%s", tenantID, siloName, docID)
}

func siloVecSetKey(tenantID, siloName string) string {
	return fmt.Sprintf("tenant:%s:silo:%s:vecs", tenantID, siloName)
}

// requireSiloCrypto returns the crypto context or writes a 503.
func (s *Server) requireSiloCrypto(w http.ResponseWriter) *siloCrypto {
	if s.crypto == nil {
		jsonError(w, "silo features not configured on this deployment", http.StatusServiceUnavailable)
		return nil
	}
	return s.crypto
}

// authTenantKey authenticates the request headers and returns (tenantID,
// keyID, role). role is read from api_keys.role (added in migration v4);
// legacy keys default to 'tenant'.
func (s *Server) authTenantKey(w http.ResponseWriter, r *http.Request) (string, int64, string, bool) {
	apiKey := r.Header.Get(s.config.Auth.APIKeyHeader)
	tenantID := r.Header.Get(s.config.Auth.TenantHeader)
	if apiKey == "" {
		jsonError(w, "missing api key", http.StatusUnauthorized)
		return "", 0, "", false
	}
	if tenantID == "" || !validID.MatchString(tenantID) {
		jsonError(w, "missing or invalid tenant id", http.StatusBadRequest)
		return "", 0, "", false
	}
	keyID, err := validateAPIKey(r.Context(), s.rdb, s.db, tenantID, apiKey)
	if err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return "", 0, "", false
	}
	// FAIL CLOSED on the role lookup: every role gate in this feature is
	// "tenant/admin or forbidden", so defaulting to 'tenant' on a lookup
	// error would silently elevate a consultant key.
	var role string
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(role, 'tenant') FROM api_keys WHERE id = ?`, keyID).Scan(&role); err != nil {
		log.Printf("role lookup failed key=%d tenant=%s: %v", keyID, tenantID, err)
		jsonError(w, "authorization check failed", http.StatusInternalServerError)
		return "", 0, "", false
	}
	return tenantID, keyID, role, true
}

// ---------------------------------------------------------------------------
// POST /silos  |  GET /silos
// ---------------------------------------------------------------------------

func (s *Server) handleSilos(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	setCORSHeaders(w)
	switch r.Method {
	case http.MethodPost:
		s.handleSiloCreate(w, r)
	case http.MethodGet:
		s.handleSiloList(w, r)
	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSiloCreate(w http.ResponseWriter, r *http.Request) {
	sc := s.requireSiloCrypto(w)
	if sc == nil {
		return
	}
	tenantID, keyID, role, ok := s.authTenantKey(w, r)
	if !ok {
		return
	}
	if role != "tenant" && role != "admin" {
		jsonError(w, "forbidden: silo management requires a tenant or admin key", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ShardMode   string `json:"shard_mode"`
	}
	if !decodeJSONBody(w, r, &req, 0) {
		return
	}
	if req.Name == "" || !validID.MatchString(req.Name) || strings.HasPrefix(req.Name, "_") {
		jsonError(w, "invalid silo name (alphanumeric, dash, dot, underscore; leading underscore reserved)", http.StatusBadRequest)
		return
	}
	if req.ShardMode == "" {
		req.ShardMode = "dedicated"
	}
	if req.ShardMode != "dedicated" && req.ShardMode != "shared" {
		jsonError(w, "shard_mode must be 'dedicated' or 'shared'", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	silo, err := s.createSilo(ctx, tenantID, req.Name, req.Description, req.ShardMode, "isolated")
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			jsonError(w, "silo name already exists", http.StatusConflict)
			return
		}
		log.Printf("silo create error tenant=%s: %v", tenantID, err)
		jsonError(w, "silo create failed", http.StatusInternalServerError)
		return
	}

	// Ensure the tenant's quarantine silo exists the moment any silo exists,
	// so fail-safe routing always has a holding pen.
	if _, err := s.createSilo(ctx, tenantID, quarantineSilo, "fail-safe holding pen", "shared", "quarantine"); err != nil &&
		!strings.Contains(err.Error(), "UNIQUE") {
		log.Printf("quarantine silo create error tenant=%s: %v", tenantID, err)
	}

	// Mint IAC v1 for the new silo (empty decrypt set until grants are added).
	if cert, err := s.mintIAC(ctx, tenantID, silo.ID, ""); err != nil {
		log.Printf("IAC mint error silo=%s: %v", silo.ID, err)
	} else {
		log.Printf("IAC v%d minted for silo=%s", cert.Version, silo.ID)
	}

	s.auditSilo(ctx, tenantID, keyID, "silo_create", silo.ID, silo.Name)
	respondJSON(w, silo)
}

// createSilo inserts the silo row and its wrapped DEK in one transaction.
func (s *Server) createSilo(ctx context.Context, tenantID, name, description, shardMode, isolation string) (*siloRow, error) {
	id := uuid.New().String()
	wrapped, fp, err := s.crypto.newDEK(id)
	if err != nil {
		return nil, fmt.Errorf("generate DEK: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO silos (id, tenant_id, name, description, isolation, crypto, shard_mode, status)
		 VALUES (?, ?, ?, ?, ?, 'firm', ?, 'active')`,
		id, tenantID, name, nullableString(description), isolation, shardMode); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO silo_keys (silo_id, wrapped_dek, kek_id, key_version, dek_fingerprint)
		 VALUES (?, ?, ?, 1, ?)`,
		id, wrapped, s.crypto.kekID, fp); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &siloRow{
		ID: id, TenantID: tenantID, Name: name, Description: description,
		Isolation: isolation, Crypto: "firm", ShardMode: shardMode, Status: "active",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// handleSiloList is server-side grant-filtered for consultant keys: a
// consultant only ever sees silos its key holds an unrevoked grant on —
// ungranted silos are non-enumerable (PRD hard invariant iv).
func (s *Server) handleSiloList(w http.ResponseWriter, r *http.Request) {
	if s.crypto == nil {
		respondJSON(w, map[string]interface{}{"silos": []siloRow{}, "count": 0})
		return
	}
	tenantID, keyID, role, ok := s.authTenantKey(w, r)
	if !ok {
		return
	}

	ctx := r.Context()
	var rows *sql.Rows
	var err error
	if role == "tenant" || role == "admin" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, tenant_id, name, COALESCE(description,''), isolation, crypto, shard_mode, status, created_at
			   FROM silos WHERE tenant_id = ? AND status = 'active' ORDER BY name`, tenantID)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT DISTINCT s.id, s.tenant_id, s.name, COALESCE(s.description,''), s.isolation, s.crypto, s.shard_mode, s.status, s.created_at
			   FROM silos s JOIN silo_grants g ON g.silo_id = s.id
			  WHERE s.tenant_id = ? AND s.status = 'active' AND g.key_id = ? AND g.revoked_at IS NULL
			  ORDER BY s.name`, tenantID, keyID)
	}
	if err != nil {
		log.Printf("silo list error tenant=%s: %v", tenantID, err)
		jsonError(w, "silo list failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	silos := []siloRow{}
	for rows.Next() {
		var sr siloRow
		if err := rows.Scan(&sr.ID, &sr.TenantID, &sr.Name, &sr.Description, &sr.Isolation, &sr.Crypto, &sr.ShardMode, &sr.Status, &sr.CreatedAt); err == nil {
			silos = append(silos, sr)
		}
	}
	respondJSON(w, map[string]interface{}{"silos": silos, "count": len(silos)})
}

// ---------------------------------------------------------------------------
// /silos/{id}            DELETE = crypto-shred
// /silos/{id}/grants     POST = grant, body {key_id, perm} | DELETE = revoke
// /silos/{id}/certificates  GET = IAC version chain (offline-verifiable)
// ---------------------------------------------------------------------------

func (s *Server) handleSiloByID(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	setCORSHeaders(w)
	sc := s.requireSiloCrypto(w)
	if sc == nil {
		return
	}
	tenantID, keyID, role, ok := s.authTenantKey(w, r)
	if !ok {
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/silos/"), "/")
	siloID := parts[0]
	if siloID == "" {
		jsonError(w, "missing silo id", http.StatusBadRequest)
		return
	}

	// Verify the silo belongs to this tenant before any action.
	var siloName, siloStatus string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT name, status FROM silos WHERE id = ? AND tenant_id = ?`, siloID, tenantID).
		Scan(&siloName, &siloStatus)
	if err == sql.ErrNoRows {
		jsonError(w, "silo not found", http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, "silo lookup failed", http.StatusInternalServerError)
		return
	}

	// Non-enumerability: a consultant key with no grant on this silo gets the
	// same 404 as a missing silo — never a 403 that confirms existence.
	if role != "tenant" && role != "admin" {
		var n int
		if err := s.db.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM silo_grants WHERE key_id = ? AND silo_id = ? AND revoked_at IS NULL`,
			keyID, siloID).Scan(&n); err != nil {
			jsonError(w, "authorization check failed", http.StatusInternalServerError)
			return
		}
		if n == 0 {
			jsonError(w, "silo not found", http.StatusNotFound)
			return
		}
	}

	switch {
	case len(parts) == 1 && r.Method == http.MethodDelete:
		s.handleSiloShred(w, r, tenantID, keyID, role, siloID, siloName, siloStatus)
	case len(parts) == 2 && parts[1] == "grants":
		s.handleSiloGrants(w, r, tenantID, keyID, role, siloID)
	case len(parts) == 2 && parts[1] == "certificates" && r.Method == http.MethodGet:
		s.handleSiloCertificates(w, r, tenantID, keyID, role, siloID)
	default:
		jsonError(w, "invalid path or method", http.StatusBadRequest)
	}
}

// handleSiloShred performs the O(1) crypto-shred: delete the wrapped DEK row,
// evict the cached DEK, mint a Destruction Certificate whose fingerprint
// equals the stored (creation-time) dek_fingerprint, and lazily GC content.
func (s *Server) handleSiloShred(w http.ResponseWriter, r *http.Request, tenantID string, keyID int64, role, siloID, siloName, siloStatus string) {
	if role != "tenant" && role != "admin" {
		jsonError(w, "forbidden: crypto-shred requires a tenant or admin key", http.StatusForbidden)
		return
	}
	if siloStatus == "shredded" {
		jsonError(w, "silo already shredded", http.StatusConflict)
		return
	}

	ctx := r.Context()

	// Refuse to shred under an active or paused engagement — close it first.
	// Fail closed: an error here must block the shred, not wave it through.
	var liveEng int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM engagements WHERE silo_id = ? AND status IN ('active','paused')`, siloID).
		Scan(&liveEng); err != nil {
		jsonError(w, "engagement check failed", http.StatusInternalServerError)
		return
	}
	if liveEng > 0 {
		jsonError(w, "silo has active or paused engagements; close them before shredding", http.StatusConflict)
		return
	}

	var fp string
	if err := s.db.QueryRowContext(ctx,
		`SELECT dek_fingerprint FROM silo_keys WHERE silo_id = ?`, siloID).Scan(&fp); err != nil {
		jsonError(w, "silo key not found (already shredded?)", http.StatusConflict)
		return
	}

	if err := s.shredSilo(ctx, tenantID, siloID, fp); err != nil {
		log.Printf("shred error silo=%s: %v", siloID, err)
		jsonError(w, "shred failed", http.StatusInternalServerError)
		return
	}

	cert, err := s.mintDestructionCert(ctx, tenantID, siloID, fp)
	if err != nil {
		log.Printf("destruction cert mint error silo=%s: %v", siloID, err)
	}

	s.auditSilo(ctx, tenantID, keyID, "silo_shred", siloID, siloName)
	resp := map[string]interface{}{
		"shredded":              true,
		"silo_id":               siloID,
		"destroyed_fingerprint": fp,
	}
	if cert != nil {
		resp["destruction_certificate"] = cert
	}
	respondJSON(w, resp)
}

func gcSiloKeyspace(ctx context.Context, rdb *redis.Client, tenantID, siloName string) {
	pattern := fmt.Sprintf("tenant:%s:silo:%s:*", tenantID, siloName)
	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, pattern, 200).Result()
		if err != nil {
			log.Printf("silo GC scan error %s: %v", pattern, err)
			return
		}
		if len(keys) > 0 {
			if err := rdb.Del(ctx, keys...).Err(); err != nil {
				log.Printf("silo GC del error %s: %v", pattern, err)
			}
		}
		cursor = next
		if cursor == 0 {
			return
		}
	}
}

func (s *Server) handleSiloGrants(w http.ResponseWriter, r *http.Request, tenantID string, keyID int64, role, siloID string) {
	if role != "tenant" && role != "admin" {
		jsonError(w, "forbidden: grant management requires a tenant or admin key", http.StatusForbidden)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		KeyID int64  `json:"key_id"`
		Perm  string `json:"perm"`
	}
	if !decodeJSONBody(w, r, &req, 0) {
		return
	}
	if req.Perm != "read" && req.Perm != "write" && req.Perm != "admin" {
		jsonError(w, "perm must be read, write, or admin", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	// The granted key must belong to the same tenant (cross-tenant grants are
	// structurally impossible).
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM api_keys WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL`,
		req.KeyID, tenantID).Scan(&n); err != nil || n == 0 {
		jsonError(w, "key not found in tenant", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodPost:
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO silo_grants (key_id, silo_id, perm) VALUES (?, ?, ?)
			 ON CONFLICT(key_id, silo_id, perm) DO UPDATE SET revoked_at = NULL`,
			req.KeyID, siloID, req.Perm); err != nil {
			log.Printf("grant error silo=%s key=%d: %v", siloID, req.KeyID, err)
			jsonError(w, "grant failed", http.StatusInternalServerError)
			return
		}
		s.auditSilo(ctx, tenantID, keyID, "silo_grant", siloID, fmt.Sprintf("key=%d perm=%s", req.KeyID, req.Perm))
	case http.MethodDelete:
		if _, err := s.db.ExecContext(ctx,
			`UPDATE silo_grants SET revoked_at = CURRENT_TIMESTAMP WHERE key_id = ? AND silo_id = ? AND perm = ?`,
			req.KeyID, siloID, req.Perm); err != nil {
			jsonError(w, "revoke failed", http.StatusInternalServerError)
			return
		}
		s.auditSilo(ctx, tenantID, keyID, "silo_grant_revoke", siloID, fmt.Sprintf("key=%d perm=%s", req.KeyID, req.Perm))
	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Any decrypt-set change mints a new IAC version: access expansion (or
	// contraction) is always a signed, customer-visible event.
	if cert, err := s.mintIAC(ctx, tenantID, siloID, ""); err != nil {
		log.Printf("IAC re-mint error silo=%s: %v", siloID, err)
	} else {
		respondJSON(w, map[string]interface{}{"ok": true, "certificate_version": cert.Version})
		return
	}
	respondJSON(w, map[string]interface{}{"ok": true})
}

// auditSilo writes a silo-tagged audit_log row (best-effort).
func (s *Server) auditSilo(ctx context.Context, tenantID string, keyID int64, action, siloID, details string) {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (tenant_id, actor, action, resource, success, details, silo_id)
		 VALUES (?, ?, ?, ?, 1, ?, ?)`,
		tenantID, fmt.Sprintf("%d", keyID), action, siloID, nullableString(details), siloID); err != nil {
		log.Printf("audit_log write failed tenant=%s action=%s: %v", tenantID, action, err)
	}
}

// ---------------------------------------------------------------------------
// Encrypted per-silo vector storage
// ---------------------------------------------------------------------------

// encPayload is the plaintext structure sealed under the silo DEK.
type encPayload struct {
	Text     string          `json:"text,omitempty"`
	Metadata json.RawMessage `json:"metadata"`
}

// storeSiloVec writes an encrypted memory into a silo's dedicated keyspace.
// Embeddings stay plaintext floats (derived data, required for similarity);
// text + metadata are AES-256-GCM ciphertext under the silo DEK.
func storeSiloVec(ctx context.Context, rdb *redis.Client, dek []byte, tenantID, siloName string, sv storedVector, text string) error {
	embJSON, err := json.Marshal(sv.Embedding)
	if err != nil {
		return fmt.Errorf("marshal embedding: %w", err)
	}
	payload, err := json.Marshal(encPayload{Text: text, Metadata: sv.Metadata})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	blob, err := sealContent(dek, sv.ID, payload)
	if err != nil {
		return fmt.Errorf("seal payload: %w", err)
	}
	if sv.CreatedAt == "" {
		sv.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	fields := map[string]interface{}{
		"embedding":   string(embJSON),
		"enc_payload": blob,
		"key_version": 1,
		"created_at":  sv.CreatedAt,
		"silo":        siloName,
	}
	if sv.AgentID != "" {
		fields["agent_id"] = sv.AgentID
	}
	if sv.Model != "" {
		fields["embedding_model"] = sv.Model
	}

	pipe := rdb.Pipeline()
	pipe.HSet(ctx, siloVecKey(tenantID, siloName, sv.ID), fields)
	pipe.SAdd(ctx, siloVecSetKey(tenantID, siloName), sv.ID)
	_, err = pipe.Exec(ctx)
	return err
}

// searchSiloVecs runs cosine search over one silo's keyspace and decrypts the
// metadata of the hits. The caller must have verified a read grant first.
func searchSiloVecs(ctx context.Context, rdb *redis.Client, dek []byte, tenantID, siloName, agentID string, query []float32, topK int, threshold float32) ([]searchResult, error) {
	ids, err := rdb.SMembers(ctx, siloVecSetKey(tenantID, siloName)).Result()
	if err != nil {
		return nil, fmt.Errorf("list silo vectors: %w", err)
	}
	if len(ids) == 0 {
		return []searchResult{}, nil
	}

	pipe := rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, siloVecKey(tenantID, siloName, id))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("pipeline fetch: %w", err)
	}

	results := make([]searchResult, 0, len(ids))
	for i, id := range ids {
		data, err := cmds[i].Result()
		if err != nil || len(data) == 0 {
			continue
		}
		if agentID != "" && data["agent_id"] != agentID {
			continue
		}
		var embedding []float32
		if err := json.Unmarshal([]byte(data["embedding"]), &embedding); err != nil {
			continue
		}
		score := cosine(query, embedding)
		if score < threshold {
			continue
		}

		meta := json.RawMessage(`{}`)
		if blob := data["enc_payload"]; blob != "" {
			if plain, err := openContent(dek, id, blob); err == nil {
				var p encPayload
				if json.Unmarshal(plain, &p) == nil && len(p.Metadata) > 0 && json.Valid(p.Metadata) {
					meta = p.Metadata
				}
			} else {
				log.Printf("silo decrypt failed tenant=%s silo=%s doc=%s: %v", tenantID, siloName, id, err)
				continue
			}
		}

		results = append(results, searchResult{
			ID: id, Score: score, AgentID: data["agent_id"], Metadata: meta, CreatedAt: data["created_at"],
		})
	}

	sortSearchResults(results)
	if topK > 0 && len(results) > topK {
		results = results[:topK]
	}
	return results, nil
}

func sortSearchResults(results []searchResult) {
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].Score > results[j-1].Score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
}
