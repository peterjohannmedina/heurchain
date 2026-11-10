package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Namespace validation — the /store contract
//
// These assert the rule handleStore enforces at handlers.go:213 without
// standing up Redis: an unknown namespace must be rejected rather than
// silently falling back to d=1.0. That silent fallback is exactly what made
// 100% of the live corpus vault-eligible, so the guard is worth pinning.
// ---------------------------------------------------------------------------

// namespaceAccepted calls the same resolveNamespace the handler calls, so this
// exercises production code rather than a reimplementation of it.
func namespaceAccepted(ns string) (string, bool) {
	got, err := resolveNamespace(ns)
	return got, err == nil
}

func TestNamespaceValidationMatrix(t *testing.T) {
	orig := consolidationCfg
	t.Cleanup(func() { consolidationCfg = orig })
	SetConsolidationConfig(&ConsolidationConfig{
		PromotionThreshold: -2.0,
		DecayRates:         map[string]float64{"ops": 3.0, "notes": 1.0, "self": 0.1},
		DecayDefault:       0.1,
	})

	for _, tc := range []struct {
		in     string
		want   string
		accept bool
	}{
		{"", "notes", true},      // default tier
		{"notes", "notes", true}, // explicit
		{"ops", "ops", true},     // fast decay
		{"self", "self", true},   // slow decay
		{"Notes", "", false},     // case-sensitive: not a configured tier
		{"nonsense", "", false},  // typo must 400, not silently become d=1.0
		{"ops ", "", false},      // trailing space is a different key
		{"../../etc", "", false}, // no path-ish values leak into a Redis field
	} {
		got, ok := namespaceAccepted(tc.in)
		if ok != tc.accept {
			t.Errorf("namespace %q: accepted=%v, want %v", tc.in, ok, tc.accept)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("namespace %q resolved to %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Each configured tier must produce a distinct decay rate. If tiers collapse
// to one value the ACT-R model is decorative.
func TestDecayTiersAreDistinct(t *testing.T) {
	orig := consolidationCfg
	t.Cleanup(func() { consolidationCfg = orig })
	SetConsolidationConfig(&ConsolidationConfig{
		PromotionThreshold: -2.0,
		DecayRates:         map[string]float64{"ops": 3.0, "notes": 1.0, "self": 0.1},
		DecayDefault:       0.1,
	})

	const week = 7 * 24 * 3600.0
	ops, notes, self := activation(1, week, "ops"), activation(1, week, "notes"), activation(1, week, "self")
	if !(ops < notes && notes < self) {
		t.Errorf("tier ordering broken: ops=%.2f notes=%.2f self=%.2f (want ops < notes < self)", ops, notes, self)
	}
	// ops decays fast enough to vault in a week; self must not.
	if ops >= -2.0 {
		t.Errorf("ops A_i=%.2f should be below the -2.0 threshold after a week", ops)
	}
	if self < -2.0 {
		t.Errorf("self A_i=%.2f should survive a week", self)
	}
}

// ---------------------------------------------------------------------------
// Edge activation
// ---------------------------------------------------------------------------

// computeEdgeActivation must never return -Inf. activation() deliberately does
// for never-read docs, but an edge stays traversable regardless of whether its
// source doc was re-read, so the access count floors at 1.
func TestEdgeActivationNeverNegInf(t *testing.T) {
	orig := consolidationCfg
	t.Cleanup(func() { consolidationCfg = orig })
	SetConsolidationConfig(&ConsolidationConfig{
		DecayRates:   map[string]float64{"ops": 3.0, "notes": 1.0, "self": 0.1},
		DecayDefault: 1.0,
	})

	for _, ac := range []int{-5, 0, 1, 10} {
		for _, age := range []float64{0, 1, 86400, 365 * 86400} {
			got := computeEdgeActivation(ac, age, "notes")
			if math.IsInf(got, -1) || math.IsNaN(got) {
				t.Errorf("computeEdgeActivation(%d, %.0f) = %v; must be finite", ac, age, got)
			}
		}
	}
	// Contrast with activation(), which is -Inf by design at ac=0.
	if !math.IsInf(activation(0, 100, "notes"), -1) {
		t.Error("activation() should stay -Inf for never-accessed docs")
	}
	// Edges must respect the configured decay default for unknown namespaces.
	if a, b := computeEdgeActivation(1, 86400, "zzz"), computeEdgeActivation(1, 86400, "notes"); a != b {
		t.Errorf("unknown namespace %v should fall back to the default rate %v", a, b)
	}
}

// Every edge scored at insert time must clear the configured cutoff, otherwise
// the v8 schema's -999 skip-sentinel is doing work it shouldn't have to.
func TestEdgeActivationVsCutoff(t *testing.T) {
	orig := consolidationCfg
	t.Cleanup(func() { consolidationCfg = orig })
	SetConsolidationConfig(&ConsolidationConfig{
		DecayRates:   map[string]float64{"notes": 1.0},
		DecayDefault: 0.1,
	})
	// A year-old, once-read doc in the default tier should still be traversable
	// at the -10.0 cutoff shipped in broker.yaml.
	if a := computeEdgeActivation(1, 365*86400, ""); a < -10.0 {
		t.Errorf("A_i=%.2f falls below the -10.0 cutoff; graph recall would skip year-old edges", a)
	}
}

// ---------------------------------------------------------------------------
// Pending-stream payload
// ---------------------------------------------------------------------------

// The worker reads msg.Values["data"] as a string and unmarshals it. Round-trip
// the exact shape queueHeurgraphExtraction writes so a field rename can't
// silently break the worker.
func TestPendingItemRoundTrip(t *testing.T) {
	in := heurgraphPendingItem{
		DocID:     "doc-1",
		AgentID:   "agent-7",
		SessionID: "sess-9",
		Namespace: "ops",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}

	msg := redis.XMessage{ID: "1-0", Values: map[string]interface{}{"data": string(data)}}
	raw, ok := msg.Values["data"].(string)
	if !ok {
		t.Fatal("data field is not a string; worker's type assertion would fail")
	}
	var out heurgraphPendingItem
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

// A structurally invalid stream entry must be classified errPermanent so it is
// acked and dropped. Left retryable it would be reclaimed by XAutoClaim every
// tick forever, wedging the pending-entries list behind an entry that can never
// succeed.
//
// This calls the real processHeurgraphItem. A nil *redis.Client would panic if
// reached, which is the point: every case here must be rejected during parsing,
// before any Redis call. A case that regressed into touching Redis would fail
// loudly rather than silently pass.
func TestMalformedStreamEntryIsPermanent(t *testing.T) {
	for name, values := range map[string]map[string]interface{}{
		"missing data field": {"other": "x"},
		"data not a string":  {"data": 42},
		"data is nil":        {"data": nil},
		"malformed json":     {"data": "{not json"},
		"json wrong type":    {"data": `["array","not","object"]`},
	} {
		t.Run(name, func(t *testing.T) {
			msg := redis.XMessage{ID: "1-0", Values: values}
			err := processHeurgraphItem(context.Background(), nil, nil, "t1", msg)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, errPermanent) {
				t.Errorf("got %v; want errPermanent so the entry is acked and dropped", err)
			}
		})
	}
}

// A well-formed entry must NOT be classified permanent — it has to survive to
// be retried once extraction exists. Guarded by the same nil-client trick: a
// valid entry is expected to reach the Redis read, so this asserts the
// classification boundary without a live server.
func TestWellFormedEntryIsNotPermanent(t *testing.T) {
	data, err := json.Marshal(heurgraphPendingItem{DocID: "doc-1", Namespace: "notes"})
	if err != nil {
		t.Fatal(err)
	}
	msg := redis.XMessage{ID: "1-0", Values: map[string]interface{}{"data": string(data)}}

	// Reaching the HGetAll with a nil client panics; recover and treat that as
	// proof the entry parsed cleanly rather than being rejected as permanent.
	defer func() {
		if rec := recover(); rec != nil {
			return // parsed fine, died at the Redis boundary — expected
		}
	}()
	if err := processHeurgraphItem(context.Background(), nil, nil, "t1", msg); errors.Is(err, errPermanent) {
		t.Errorf("well-formed entry classified permanent (%v); it would be dropped instead of retried", err)
	}
}

// ---------------------------------------------------------------------------
// Key derivation — tenant isolation
// ---------------------------------------------------------------------------

// Graph keys must be tenant-scoped, and must not collide with the corpus
// keyspace the module is forbidden from touching.
func TestGraphKeysAreTenantScopedAndDistinct(t *testing.T) {
	const a, b = "tenant-a", "tenant-b"

	if heurgraphPendingKey(a) == heurgraphPendingKey(b) {
		t.Error("pending stream key is not tenant-scoped")
	}
	if heurgraphAdjKey(a, "e1") == heurgraphAdjKey(b, "e1") {
		t.Error("adjacency key is not tenant-scoped")
	}
	if heurgraphEntKey(a, "e1") == heurgraphEntKey(b, "e1") {
		t.Error("entity key is not tenant-scoped")
	}

	// Must never alias the corpus keyspace.
	corpus := map[string]bool{vecKey(a, "e1"): true, vecSetKey(a): true}
	for _, k := range []string{
		heurgraphPendingKey(a), heurgraphAdjKey(a, "e1"), heurgraphEntKey(a, "e1"),
	} {
		if corpus[k] {
			t.Errorf("graph key %q collides with a corpus key", k)
		}
	}
}

func TestExtractionRuntimeGateDefaultOff(t *testing.T) {
	original := heurgraphCfg
	t.Cleanup(func() { heurgraphCfg = original })
	heurgraphCfg = HeurgraphConfig{}
	drainHeurgraphStream(context.Background(), nil, nil)
	if graphTenantEnabled("t1") {
		t.Fatal("graph must default off")
	}
}
