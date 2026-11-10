package main

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"testing"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

// newTestDB seeds an in-memory SQLite with the v1–v2 base (the parts the
// silo/engagement code touches) and runs the v4+ migrations.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	base := []string{
		`CREATE TABLE tenants (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, email TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT, tenant_id TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
			key_hash TEXT NOT NULL, name TEXT NOT NULL DEFAULT 'default',
			rate_limit_qps INTEGER NOT NULL DEFAULT 10, rate_limit_burst INTEGER NOT NULL DEFAULT 20,
			last_used_at DATETIME, created_at DATETIME DEFAULT CURRENT_TIMESTAMP, revoked_at DATETIME)`,
		`CREATE TABLE usage_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT, tenant_id TEXT NOT NULL,
			event_type TEXT NOT NULL CHECK (event_type IN ('store','query','embed','search','flush')),
			tokens_used INTEGER NOT NULL DEFAULT 0, vectors_stored INTEGER NOT NULL DEFAULT 0,
			metadata TEXT, agent_id TEXT, created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT, tenant_id TEXT, actor TEXT NOT NULL,
			action TEXT NOT NULL, resource TEXT, ip_address TEXT, user_agent TEXT,
			success BOOLEAN NOT NULL DEFAULT 1, details TEXT, agent_id TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE schema_version (
			version INTEGER PRIMARY KEY, applied_at DATETIME DEFAULT CURRENT_TIMESTAMP, description TEXT)`,
		`INSERT INTO schema_version (version, description) VALUES (1, 'base'), (2, 'agent_id')`,
		`INSERT INTO tenants (id, name, email) VALUES ('t1', 'Sigma', 'ops@sigma.test')`,
		`INSERT INTO api_keys (tenant_id, key_hash, name) VALUES ('t1', 'x', 'tenant-key')`,     // key 1
		`INSERT INTO api_keys (tenant_id, key_hash, name) VALUES ('t1', 'y', 'consultant-key')`, // key 2
	}
	for _, stmt := range base {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed base: %v\n%s", err, stmt)
		}
	}
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}
	// api_keys.role was added by v4 with DEFAULT 'tenant'
	if _, err := db.Exec(`UPDATE api_keys SET role = 'consultant', principal = 'jane@firm' WHERE id = 2`); err != nil {
		t.Fatalf("set consultant role: %v", err)
	}
	return db
}

