package main

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// ---------------------------------------------------------------------------
// Isolation Attestation Certificates (IAC) + Destruction Certificates.
//
// Honesty rules (these are customer-facing legal-adjacent artifacts):
//
//  1. The signature is Ed25519 over the EXACT payload_json bytes stored in
//     SQLite. Verifiers verify over those literal bytes — never re-serialize.
//     This sidesteps every canonical-JSON interop pitfall.
//  2. The decrypt-principal list is named authorized_decrypt_principals, NOT
//     "who_can_decrypt": it enumerates API-plane principals only. The
//     platform_trust block discloses that the KEK and attestation key live in
//     the broker environment, and the operator class + backup chain can also
//     decrypt. We disclose the trust domain rather than overclaim.
//  3. The Destruction Certificate states the backup-retention caveat: the
//     wrapped DEK persists in encrypted off-site backups until they age out,
//     so destruction *completes* at completes_at, not at issuance.
// ---------------------------------------------------------------------------

const backupRetentionDays = 7 // mirrors scripts/backup-heurchain.sh RETENTION_DAYS

type attestor struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// loadAttestor reads the Ed25519 seed from HEURCHAIN_ATTEST_SEED (base64,
// 32 bytes). Returns nil when unset (attestation dark, silos still work).
func loadAttestor() (*attestor, error) {
	seedB64 := os.Getenv(envAttestSeed)
	if seedB64 == "" {
		return nil, nil
	}
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s must be base64 of exactly %d bytes", envAttestSeed, ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return &attestor{priv: priv, pub: priv.Public().(ed25519.PublicKey)}, nil
}

type platformTrust struct {
	KEKCustody           string `json:"kek_custody"`
	AttestationKeyCustody string `json:"attestation_key_custody"`
	BackupRetentionDays  int    `json:"backup_retention_days"`
	Note                 string `json:"note"`
}

func currentPlatformTrust() platformTrust {
	return platformTrust{
		KEKCustody:            "broker-environment",
		AttestationKeyCustody: "broker-environment",
		BackupRetentionDays:   backupRetentionDays,
		Note: "KEK and attestation key reside in the broker host environment; " +
			"host operators and holders of encrypted off-site backups plus the KEK " +
			"are outside the principal list below. BYOK custody removes firm decrypt capability.",
	}
}

// iacPayload is the signed body of an Isolation Attestation Certificate.
// Field order is the canonical serialization; the stored payload_json bytes
// are what the signature covers.
type iacPayload struct {
	Type                         string        `json:"type"` // "isolation_attestation"
	TenantID                     string        `json:"tenant_id"`
	SiloID                       string        `json:"silo_id"`
	SiloName                     string        `json:"silo_name"`
	EngagementID                 string        `json:"engagement_id,omitempty"`
	Version                      int           `json:"version"`
	DEKFingerprint               string        `json:"dek_fingerprint"`
	CustodyTier                  string        `json:"custody_tier"`
	AuthorizedDecryptPrincipals  []string      `json:"authorized_decrypt_principals"`
	PlatformTrust                platformTrust `json:"platform_trust"`
	IsolationInvariants          []string      `json:"isolation_invariants"`
	PrevCertChecksum             string        `json:"prev_cert_checksum,omitempty"`
	IssuedAt                     string        `json:"issued_at"`
}

type certRecord struct {
	Version     int    `json:"version"`
	PayloadJSON string `json:"payload_json"`
	Sig         string `json:"sig"`
	PubKey      string `json:"pubkey"`
	CreatedAt   string `json:"created_at"`
}

