package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Four-gate write enforcement for silo'd writes.
//
// Every silo write must pass, independently:
//
//   gate 1: silo ∈ key.grants(write)         — the key ACL (the hard boundary)
//   gate 2: key ∈ engagement.members         — staffing (when engagement-bound)
//   gate 3: engagement active ∧ now ∈ window — contract validity
//   gate 4: engagement.silo_id == resolved   — engagement↔silo consistency
//
// Gates 2–4 apply only when the write is engagement-bound (the billable
// path). A direct silo write (explicit silo, no engagement) passes gate 1
// only and is non-billable by construction. Honest scoping note: all four
// gates execute in one broker process — they are defense-in-depth within a
// single trust domain, not independent systems.
//
// Reads fail closed to deny; ambiguous writes are routed to _quarantine by
// the caller (writes fail closed to quarantine, never to a client silo).
// ---------------------------------------------------------------------------

// enforcement failure reason codes (stable, machine-readable, returned in 403s)
const (
	denyNoWriteGrant       = "silo_write_grant_missing"      // gate 1
	denyNotEngagementMember = "not_engagement_member"        // gate 2
	denyEngagementInactive = "engagement_not_active"         // gate 3
	denyEngagementWindow   = "engagement_outside_window"     // gate 3
	denyEngagementSiloMismatch = "engagement_silo_mismatch"  // gate 4
	denySiloNotFound       = "silo_not_found"
	denySiloShredded       = "silo_shredded"
	denySessionKeyMismatch = "session_key_mismatch"
	denySessionExpired     = "session_expired"
)

type enforceError struct {
	Code string
}

func (e *enforceError) Error() string { return e.Code }

// writeContext is the fully-resolved, fully-enforced target of a silo write.
type writeContext struct {
	SiloID       string
	SiloName     string
	EngagementID string // empty for direct (non-billable) silo writes
	DEK          []byte
}

// resolveWriteContext resolves where a write lands and enforces the gates.
//
// Resolution order (Tier 0 of the routing ladder):
//   1. session binding — the session's engagement seeds silo + engagement
//   2. explicit silo name on the request (direct, non-billable)
//
// A request with neither is not a silo write (legacy tenant path; caller
// handles it).
func (s *Server) resolveWriteContext(ctx context.Context, tenantID string, keyID int64, sessionID, explicitSilo string) (*writeContext, error) {
	var siloName, engagementID string

	if sessionID != "" {
		meta, err := s.rdb.HGetAll(ctx, sessMetaKey(tenantID, sessionID)).Result()
		if err != nil || len(meta) == 0 {
			return nil, &enforceError{denySessionExpired}
		}
		// Session-key binding: a session is only usable by the key that
		// created it (prevents session fixation/borrowing within a tenant).
		if meta["key_id"] != "" && meta["key_id"] != fmt.Sprintf("%d", keyID) {
			return nil, &enforceError{denySessionKeyMismatch}
		}
		siloName = meta["silo"]
		engagementID = meta["engagement_id"]
	}
	if siloName == "" {
		siloName = explicitSilo
	}
	if siloName == "" {
		return nil, nil // not a silo write
	}

	wc, err := s.enforceGates(ctx, tenantID, keyID, siloName, engagementID)
	if err != nil {
		return nil, err
	}
	return wc, nil
}