func newTestCrypto(t *testing.T) *siloCrypto {
	t.Helper()
	return &siloCrypto{
		kek:      make([]byte, 32),
		kekID:    "kek-test",
		fpKey:    append(make([]byte, 31), 1),
		cache:    make(map[string]cachedDEK),
		shredded: make(map[string]struct{}),
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	return &Server{
		db:     newTestDB(t),
		crypto: newTestCrypto(t),
		attest: &attestor{priv: priv, pub: priv.Public().(ed25519.PublicKey)},
	}
}

// seedSilo inserts a silo + wrapped DEK directly (bypassing HTTP).
func seedSilo(t *testing.T, s *Server, id, name, shardMode string) string {
	t.Helper()
	wrapped, fp, err := s.crypto.newDEK(id)
	if err != nil {
		t.Fatalf("newDEK: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO silos (id, tenant_id, name, isolation, crypto, shard_mode, status)
		 VALUES (?, 't1', ?, 'isolated', 'firm', ?, 'active')`, id, name, shardMode); err != nil {
		t.Fatalf("insert silo: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO silo_keys (silo_id, wrapped_dek, kek_id, key_version, dek_fingerprint)
		 VALUES (?, ?, 'kek-test', 1, ?)`, id, wrapped, fp); err != nil {
		t.Fatalf("insert silo_keys: %v", err)
	}
	return fp
}

// ---------------------------------------------------------------------------
// Envelope crypto
// ---------------------------------------------------------------------------

func TestDEKWrapUnwrapRoundtrip(t *testing.T) {
	sc := newTestCrypto(t)
	wrapped, fp, err := sc.newDEK("silo-a")
	if err != nil {
		t.Fatal(err)
	}
	dek, err := sc.unwrapDEK("silo-a", wrapped)
	if err != nil {
		t.Fatalf("unwrap: %v", err)
	}
	if len(dek) != 32 {
		t.Fatalf("dek length = %d, want 32", len(dek))
	}
	if sc.fingerprint(dek) != fp {
		t.Fatal("fingerprint of unwrapped DEK does not match creation-time fingerprint")
	}
}

func TestDEKWrapBindsSiloID(t *testing.T) {
	sc := newTestCrypto(t)
	wrapped, _, err := sc.newDEK("silo-a")
	if err != nil {
		t.Fatal(err)
	}
	// A wrapped DEK copied to another silo's row must fail to unwrap (AAD).
	if _, err := sc.unwrapDEK("silo-b", wrapped); err == nil {
		t.Fatal("unwrap with wrong silo id succeeded; AAD binding broken")
	}
}

func TestFingerprintStableAcrossKEKRewrap(t *testing.T) {
	// The destruction-certificate equality guarantee depends on the
	// fingerprint committing to the PLAINTEXT DEK, not the wrapping.
	sc := newTestCrypto(t)
	wrapped, fp, err := sc.newDEK("silo-a")
	if err != nil {
		t.Fatal(err)
	}
	dek, err := sc.unwrapDEK("silo-a", wrapped)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate KEK rotation: re-wrap the same DEK under a new KEK.
	sc2 := &siloCrypto{kek: append(make([]byte, 31), 7), kekID: "kek-2", fpKey: sc.fpKey, cache: make(map[string]cachedDEK), shredded: make(map[string]struct{})}
	rewrapped, err := gcmSeal(sc2.kek, dek, []byte("silo:silo-a"))
	if err != nil {
		t.Fatal(err)
	}
	dek2, err := sc2.unwrapDEK("silo-a", rewrapped)
	if err != nil {
		t.Fatal(err)
	}
	if sc2.fingerprint(dek2) != fp {
		t.Fatal("fingerprint changed across KEK re-wrap; destruction-certificate equality would break")
	}
}

func TestSealOpenContentAndAAD(t *testing.T) {
	sc := newTestCrypto(t)
	wrapped, _, _ := sc.newDEK("silo-a")
	dek, _ := sc.unwrapDEK("silo-a", wrapped)

	blob, err := sealContent(dek, "doc-1", []byte(`{"text":"acme work product"}`))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := openContent(dek, "doc-1", blob)
	if err != nil || string(plain) != `{"text":"acme work product"}` {
		t.Fatalf("roundtrip failed: %v %q", err, plain)
	}
	// Ciphertext replayed onto a different doc id must fail (AAD).
	if _, err := openContent(dek, "doc-2", blob); err == nil {
		t.Fatal("ciphertext accepted under wrong doc id; AAD binding broken")
	}
}

func TestEvictDEKRemovesCachedKey(t *testing.T) {
	sc := newTestCrypto(t)
	wrapped, _, _ := sc.newDEK("silo-a")
	if _, err := sc.unwrapDEK("silo-a", wrapped); err != nil {
		t.Fatal(err)
	}
	sc.evictDEK("silo-a")
	if _, ok := sc.cache["silo-a"]; ok {
		t.Fatal("DEK still cached after evict; shredded silo would remain decryptable")
	}
}

// ---------------------------------------------------------------------------
// Migrations
// ---------------------------------------------------------------------------

func TestMigrationsApplyOnceAndAreIdempotent(t *testing.T) {
	db := newTestDB(t)
	var v int
	if err := db.QueryRow(`SELECT MAX(version) FROM schema_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	// Assert against the migration table rather than a literal, so adding a
	// migration doesn't require editing this test to keep it passing.
	want := migrations[len(migrations)-1].version
	if v != want {
		t.Fatalf("schema version = %d, want %d (highest defined migration)", v, want)
	}
	// Re-running must be a no-op, not an ALTER-duplicate-column error.
	if err := runMigrations(db); err != nil {
		t.Fatalf("second runMigrations: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Four-gate enforcement
// ---------------------------------------------------------------------------

func grantWrite(t *testing.T, db *sql.DB, keyID int64, siloID string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO silo_grants (key_id, silo_id, perm) VALUES (?, ?, 'write')`, keyID, siloID); err != nil {
		t.Fatal(err)
	}
}

func seedEngagement(t *testing.T, db *sql.DB, id, siloID, status string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO engagements (id, tenant_id, customer_id, silo_id, status) VALUES (?, 't1', 'acme', ?, ?)`,
		id, siloID, status); err != nil {
		t.Fatal(err)
	}
}

func addMember(t *testing.T, db *sql.DB, engID string, keyID int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO engagement_consultants (engagement_id, key_id, principal) VALUES (?, ?, 'jane@firm')`,
		engID, keyID); err != nil {
		t.Fatal(err)
	}
}

func wantDeny(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected denial %s, got success", code)
	}
	ee, ok := asEnforceError(err)
	if !ok {
		t.Fatalf("expected enforce error %s, got system error: %v", code, err)
	}
	if ee.Code != code {
		t.Fatalf("expected denial %s, got %s", code, ee.Code)
	}
}

