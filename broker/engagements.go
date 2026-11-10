package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Engagements — the contract/billing boundary layered on top of the silo
// (crypto) boundary. An engagement ties {customer, contract_ref, silo, time
// window, staffed consultant keys}. It is a reporting/billing key, not a
// security key: cross-contamination protection lives in silo_grants + the
// DEK (see enforce.go).
// ---------------------------------------------------------------------------

type engagementRow struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id"`
	CustomerID  string `json:"customer_id"`
	ContractRef string `json:"contract_ref,omitempty"`
	SiloID      string `json:"silo_id"`
	Status      string `json:"status"`
	StartsAt    string `json:"starts_at,omitempty"`
	EndsAt      string `json:"ends_at,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// legal lifecycle transitions: draft→active, active⇄paused, active→closed,
// paused→closed, closed→shred. Everything else (including resurrecting a
// shredded engagement) is rejected.
var legalTransitions = map[string][]string{
	"draft":  {"active"},
	"active": {"paused", "closed"},
	"paused": {"active", "closed"},
	"closed": {"shred"},
	"shred":  {},
}

func transitionAllowed(from, to string) bool {
	for _, t := range legalTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

func engagementSessSetKey(tenantID, engagementID string) string {
	return fmt.Sprintf("tenant:%s:eng:%s:sessions", tenantID, engagementID)
}

// ---------------------------------------------------------------------------
// POST /engagements | GET /engagements
// ---------------------------------------------------------------------------

func (s *Server) handleEngagements(w http.ResponseWriter, r *http.Request) {
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

	switch r.Method {
	case http.MethodPost:
		s.handleEngagementCreate(w, r, tenantID, keyID, role)
	case http.MethodGet:
		s.handleEngagementList(w, r, tenantID, keyID, role)
	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleEngagementCreate(w http.ResponseWriter, r *http.Request, tenantID string, keyID int64, role string) {
	if role != "tenant" && role != "admin" {
		jsonError(w, "forbidden: engagement management requires a tenant or admin key", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		CustomerID  string `json:"customer_id"`
		ContractRef string `json:"contract_ref"`
		SiloID      string `json:"silo_id"`
		StartsAt    string `json:"starts_at"` // RFC3339, optional
		EndsAt      string `json:"ends_at"`   // RFC3339, optional
	}
	if !decodeJSONBody(w, r, &req, 0) {
		return
	}
	if req.CustomerID == "" || req.SiloID == "" {
		jsonError(w, "customer_id and silo_id are required", http.StatusBadRequest)
		return
	}
	for _, ts := range []string{req.StartsAt, req.EndsAt} {
		if ts != "" {
			if _, err := time.Parse(time.RFC3339, ts); err != nil {
				jsonError(w, "starts_at/ends_at must be RFC3339", http.StatusBadRequest)
				return
			}
		}
	}

	ctx := r.Context()
	var shardMode, siloStatus string
	err := s.db.QueryRowContext(ctx,
		`SELECT shard_mode, status FROM silos WHERE id = ? AND tenant_id = ?`, req.SiloID, tenantID).
		Scan(&shardMode, &siloStatus)
	if err == sql.ErrNoRows {
		jsonError(w, "silo not found", http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, "silo lookup failed", http.StatusInternalServerError)
		return
	}
	if siloStatus != "active" {
		jsonError(w, "silo is not active", http.StatusConflict)
		return
	}

	// A dedicated silo binds at most one non-terminal engagement: this is the
	// rule that prevents contract bleed inside one DEK. Fail closed on error.
	if shardMode == "dedicated" {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM engagements WHERE silo_id = ? AND status IN ('draft','active','paused')`,
			req.SiloID).Scan(&n); err != nil {
			jsonError(w, "engagement check failed", http.StatusInternalServerError)
			return
		}
		if n > 0 {
			jsonError(w, "dedicated silo already has a live engagement (close it first or use a shared silo)", http.StatusConflict)
			return
		}
	}

	id := "ENG-" + uuid.New().String()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO engagements (id, tenant_id, customer_id, contract_ref, silo_id, status, starts_at, ends_at)
		 VALUES (?, ?, ?, ?, ?, 'draft', ?, ?)`,
		id, tenantID, req.CustomerID, nullableString(req.ContractRef), req.SiloID,
		nullableString(req.StartsAt), nullableString(req.EndsAt)); err != nil {
		log.Printf("engagement create error tenant=%s: %v", tenantID, err)
		jsonError(w, "engagement create failed", http.StatusInternalServerError)
		return
	}
	s.appendEngagementAudit(ctx, id, fmt.Sprintf("%d", keyID), "", "draft", "created")

	respondJSON(w, engagementRow{
		ID: id, TenantID: tenantID, CustomerID: req.CustomerID, ContractRef: req.ContractRef,
		SiloID: req.SiloID, Status: "draft", StartsAt: req.StartsAt, EndsAt: req.EndsAt,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

// handleEngagementList: tenant/admin see all; consultants see only
// engagements they are a live member of (non-enumerable otherwise).
func (s *Server) handleEngagementList(w http.ResponseWriter, r *http.Request, tenantID string, keyID int64, role string) {
	ctx := r.Context()
	var rows *sql.Rows
	var err error
	if role == "tenant" || role == "admin" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT id, tenant_id, customer_id, COALESCE(contract_ref,''), silo_id, status,
			        COALESCE(starts_at,''), COALESCE(ends_at,''), created_at
			   FROM engagements WHERE tenant_id = ? ORDER BY created_at DESC`, tenantID)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT e.id, e.tenant_id, e.customer_id, COALESCE(e.contract_ref,''), e.silo_id, e.status,
			        COALESCE(e.starts_at,''), COALESCE(e.ends_at,''), e.created_at
			   FROM engagements e JOIN engagement_consultants ec ON ec.engagement_id = e.id
			  WHERE e.tenant_id = ? AND ec.key_id = ? AND ec.removed_at IS NULL
			  ORDER BY e.created_at DESC`, tenantID, keyID)
	}
	if err != nil {
		jsonError(w, "engagement list failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []engagementRow{}
	for rows.Next() {
		var e engagementRow
		if rows.Scan(&e.ID, &e.TenantID, &e.CustomerID, &e.ContractRef, &e.SiloID, &e.Status, &e.StartsAt, &e.EndsAt, &e.CreatedAt) == nil {
			out = append(out, e)
		}
	}
	respondJSON(w, map[string]interface{}{"engagements": out, "count": len(out)})
}

// ---------------------------------------------------------------------------
// /engagements/{id}/status   PATCH — lifecycle transitions (broker-enforced)
// /engagements/{id}/consultants  POST/DELETE — membership
// ---------------------------------------------------------------------------

func (s *Server) handleEngagementByID(w http.ResponseWriter, r *http.Request) {
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
	if role != "tenant" && role != "admin" {
		jsonError(w, "forbidden: engagement management requires a tenant or admin key", http.StatusForbidden)
		return
	}

	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/engagements/"), "/")
	engID := parts[0]
	if engID == "" {
		jsonError(w, "missing engagement id", http.StatusBadRequest)
		return
	}

	var status, siloID string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT status, silo_id FROM engagements WHERE id = ? AND tenant_id = ?`, engID, tenantID).
		Scan(&status, &siloID)
	if err == sql.ErrNoRows {
		jsonError(w, "engagement not found", http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, "engagement lookup failed", http.StatusInternalServerError)
		return
	}

	switch {
	case len(parts) == 2 && parts[1] == "status" && r.Method == http.MethodPatch:
		s.handleEngagementStatus(w, r, tenantID, keyID, engID, status, siloID)
	case len(parts) == 2 && parts[1] == "consultants":
		s.handleEngagementConsultants(w, r, tenantID, keyID, engID)
	default:
		jsonError(w, "invalid path or method", http.StatusBadRequest)
	}
}