// enforceGates runs gates 1–4 against live table state and unwraps the DEK.
func (s *Server) enforceGates(ctx context.Context, tenantID string, keyID int64, siloName, engagementID string) (*writeContext, error) {
	var siloID, siloStatus string
	var wrapped []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT s.id, s.status, k.wrapped_dek
		   FROM silos s LEFT JOIN silo_keys k ON k.silo_id = s.id
		  WHERE s.tenant_id = ? AND s.name = ?`, tenantID, siloName).
		Scan(&siloID, &siloStatus, &wrapped)
	if err == sql.ErrNoRows {
		return nil, &enforceError{denySiloNotFound}
	}
	if err != nil {
		return nil, fmt.Errorf("silo lookup: %w", err)
	}

	// Gate 1 — the key ACL. Re-checked on EVERY write even when the session
	// pre-resolved the silo (PRD hard invariant iii). The grant check runs
	// BEFORE any status/grant-level detail is revealed: a key holding no
	// grant at all on this silo gets the same code as "no such silo", so
	// silo names are non-enumerable on the write plane too (a consultant
	// probing client names must not learn the firm's client roster from
	// distinguishable 403 reason codes).
	var anyGrant, writeGrant int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN g.perm IN ('write','admin') THEN 1 ELSE 0 END), 0)
		   FROM silo_grants g JOIN api_keys k ON k.id = g.key_id
		  WHERE g.key_id = ? AND g.silo_id = ?
		    AND g.revoked_at IS NULL AND k.revoked_at IS NULL`,
		keyID, siloID).Scan(&anyGrant, &writeGrant); err != nil {
		return nil, fmt.Errorf("grant check: %w", err)
	}
	if anyGrant == 0 {
		return nil, &enforceError{denySiloNotFound}
	}
	if siloStatus != "active" || len(wrapped) == 0 {
		return nil, &enforceError{denySiloShredded}
	}
	if writeGrant == 0 {
		return nil, &enforceError{denyNoWriteGrant}
	}

	// Gates 2–4 — only on the engagement-bound (billable) path.
	if engagementID != "" {
		var engStatus, engSiloID string
		var startsAt, endsAt sql.NullString
		err := s.db.QueryRowContext(ctx,
			`SELECT status, silo_id, starts_at, ends_at FROM engagements
			  WHERE id = ? AND tenant_id = ?`, engagementID, tenantID).
			Scan(&engStatus, &engSiloID, &startsAt, &endsAt)
		if err == sql.ErrNoRows {
			return nil, &enforceError{denyEngagementInactive}
		}
		if err != nil {
			return nil, fmt.Errorf("engagement lookup: %w", err)
		}

		// Gate 2 — staffing membership.
		var members int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM engagement_consultants
			  WHERE engagement_id = ? AND key_id = ? AND removed_at IS NULL`,
			engagementID, keyID).Scan(&members); err != nil {
			return nil, fmt.Errorf("membership check: %w", err)
		}
		if members == 0 {
			return nil, &enforceError{denyNotEngagementMember}
		}

		// Gate 3 — lifecycle + time window.
		if engStatus != "active" {
			return nil, &enforceError{denyEngagementInactive}
		}
		now := time.Now().UTC()
		if startsAt.Valid && startsAt.String != "" {
			if t, err := time.Parse(time.RFC3339, startsAt.String); err == nil && now.Before(t) {
				return nil, &enforceError{denyEngagementWindow}
			}
		}
		if endsAt.Valid && endsAt.String != "" {
			if t, err := time.Parse(time.RFC3339, endsAt.String); err == nil && now.After(t) {
				return nil, &enforceError{denyEngagementWindow}
			}
		}

		// Gate 4 — the bound engagement's silo must be the resolved silo.
		// Even a wrongly-granted key cannot steer this session's writes into
		// another client's silo: the engagement pins the target.
		if engSiloID != siloID {
			return nil, &enforceError{denyEngagementSiloMismatch}
		}
	}

	dek, err := s.crypto.unwrapDEK(siloID, wrapped)
	if err != nil {
		return nil, fmt.Errorf("unwrap DEK: %w", err)
	}
	return &writeContext{SiloID: siloID, SiloName: siloName, EngagementID: engagementID, DEK: dek}, nil
}

// checkReadGrant enforces the read plane: reads fail closed to deny. Returns
// the silo id + DEK when the key holds a read/admin grant.
func (s *Server) checkReadGrant(ctx context.Context, tenantID string, keyID int64, siloName string) (*writeContext, error) {
	var siloID, siloStatus string
	var wrapped []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT s.id, s.status, k.wrapped_dek
		   FROM silos s LEFT JOIN silo_keys k ON k.silo_id = s.id
		  WHERE s.tenant_id = ? AND s.name = ?`, tenantID, siloName).
		Scan(&siloID, &siloStatus, &wrapped)
	if err == sql.ErrNoRows {
		return nil, &enforceError{denySiloNotFound}
	}
	if err != nil {
		return nil, fmt.Errorf("silo lookup: %w", err)
	}

	// Grant check BEFORE the shredded check: a key with no grant must not be
	// able to distinguish an existing/shredded silo from a missing one.
	var anyGrant, readGrant int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN g.perm IN ('read','admin') THEN 1 ELSE 0 END), 0)
		   FROM silo_grants g JOIN api_keys k ON k.id = g.key_id
		  WHERE g.key_id = ? AND g.silo_id = ?
		    AND g.revoked_at IS NULL AND k.revoked_at IS NULL`,
		keyID, siloID).Scan(&anyGrant, &readGrant); err != nil {
		return nil, fmt.Errorf("grant check: %w", err)
	}
	if anyGrant == 0 {
		// Deny indistinguishably from not-found: ungranted silos are
		// non-enumerable to consultants.
		return nil, &enforceError{denySiloNotFound}
	}
	if siloStatus != "active" || len(wrapped) == 0 {
		return nil, &enforceError{denySiloShredded}
	}
	if readGrant == 0 {
		// Write-only key reading: deny without inventing a new reason code
		// (the caller already knows the silo exists via its write grant).
		return nil, &enforceError{denySiloNotFound}
	}

	dek, err := s.crypto.unwrapDEK(siloID, wrapped)
	if err != nil {
		return nil, fmt.Errorf("unwrap DEK: %w", err)
	}
	return &writeContext{SiloID: siloID, SiloName: siloName, DEK: dek}, nil
}

// asEnforceError unwraps an enforcement denial; (nil, false) for system errors.
func asEnforceError(err error) (*enforceError, bool) {
	var ee *enforceError
	if errors.As(err, &ee) {
		return ee, true
	}
	return nil, false
}