func TestGate1WriteRequiresGrant(t *testing.T) {
	s := newTestServer(t)
	seedSilo(t, s, "s-acme", "acme", "dedicated")
	ctx := context.Background()

	// No grant at all → not-found (non-enumerable), not a grant-specific code.
	_, err := s.enforceGates(ctx, "t1", 2, "acme", "")
	wantDeny(t, err, denySiloNotFound)

	// Read-only grant → the key legitimately knows the silo; write denied
	// with the specific code.
	if _, err := s.db.Exec(`INSERT INTO silo_grants (key_id, silo_id, perm) VALUES (2, 's-acme', 'read')`); err != nil {
		t.Fatal(err)
	}
	_, err = s.enforceGates(ctx, "t1", 2, "acme", "")
	wantDeny(t, err, denyNoWriteGrant)

	grantWrite(t, s.db, 2, "s-acme")
	if _, err := s.enforceGates(ctx, "t1", 2, "acme", ""); err != nil {
		t.Fatalf("granted write denied: %v", err)
	}
}

func TestGate1RevokedGrantAndRevokedKeyDeny(t *testing.T) {
	s := newTestServer(t)
	seedSilo(t, s, "s-acme", "acme", "dedicated")
	grantWrite(t, s.db, 2, "s-acme")
	ctx := context.Background()

	// Revoked grant = no grant at all → collapses to not-found.
	if _, err := s.db.Exec(`UPDATE silo_grants SET revoked_at = CURRENT_TIMESTAMP WHERE key_id = 2`); err != nil {
		t.Fatal(err)
	}
	_, err := s.enforceGates(ctx, "t1", 2, "acme", "")
	wantDeny(t, err, denySiloNotFound)

	if _, err := s.db.Exec(`UPDATE silo_grants SET revoked_at = NULL WHERE key_id = 2`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE api_keys SET revoked_at = CURRENT_TIMESTAMP WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	_, err = s.enforceGates(ctx, "t1", 2, "acme", "")
	wantDeny(t, err, denySiloNotFound)
}

func TestGate2MembershipRequired(t *testing.T) {
	s := newTestServer(t)
	seedSilo(t, s, "s-acme", "acme", "dedicated")
	grantWrite(t, s.db, 2, "s-acme")
	seedEngagement(t, s.db, "ENG-1", "s-acme", "active")
	ctx := context.Background()

	_, err := s.enforceGates(ctx, "t1", 2, "acme", "ENG-1")
	wantDeny(t, err, denyNotEngagementMember)

	addMember(t, s.db, "ENG-1", 2)
	if _, err := s.enforceGates(ctx, "t1", 2, "acme", "ENG-1"); err != nil {
		t.Fatalf("member denied: %v", err)
	}

	// Removing the member re-denies even though the silo grant remains.
	if _, err := s.db.Exec(`UPDATE engagement_consultants SET removed_at = CURRENT_TIMESTAMP WHERE engagement_id = 'ENG-1'`); err != nil {
		t.Fatal(err)
	}
	_, err = s.enforceGates(ctx, "t1", 2, "acme", "ENG-1")
	wantDeny(t, err, denyNotEngagementMember)
}

func TestGate3EngagementMustBeActiveAndInWindow(t *testing.T) {
	s := newTestServer(t)
	seedSilo(t, s, "s-acme", "acme", "dedicated")
	grantWrite(t, s.db, 2, "s-acme")
	seedEngagement(t, s.db, "ENG-1", "s-acme", "paused")
	addMember(t, s.db, "ENG-1", 2)
	ctx := context.Background()

	_, err := s.enforceGates(ctx, "t1", 2, "acme", "ENG-1")
	wantDeny(t, err, denyEngagementInactive)

	if _, err := s.db.Exec(`UPDATE engagements SET status = 'active', ends_at = '2020-01-01T00:00:00Z' WHERE id = 'ENG-1'`); err != nil {
		t.Fatal(err)
	}
	_, err = s.enforceGates(ctx, "t1", 2, "acme", "ENG-1")
	wantDeny(t, err, denyEngagementWindow)
}

func TestGate4EngagementSiloMismatch(t *testing.T) {
	// Even a wrongly-granted key cannot steer an Acme-bound session's writes
	// into Beta: the engagement pins the silo.
	s := newTestServer(t)
	seedSilo(t, s, "s-acme", "acme", "dedicated")
	seedSilo(t, s, "s-beta", "beta", "dedicated")
	grantWrite(t, s.db, 2, "s-acme")
	grantWrite(t, s.db, 2, "s-beta") // the "wrongly granted" extra silo
	seedEngagement(t, s.db, "ENG-1", "s-acme", "active")
	addMember(t, s.db, "ENG-1", 2)
	ctx := context.Background()

	// Router/session steered to beta while bound to the acme engagement.
	_, err := s.enforceGates(ctx, "t1", 2, "beta", "ENG-1")
	wantDeny(t, err, denyEngagementSiloMismatch)
}

func TestShreddedSiloDeniesAndReadFailsClosed(t *testing.T) {
	s := newTestServer(t)
	seedSilo(t, s, "s-acme", "acme", "dedicated")
	grantWrite(t, s.db, 2, "s-acme")
	ctx := context.Background()

	if _, err := s.db.Exec(`DELETE FROM silo_keys WHERE silo_id = 's-acme'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE silos SET status = 'shredded' WHERE id = 's-acme'`); err != nil {
		t.Fatal(err)
	}
	_, err := s.enforceGates(ctx, "t1", 2, "acme", "")
	wantDeny(t, err, denySiloShredded)
	_, err = s.checkReadGrant(ctx, "t1", 2, "acme")
	wantDeny(t, err, denySiloShredded)
}

func TestWriteDenyIsIndistinguishableFromNotFound(t *testing.T) {
	// Write-plane non-enumerability: a key with NO grant on a silo must get
	// the same code for "exists, ungranted", "exists but shredded", and "no
	// such silo" — otherwise /store {"silo": guess} is a client-roster oracle.
	s := newTestServer(t)
	seedSilo(t, s, "s-acme", "acme", "dedicated")
	seedSilo(t, s, "s-gone", "gone", "dedicated")
	if _, err := s.db.Exec(`DELETE FROM silo_keys WHERE silo_id = 's-gone'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE silos SET status = 'shredded' WHERE id = 's-gone'`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	_, errUngranted := s.enforceGates(ctx, "t1", 2, "acme", "")
	_, errShredded := s.enforceGates(ctx, "t1", 2, "gone", "")
	_, errMissing := s.enforceGates(ctx, "t1", 2, "gamma", "")
	for name, err := range map[string]error{"ungranted": errUngranted, "shredded": errShredded, "missing": errMissing} {
		ee, ok := asEnforceError(err)
		if !ok || ee.Code != denySiloNotFound {
			t.Errorf("%s silo write probe returned %v; want %s (enumeration oracle)", name, err, denySiloNotFound)
		}
	}
}

func TestShreddedDEKCannotBeReCached(t *testing.T) {
	// A request racing a crypto-shred must not re-cache (or use) the
	// destroyed DEK even if it read the wrapped bytes before the DELETE.
	sc := newTestCrypto(t)
	wrapped, _, _ := sc.newDEK("silo-a")
	if _, err := sc.unwrapDEK("silo-a", wrapped); err != nil {
		t.Fatal(err)
	}
	sc.evictDEK("silo-a")
	if _, err := sc.unwrapDEK("silo-a", wrapped); err == nil {
		t.Fatal("unwrap succeeded after shred eviction; destroyed DEK re-cached")
	}
}

func TestReadDenyIsIndistinguishableFromNotFound(t *testing.T) {
	// Ungranted silos must be non-enumerable: same code for "no grant" and
	// "no such silo".
	s := newTestServer(t)
	seedSilo(t, s, "s-acme", "acme", "dedicated")
	ctx := context.Background()

	_, errNoGrant := s.checkReadGrant(ctx, "t1", 2, "acme")
	_, errNoSilo := s.checkReadGrant(ctx, "t1", 2, "gamma")
	ee1, _ := asEnforceError(errNoGrant)
	ee2, _ := asEnforceError(errNoSilo)
	if ee1 == nil || ee2 == nil || ee1.Code != ee2.Code {
		t.Fatalf("deny (%v) and not-found (%v) are distinguishable; silos are enumerable", errNoGrant, errNoSilo)
	}
}

// ---------------------------------------------------------------------------
// Engagement lifecycle
// ---------------------------------------------------------------------------

func TestTransitionMatrix(t *testing.T) {
	allowed := [][2]string{
		{"draft", "active"}, {"active", "paused"}, {"paused", "active"},
		{"active", "closed"}, {"paused", "closed"}, {"closed", "shred"},
	}
	denied := [][2]string{
		{"draft", "closed"}, {"draft", "shred"}, {"active", "shred"},
		{"closed", "active"}, {"shred", "active"}, {"shred", "closed"},
		{"paused", "shred"}, {"closed", "draft"},
	}
	for _, p := range allowed {
		if !transitionAllowed(p[0], p[1]) {
			t.Errorf("transition %s -> %s should be allowed", p[0], p[1])
		}
	}
	for _, p := range denied {
		if transitionAllowed(p[0], p[1]) {
			t.Errorf("transition %s -> %s should be denied", p[0], p[1])
		}
	}
}

func TestEngagementAuditChainVerifies(t *testing.T) {
	s := newTestServer(t)
	seedSilo(t, s, "s-acme", "acme", "dedicated")
	seedEngagement(t, s.db, "ENG-1", "s-acme", "draft")
	ctx := context.Background()

	s.appendEngagementAudit(ctx, "ENG-1", "1", "", "draft", "created")
	s.appendEngagementAudit(ctx, "ENG-1", "1", "draft", "active", "kickoff")
	s.appendEngagementAudit(ctx, "ENG-1", "1", "active", "closed", "done")

	rows, err := s.db.Query(
		`SELECT prev_checksum, checksum, payload_json FROM engagement_audit WHERE engagement_id = 'ENG-1' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	prev := ""
	n := 0
	for rows.Next() {
		var prevCS sql.NullString
		var cs, payload string
		if err := rows.Scan(&prevCS, &cs, &payload); err != nil {
			t.Fatal(err)
		}
		if prevCS.String != prev {
			t.Fatalf("row %d: prev_checksum %q != expected %q", n, prevCS.String, prev)
		}
		if chainChecksum(prev, payload) != cs {
			t.Fatalf("row %d: checksum does not verify", n)
		}
		prev = cs
		n++
	}
	if n != 3 {
		t.Fatalf("chain rows = %d, want 3", n)
	}
}

// ---------------------------------------------------------------------------
// Attestation
// ---------------------------------------------------------------------------

func TestIACMintDeriveAndVerify(t *testing.T) {
	s := newTestServer(t)
	seedSilo(t, s, "s-acme", "acme", "dedicated")
	ctx := context.Background()

	// v1: empty decrypt set (plus disclosed firm-admin).
	c1, err := s.mintIAC(ctx, "t1", "s-acme", "")
	if err != nil {
		t.Fatalf("mint v1: %v", err)
	}
	if c1.Version != 1 {
		t.Fatalf("version = %d, want 1", c1.Version)
	}

	// Grant read to the consultant key: the decrypt set expands, so a new
	// version must carry the new principal and chain to v1.
	if _, err := s.db.Exec(`INSERT INTO silo_grants (key_id, silo_id, perm) VALUES (2, 's-acme', 'read')`); err != nil {
		t.Fatal(err)
	}
	c2, err := s.mintIAC(ctx, "t1", "s-acme", "")
	if err != nil {
		t.Fatalf("mint v2: %v", err)
	}
	if c2.Version != 2 {
		t.Fatalf("version = %d, want 2", c2.Version)
	}

	var p2 iacPayload
	if err := json.Unmarshal([]byte(c2.PayloadJSON), &p2); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, pr := range p2.AuthorizedDecryptPrincipals {
		if pr == "jane@firm" {
			found = true
		}
	}
	if !found {
		t.Fatalf("v2 principals missing jane@firm: %v", p2.AuthorizedDecryptPrincipals)
	}
	if p2.PrevCertChecksum != chainChecksum("", c1.PayloadJSON) {
		t.Fatal("v2 prev_cert_checksum does not chain to v1 payload")
	}

	// Offline verification: signature over the literal stored payload bytes.
	pub, _ := base64.StdEncoding.DecodeString(c2.PubKey)
	sig, _ := base64.StdEncoding.DecodeString(c2.Sig)
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(c2.PayloadJSON), sig) {
		t.Fatal("IAC signature does not verify over stored payload bytes")
	}
}

func TestDestructionCertFingerprintEqualsIAC(t *testing.T) {
	s := newTestServer(t)
	fp := seedSilo(t, s, "s-acme", "acme", "dedicated")
	ctx := context.Background()

	c1, err := s.mintIAC(ctx, "t1", "s-acme", "")
	if err != nil {
		t.Fatal(err)
	}
	var p1 iacPayload
	_ = json.Unmarshal([]byte(c1.PayloadJSON), &p1)
	if p1.DEKFingerprint != fp {
		t.Fatal("IAC fingerprint != stored creation-time fingerprint")
	}

	dc, err := s.mintDestructionCert(ctx, "t1", "s-acme", fp)
	if err != nil {
		t.Fatal(err)
	}
	var dp destructionPayload
	if err := json.Unmarshal([]byte(dc.PayloadJSON), &dp); err != nil {
		t.Fatal(err)
	}
	if dp.DestroyedFingerprint != p1.DEKFingerprint {
		t.Fatal("destroyed_fingerprint != IAC dek_fingerprint; the offboarding equality guarantee is broken")
	}
	if dp.BackupCaveat == "" || dp.CompletesAt == "" {
		t.Fatal("destruction certificate must disclose the backup-retention caveat and completion time")
	}

	pub, _ := base64.StdEncoding.DecodeString(dc.PubKey)
	sig, _ := base64.StdEncoding.DecodeString(dc.Sig)
	if !ed25519.Verify(ed25519.PublicKey(pub), []byte(dc.PayloadJSON), sig) {
		t.Fatal("destruction certificate signature does not verify")
	}
}

// ---------------------------------------------------------------------------
// Telemetry columns
// ---------------------------------------------------------------------------

func TestUsageEventsAcceptSiloAndEngagementColumns(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec(
		`INSERT INTO usage_events (tenant_id, event_type, vectors_stored, silo_id, engagement_id)
		 VALUES ('t1', 'store', 1, 's-acme', 'ENG-1')`); err != nil {
		t.Fatalf("v6 columns missing on usage_events: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO audit_log (tenant_id, actor, action, silo_id, engagement_id)
		 VALUES ('t1', '1', 'store', 's-acme', 'ENG-1')`); err != nil {
		t.Fatalf("v6 columns missing on audit_log: %v", err)
	}
}