func (s *Server) handleEngagementStatus(w http.ResponseWriter, r *http.Request, tenantID string, keyID int64, engID, fromStatus, siloID string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if !decodeJSONBody(w, r, &req, 0) {
		return
	}
	if !transitionAllowed(fromStatus, req.Status) {
		jsonError(w, fmt.Sprintf("illegal transition %s -> %s", fromStatus, req.Status), http.StatusConflict)
		return
	}

	ctx := r.Context()
	resp := map[string]interface{}{"id": engID, "status": req.Status}

	// Shred is broker-enforced, not a status flip: ALL destruction work runs
	// and succeeds BEFORE the engagement is moved into the terminal 'shred'
	// state. Any destruction failure returns 500 with the engagement still
	// 'closed' (retryable). v1 supports shred only for dedicated silos —
	// the PRD's shared-silo lazy-rekey design does not actually achieve
	// erasure, so we refuse rather than fake it.
	if req.Status == "shred" {
		var shardMode, siloStatus string
		if err := s.db.QueryRowContext(ctx,
			`SELECT shard_mode, status FROM silos WHERE id = ?`, siloID).Scan(&shardMode, &siloStatus); err != nil {
			jsonError(w, "silo lookup failed", http.StatusInternalServerError)
			return
		}
		if shardMode != "dedicated" {
			jsonError(w, "shred is only supported for dedicated silos in this release", http.StatusNotImplemented)
			return
		}

		var fp string
		err := s.db.QueryRowContext(ctx,
			`SELECT dek_fingerprint FROM silo_keys WHERE silo_id = ?`, siloID).Scan(&fp)
		switch {
		case err == sql.ErrNoRows && siloStatus == "shredded":
			// Silo already shredded via DELETE /silos/{id} — the status flip
			// below is the only remaining work.
		case err != nil:
			// FAIL CLOSED: never report 'shred' while the DEK may survive.
			log.Printf("engagement shred: fingerprint lookup failed eng=%s silo=%s: %v", engID, siloID, err)
			jsonError(w, "shred failed (key lookup); engagement unchanged, retry", http.StatusInternalServerError)
			return
		default:
			if err := s.shredSilo(ctx, tenantID, siloID, fp); err != nil {
				log.Printf("engagement shred: silo shred error eng=%s silo=%s: %v", engID, siloID, err)
				jsonError(w, "silo shred failed; engagement unchanged, retry", http.StatusInternalServerError)
				return
			}
			if cert, err := s.mintDestructionCert(ctx, tenantID, siloID, fp); err != nil {
				log.Printf("destruction cert mint error eng=%s silo=%s: %v", engID, siloID, err)
			} else {
				resp["destruction_certificate"] = cert
			}
			resp["destroyed_fingerprint"] = fp
		}
	}

	// Compare-and-swap on the snapshot status: a concurrent transition loses
	// with a 409 instead of silently producing an illegal transition or an
	// audit row whose from_status lies.
	res, err := s.db.ExecContext(ctx,
		`UPDATE engagements SET status = ? WHERE id = ? AND status = ?`,
		req.Status, engID, fromStatus)
	if err != nil {
		jsonError(w, "status update failed", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		jsonError(w, "engagement status changed concurrently; re-read and retry", http.StatusConflict)
		return
	}
	s.appendEngagementAudit(ctx, engID, fmt.Sprintf("%d", keyID), fromStatus, req.Status, req.Reason)

	// Pausing/closing/shredding kills live sessions bound to this engagement:
	// in-flight agents 403 on their next write (enforce.go gate 3).
	if req.Status == "paused" || req.Status == "closed" || req.Status == "shred" {
		s.invalidateEngagementSessions(ctx, tenantID, engID)
	}

	respondJSON(w, resp)
}

// shredSilo is the shared crypto-shred core used by both DELETE /silos/{id}
// and engagement →shred: delete the wrapped DEK, tombstone the shred (so a
// backup restore can replay the deletion), checkpoint the WAL so the deleted
// row does not linger in the journal, evict the DEK cache, and GC ciphertext.
func (s *Server) shredSilo(ctx context.Context, tenantID, siloID, fingerprint string) error {
	var siloName string
	if err := s.db.QueryRowContext(ctx,
		`SELECT name FROM silos WHERE id = ?`, siloID).Scan(&siloName); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM silo_keys WHERE silo_id = ?`, siloID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE silos SET status = 'shredded', shredded_at = CURRENT_TIMESTAMP WHERE id = ?`, siloID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO silo_shred_tombstones (silo_id, tenant_id, dek_fingerprint) VALUES (?, ?, ?)`,
		siloID, tenantID, fingerprint); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// Push the deleted row out of the WAL so the wrapped DEK does not survive
	// in journal frames (residue still exists in retained backups — disclosed
	// in the destruction certificate).
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Printf("wal checkpoint after shred failed: %v", err)
	}

	// Sidecar tombstone file: restore-heurchain.sh replays this after any
	// backup restore to re-delete resurrected silo_keys rows. The SQLite
	// tombstone row alone is useless there — restored snapshots predate it.
	s.appendShredTombstoneFile(tenantID, siloID, fingerprint)

	s.crypto.evictDEK(siloID)

	go func() {
		gcCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		gcSiloKeyspace(gcCtx, s.rdb, tenantID, siloName)
	}()
	return nil
}

// appendShredTombstoneFile appends one JSON line per shred to
// <sqlite-dir>/shred-tombstones.jsonl. Best-effort: failure is logged, never
// blocks the shred (the in-DB tombstone row is the primary record).
func (s *Server) appendShredTombstoneFile(tenantID, siloID, fingerprint string) {
	dir := filepath.Dir(s.config.SQLite.Path)
	path := filepath.Join(dir, "shred-tombstones.jsonl")
	line, _ := json.Marshal(map[string]string{
		"silo_id": siloID, "tenant_id": tenantID, "dek_fingerprint": fingerprint,
		"shredded_at": time.Now().UTC().Format(time.RFC3339),
	})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("shred tombstone file open failed %s: %v", path, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		log.Printf("shred tombstone file write failed %s: %v", path, err)
	}
}

// invalidateEngagementSessions deletes the Redis session meta of every
// session bound to this engagement (tracked at bind time in a reverse index).
func (s *Server) invalidateEngagementSessions(ctx context.Context, tenantID, engID string) {
	setKey := engagementSessSetKey(tenantID, engID)
	sids, err := s.rdb.SMembers(ctx, setKey).Result()
	if err != nil {
		log.Printf("engagement session invalidation: smembers error eng=%s: %v", engID, err)
		return
	}
	if len(sids) == 0 {
		return
	}
	pipe := s.rdb.Pipeline()
	for _, sid := range sids {
		pipe.Del(ctx, sessMetaKey(tenantID, sid), sessCtxKey(tenantID, sid), sessMemoryCtxKey(tenantID, sid))
	}
	pipe.Del(ctx, setKey)
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("engagement session invalidation: pipeline error eng=%s: %v", engID, err)
	}
	log.Printf("invalidated %d sessions for engagement %s", len(sids), engID)
}

func (s *Server) handleEngagementConsultants(w http.ResponseWriter, r *http.Request, tenantID string, keyID int64, engID string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		KeyID     int64  `json:"key_id"`
		Principal string `json:"principal"`
		Role      string `json:"role"`
	}
	if !decodeJSONBody(w, r, &req, 0) {
		return
	}
	ctx := r.Context()

	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM api_keys WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL`,
		req.KeyID, tenantID).Scan(&n); err != nil || n == 0 {
		jsonError(w, "key not found in tenant", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodPost:
		if req.Role == "" {
			req.Role = "member"
		}
		if req.Role != "lead" && req.Role != "member" && req.Role != "reviewer" {
			jsonError(w, "role must be lead, member, or reviewer", http.StatusBadRequest)
			return
		}
		if req.Principal == "" {
			req.Principal = fmt.Sprintf("key-%d", req.KeyID)
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO engagement_consultants (engagement_id, key_id, principal, role)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(engagement_id, key_id) DO UPDATE SET removed_at = NULL, role = excluded.role`,
			engID, req.KeyID, req.Principal, req.Role); err != nil {
			log.Printf("consultant add error eng=%s: %v", engID, err)
			jsonError(w, "consultant add failed", http.StatusInternalServerError)
			return
		}
		s.appendEngagementAudit(ctx, engID, fmt.Sprintf("%d", keyID), "", "membership",
			fmt.Sprintf("consultant_added key=%d principal=%s role=%s", req.KeyID, req.Principal, req.Role))
	case http.MethodDelete:
		if _, err := s.db.ExecContext(ctx,
			`UPDATE engagement_consultants SET removed_at = CURRENT_TIMESTAMP
			  WHERE engagement_id = ? AND key_id = ?`, engID, req.KeyID); err != nil {
			jsonError(w, "consultant remove failed", http.StatusInternalServerError)
			return
		}
		s.appendEngagementAudit(ctx, engID, fmt.Sprintf("%d", keyID), "", "membership",
			fmt.Sprintf("consultant_removed key=%d", req.KeyID))
	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Membership changes the staffing surface; re-mint the silo IAC so the
	// change is a signed, customer-visible event.
	var siloID string
	if err := s.db.QueryRowContext(ctx, `SELECT silo_id FROM engagements WHERE id = ?`, engID).Scan(&siloID); err == nil {
		if _, err := s.mintIAC(ctx, tenantID, siloID, engID); err != nil {
			log.Printf("IAC re-mint after membership change eng=%s: %v", engID, err)
		}
	}
	respondJSON(w, map[string]interface{}{"ok": true})
}

// appendEngagementAudit writes a checksum-chained lifecycle row. The chain is
// per engagement: checksum = SHA256(prev_checksum || row payload).
func (s *Server) appendEngagementAudit(ctx context.Context, engID, actor, from, to, reason string) {
	var prev sql.NullString
	_ = s.db.QueryRowContext(ctx,
		`SELECT checksum FROM engagement_audit WHERE engagement_id = ? ORDER BY id DESC LIMIT 1`, engID).
		Scan(&prev)

	rowPayload, _ := json.Marshal(map[string]string{
		"engagement_id": engID, "actor": actor, "from": from, "to": to, "reason": reason,
		"ts": time.Now().UTC().Format(time.RFC3339Nano),
	})
	checksum := chainChecksum(prev.String, string(rowPayload))

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO engagement_audit (engagement_id, actor, from_status, to_status, reason, prev_checksum, checksum, payload_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		engID, actor, nullableString(from), to, nullableString(reason),
		nullableString(prev.String), checksum, string(rowPayload)); err != nil {
		log.Printf("engagement_audit write failed eng=%s: %v", engID, err)
	}
}
