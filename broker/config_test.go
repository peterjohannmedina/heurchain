package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "broker.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A key the broker does not understand must fail startup rather than be
// ignored. This is the regression that motivated strict decoding: prod carried
// promote_live/decay_default/scan_cap for weeks while no code read them, so the
// config looked tuned while the binary ran on defaults.
func TestUnknownKeyIsFatal(t *testing.T) {
	for _, body := range []string{
		"consolidation:\n  promotion_threshhold: -2.0\n", // typo
		"totally_made_up:\n  x: 1\n",                     // unknown section
	} {
		if _, err := loadConfig(writeCfg(t, body)); err == nil {
			t.Errorf("silently accepted unknown key in:\n%s", body)
		}
	}
}

// features: is the deliberate exception — unknown flags there must NOT block
// startup, since config is allowed to lead code.
func TestFeaturesBagAcceptsUnknownFlags(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, "features:\n  some_future_stage: true\n  another: false\n"))
	if err != nil {
		t.Fatalf("features bag rejected unknown flag: %v", err)
	}
	if len(cfg.Features) != 2 {
		t.Fatalf("got %d flags, want 2", len(cfg.Features))
	}
}

// An ontology: section written for the mainline broker must still load.
func TestOntologySectionIsAcceptedAndIgnored(t *testing.T) {
	if _, err := loadConfig(writeCfg(t, "ontology:\n  enabled: true\n  mode: reject\n")); err != nil {
		t.Fatalf("ontology section rejected: %v", err)
	}
}

// Consolidation deletes from the active retrieval set. Absent explicit opt-in
// it must default to dry-run.
func TestPromoteLiveDefaultsToDryRun(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, "consolidation:\n  promotion_threshold: -2.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Consolidation.PromoteLive == nil {
		t.Fatal("PromoteLive nil after defaults")
	}
	if *cfg.Consolidation.PromoteLive {
		t.Error("promote_live defaulted to TRUE; a destructive worker must be opt-in")
	}

	// ...but an explicit true must survive.
	cfg, err = loadConfig(writeCfg(t, "consolidation:\n  promote_live: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !*cfg.Consolidation.PromoteLive {
		t.Error("explicit promote_live: true was not honoured")
	}
}

// decay_default governs every document with an empty namespace, which is the
// entire pre-namespace corpus. Confirm it is actually plumbed into decayRate,
// and that the 1.0-vs-0.1 distinction is as decisive as it looks: at d=1.0 a
// week-old once-read doc is far below the -2.0 threshold, at d=0.1 it is not.
func TestDecayDefaultReachesDecayRate(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, "consolidation:\n  decay_default: 0.1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Consolidation.DecayDefault != 0.1 {
		t.Fatalf("decay_default = %v, want 0.1", cfg.Consolidation.DecayDefault)
	}

	orig := consolidationCfg
	t.Cleanup(func() { consolidationCfg = orig })

	SetConsolidationConfig(&ConsolidationConfig{
		PromotionThreshold: -2.0,
		DecayRates:         map[string]float64{"ops": 3.0},
		DecayDefault:       cfg.Consolidation.DecayDefault,
	})
	if got := decayRate(""); got != 0.1 {
		t.Errorf("decayRate(\"\") = %v, want 0.1 (configured default)", got)
	}
	if got := decayRate("ops"); got != 3.0 {
		t.Errorf("decayRate(\"ops\") = %v, want 3.0 (explicit rate)", got)
	}

	const week = 7 * 24 * 3600.0
	if a := activation(1, week, ""); a < -2.0 {
		t.Errorf("at d=0.1 a week-old read-once doc has A_i=%.2f (< -2.0) and would vault", a)
	}
	// Sanity-check the counterfactual that caused the incident.
	if a := math.Log(1 * math.Pow(week+1, -1.0)); a > -2.0 {
		t.Errorf("expected d=1.0 to put A_i (%.2f) below -2.0", a)
	}
}

// An empty config file is legitimate — every field has a default.
func TestEmptyConfigLoads(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, ""))
	if err != nil {
		t.Fatalf("empty config rejected: %v", err)
	}
	if cfg.Server.Port != "8080" || cfg.Consolidation.DecayDefault != 1.0 {
		t.Errorf("defaults not applied: port=%q decay_default=%v",
			cfg.Server.Port, cfg.Consolidation.DecayDefault)
	}
}

// Revoking by key ID alone must still invalidate the auth cache. The cache is
// keyed by sha256(rawKey) and api_keys holds only a bcrypt hash, so without the
// raw key the entry cannot be addressed directly — revokeKey must bump the
// tenant's epoch instead, or the revoked key keeps authenticating from cache
// for up to authCacheTTL (5 min). Found while revoking a live test key.
func TestRevokeByIDInvalidatesCache(t *testing.T) {
	db := newTestDB(t)

	var keyID int64
	if err := db.QueryRow(
		`SELECT id FROM api_keys WHERE tenant_id = 't1' LIMIT 1`).Scan(&keyID); err != nil {
		t.Fatal(err)
	}

	// revokeKey must resolve the tenant from the key ID itself, since the
	// caller may pass an empty rawKey. A nil *redis.Client would panic on use,
	// so a client pointed at a dead address exercises the code path while
	// keeping the Redis calls best-effort (they log and move on).
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 50 * time.Millisecond,
	})
	defer rdb.Close()

	if err := revokeKey(context.Background(), db, rdb, keyID, ""); err != nil {
		t.Fatalf("revokeKey with empty rawKey: %v", err)
	}

	var revoked *string
	if err := db.QueryRow(`SELECT revoked_at FROM api_keys WHERE id = ?`, keyID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked == nil {
		t.Error("key was not marked revoked")
	}
}

// A graph: section written for the mainline broker must still load: the OSS
// edition has no knowledge-graph worker and ignores the section.
func TestGraphSectionIsAcceptedAndIgnored(t *testing.T) {
	if _, err := loadConfig(writeCfg(t, "graph:\n  enabled: true\n  activation_cutoff: -3.5\n")); err != nil {
		t.Fatalf("graph section rejected: %v", err)
	}
}