// mintIAC derives a new certificate version from live table state.
//
// Every read below — silo state, the principal set, the previous version — and
// the INSERT that persists the new version run inside one transaction, so the
// certificate cannot drift from enforcement state at mint time (PRD hard
// invariant ii). That claim used to be false: the four statements ran
// independently on s.db, so a grant revoked between the principal query and the
// INSERT produced a signed certificate naming a principal that no longer held
// access, and two concurrent mints could both derive prevVersion+1.
//
// The mutual exclusion comes from the pool as much as from the transaction:
// main.go sets SetMaxOpenConns(1), so a transaction holds the only connection
// and a second mint blocks until the first commits. If the pool is ever widened,
// this read-then-insert pair stops being atomic under SQLite's deferred
// transaction rules (the loser's upgrade fails with SQLITE_BUSY_SNAPSHOT) and
// this must become an immediate transaction — modernc takes that via the DSN
// parameter `_txlock=immediate`, there is no Go-side API for it.
func (s *Server) mintIAC(ctx context.Context, tenantID, siloID, engagementID string) (*certRecord, error) {
	if s.attest == nil {
		return nil, fmt.Errorf("attestation key not configured (%s unset)", envAttestSeed)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin certificate transaction: %w", err)
	}
	// Every early return below leaves the transaction open, so the rollback is
	// registered here rather than at each exit; after a successful Commit it is
	// a documented no-op (sql.ErrTxDone is discarded).
	defer func() { _ = tx.Rollback() }()

	var siloName, custody, fp string
	if err := tx.QueryRowContext(ctx,
		`SELECT s.name, s.crypto, k.dek_fingerprint
		   FROM silos s JOIN silo_keys k ON k.silo_id = s.id
		  WHERE s.id = ? AND s.tenant_id = ?`, siloID, tenantID).
		Scan(&siloName, &custody, &fp); err != nil {
		return nil, fmt.Errorf("silo state: %w", err)
	}

	// Derive the API-plane decrypt set: read/admin grants joined to live keys.
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT COALESCE(k.principal, 'key-' || k.id)
		   FROM silo_grants g JOIN api_keys k ON k.id = g.key_id
		  WHERE g.silo_id = ? AND g.perm IN ('read','admin')
		    AND g.revoked_at IS NULL AND k.revoked_at IS NULL
		  ORDER BY 1`, siloID)
	if err != nil {
		return nil, fmt.Errorf("derive principals: %w", err)
	}
	principals := []string{}
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			principals = append(principals, p)
		}
	}
	rows.Close()
	if custody == "firm" {
		principals = append(principals, "firm-admin (broker operator, firm custody)")
	}

	var prevVersion int
	var prevPayload sql.NullString
	_ = tx.QueryRowContext(ctx,
		`SELECT version, payload_json FROM isolation_certificates
		  WHERE silo_id = ? ORDER BY version DESC LIMIT 1`, siloID).
		Scan(&prevVersion, &prevPayload)
	prevChecksum := ""
	if prevPayload.Valid {
		prevChecksum = chainChecksum("", prevPayload.String)
	}

	payload := iacPayload{
		Type:           "isolation_attestation",
		TenantID:       tenantID,
		SiloID:         siloID,
		SiloName:       siloName,
		EngagementID:   engagementID,
		Version:        prevVersion + 1,
		DEKFingerprint: fp,
		CustodyTier:    custody,
		AuthorizedDecryptPrincipals: principals,
		PlatformTrust:               currentPlatformTrust(),
		IsolationInvariants: []string{
			"content-encrypted-aes256gcm-under-per-silo-dek",
			"dedicated-redis-keyspace-per-silo",
			"no-cross-silo-query-fusion",
			"silo-writes-require-explicit-key-grant",
		},
		PrevCertChecksum: prevChecksum,
		IssuedAt:         time.Now().UTC().Format(time.RFC3339),
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(s.attest.priv, payloadJSON)

	rec := &certRecord{
		Version:     payload.Version,
		PayloadJSON: string(payloadJSON),
		Sig:         base64.StdEncoding.EncodeToString(sig),
		PubKey:      base64.StdEncoding.EncodeToString(s.attest.pub),
		CreatedAt:   payload.IssuedAt,
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO isolation_certificates (tenant_id, silo_id, engagement_id, version, dek_fingerprint, payload_json, sig, pubkey)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		tenantID, siloID, nullableString(engagementID), rec.Version, fp, rec.PayloadJSON, rec.Sig, rec.PubKey); err != nil {
		return nil, fmt.Errorf("persist certificate: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit certificate: %w", err)
	}
	return rec, nil
}

// destructionPayload is the signed body of a Destruction Certificate.
type destructionPayload struct {
	Type                 string `json:"type"` // "destruction_certificate"
	TenantID             string `json:"tenant_id"`
	SiloID               string `json:"silo_id"`
	DestroyedFingerprint string `json:"destroyed_fingerprint"`
	Mechanism            string `json:"mechanism"`
	BackupCaveat         string `json:"backup_caveat"`
	IssuedAt             string `json:"issued_at"`
	CompletesAt          string `json:"completes_at"`
}

// mintDestructionCert signs the destruction receipt. destroyed_fingerprint is
// the creation-time DEK fingerprint, so equality with every IAC version's
// dek_fingerprint holds regardless of any KEK re-wraps in between.
func (s *Server) mintDestructionCert(ctx context.Context, tenantID, siloID, fingerprint string) (*certRecord, error) {
	if s.attest == nil {
		return nil, fmt.Errorf("attestation key not configured (%s unset)", envAttestSeed)
	}
	now := time.Now().UTC()
	payload := destructionPayload{
		Type:                 "destruction_certificate",
		TenantID:             tenantID,
		SiloID:               siloID,
		DestroyedFingerprint: fingerprint,
		Mechanism:            "wrapped-DEK row deleted, key cache evicted, WAL checkpointed, shred tombstone recorded for backup-restore replay",
		BackupCaveat: fmt.Sprintf(
			"encrypted off-site backups taken before this shred may contain the wrapped DEK for up to %d days; destruction completes when the last such backup expires",
			backupRetentionDays),
		IssuedAt:    now.Format(time.RFC3339),
		CompletesAt: now.AddDate(0, 0, backupRetentionDays).Format(time.RFC3339),
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(s.attest.priv, payloadJSON)
	rec := &certRecord{
		PayloadJSON: string(payloadJSON),
		Sig:         base64.StdEncoding.EncodeToString(sig),
		PubKey:      base64.StdEncoding.EncodeToString(s.attest.pub),
		CreatedAt:   payload.IssuedAt,
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO destruction_certificates (tenant_id, silo_id, destroyed_fingerprint, payload_json, sig, pubkey)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		tenantID, siloID, fingerprint, rec.PayloadJSON, rec.Sig, rec.PubKey); err != nil {
		return nil, fmt.Errorf("persist destruction certificate: %w", err)
	}
	return rec, nil
}

// handleSiloCertificates returns the full IAC version chain for a silo plus
// any destruction certificate. Consultants need a grant; tenant/admin pass.
// Verification instructions: verify sig (base64) with pubkey (base64 Ed25519)
// over the literal payload_json bytes.
func (s *Server) handleSiloCertificates(w http.ResponseWriter, r *http.Request, tenantID string, keyID int64, role, siloID string) {
	ctx := r.Context()
	if role != "tenant" && role != "admin" {
		var n int
		_ = s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM silo_grants WHERE key_id = ? AND silo_id = ? AND revoked_at IS NULL`,
			keyID, siloID).Scan(&n)
		if n == 0 {
			jsonError(w, "silo not found", http.StatusNotFound) // non-enumerable
			return
		}
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT version, payload_json, sig, pubkey, created_at
		   FROM isolation_certificates WHERE silo_id = ? AND tenant_id = ? ORDER BY version`,
		siloID, tenantID)
	if err != nil {
		log.Printf("certificates query error silo=%s: %v", siloID, err)
		jsonError(w, "certificates failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	certs := []certRecord{}
	for rows.Next() {
		var c certRecord
		if rows.Scan(&c.Version, &c.PayloadJSON, &c.Sig, &c.PubKey, &c.CreatedAt) == nil {
			certs = append(certs, c)
		}
	}

	var dc certRecord
	hasDC := s.db.QueryRowContext(ctx,
		`SELECT payload_json, sig, pubkey, created_at FROM destruction_certificates
		  WHERE silo_id = ? AND tenant_id = ? ORDER BY id DESC LIMIT 1`, siloID, tenantID).
		Scan(&dc.PayloadJSON, &dc.Sig, &dc.PubKey, &dc.CreatedAt) == nil

	resp := map[string]interface{}{
		"certificates": certs,
		"count":        len(certs),
		"verify_hint":  "ed25519.Verify(base64(pubkey), payload_json bytes as stored, base64(sig)); do not re-serialize payload_json",
	}
	if hasDC {
		resp["destruction_certificate"] = dc
	}
	respondJSON(w, resp)
}
