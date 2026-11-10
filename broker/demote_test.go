package main

import (
	"container/heap"
	"math"
	"testing"
)

func withRates(t *testing.T, rates map[string]float64, def float64) {
	t.Helper()
	orig := consolidationCfg
	t.Cleanup(func() { consolidationCfg = orig })
	SetConsolidationConfig(&ConsolidationConfig{
		PromotionThreshold: -2.0,
		DecayRates:         rates,
		DecayDefault:       def,
	})
}

// keepLowest must retain exactly the K lowest activations regardless of the
// order they arrive in — the heap is bounded, so a late-arriving low value has
// to displace an already-held high one.
func TestKeepLowestRetainsBottomK(t *testing.T) {
	h := candidateHeap{}
	heap.Init(&h)

	// Deliberately not sorted: descending, so every push after the first
	// exercises the eviction path.
	for i := 20; i >= 1; i-- {
		keepLowest(&h, demoteCandidate{
			docID:      string(rune('a' + i)),
			activation: float64(i),
		}, 5)
	}

	if h.Len() != 5 {
		t.Fatalf("heap holds %d, want 5", h.Len())
	}
	for _, c := range h {
		if c.activation > 5 {
			t.Errorf("activation %.0f retained; only the 5 lowest (1-5) should survive", c.activation)
		}
	}
}

// A limit of zero or less must retain nothing rather than growing unbounded.
func TestKeepLowestZeroLimit(t *testing.T) {
	h := candidateHeap{}
	heap.Init(&h)
	for i := range 10 {
		keepLowest(&h, demoteCandidate{docID: "x", activation: float64(i)}, 0)
	}
	if h.Len() != 0 {
		t.Errorf("heap holds %d with limit 0, want 0", h.Len())
	}
}

// Ties must break deterministically, or two sweeps over an unchanged corpus
// could demote different documents. This matters in practice: 64% of the
// production corpus shares access_count = 1.
func TestKeepLowestTieBreakIsDeterministic(t *testing.T) {
	run := func() []string {
		h := candidateHeap{}
		heap.Init(&h)
		for _, id := range []string{"d", "a", "c", "b", "e"} {
			keepLowest(&h, demoteCandidate{docID: id, activation: -1.0}, 3)
		}
		out := make([]string, 0, h.Len())
		for _, c := range h {
			out = append(out, c.docID)
		}
		return out
	}
	a, b := run(), run()
	if len(a) != 3 {
		t.Fatalf("retained %d, want 3", len(a))
	}
	seen := map[string]bool{}
	for _, id := range a {
		seen[id] = true
	}
	for _, id := range b {
		if !seen[id] {
			t.Errorf("tie-break not deterministic: %v vs %v", a, b)
		}
	}
}

// activationRanked must never return -Inf. activation() does so by design for
// never-read documents, but under a ranking that collapses 64% of the corpus
// into one indistinguishable bucket and makes their order arbitrary.
func TestActivationRankedIsFiniteAndOrders(t *testing.T) {
	withRates(t, map[string]float64{"notes": 1.0}, 0.1)

	for _, ac := range []int{-1, 0, 1} {
		got := activationRanked(ac, 3600, "notes")
		if math.IsInf(got, -1) || math.IsNaN(got) {
			t.Errorf("activationRanked(%d) = %v; must be finite", ac, got)
		}
	}
	// The contrast is the point.
	if !math.IsInf(activation(0, 3600, "notes"), -1) {
		t.Error("activation() should still return -Inf for access_count 0")
	}

	// Among never-read documents, age must still discriminate — otherwise the
	// ranking has nothing to sort on across most of the corpus.
	young := activationRanked(1, 3600, "notes")
	old := activationRanked(1, 30*86400, "notes")
	if !(old < young) {
		t.Errorf("older doc should rank lower: young=%.3f old=%.3f", young, old)
	}
}

// More retrievals must outrank fewer at equal age, or the model is not
// measuring use at all.
func TestActivationRankedRewardsUse(t *testing.T) {
	withRates(t, map[string]float64{"notes": 1.0}, 0.1)
	const age = 7 * 86400.0
	if !(activationRanked(1, age, "notes") < activationRanked(10, age, "notes")) {
		t.Error("higher access_count should rank higher at equal age")
	}
}

// The decay rate must change the age/use trade-off. At a low rate, use should
// outweigh a large age gap; at a high rate, age should dominate. This is the
// property that makes `d` a ranking weight rather than a global offset.
func TestDecayRateControlsAgeVsUseTradeoff(t *testing.T) {
	const youngAge = 1 * 86400.0
	const oldAge = 60 * 86400.0

	// d = 0.1: a well-used old doc should still beat a never-read young one.
	withRates(t, map[string]float64{"t": 0.1}, 0.1)
	if !(activationRanked(20, oldAge, "t") > activationRanked(1, youngAge, "t")) {
		t.Error("at d=0.1, heavy use should outweigh age")
	}

	// d = 3.0: age dominates, so the young doc wins despite no use.
	withRates(t, map[string]float64{"t": 3.0}, 0.1)
	if !(activationRanked(20, oldAge, "t") < activationRanked(1, youngAge, "t")) {
		t.Error("at d=3.0, age should dominate use")
	}
}

// An unknown namespace must fall back to the configured default, not to a
// hardcoded rate. The production corpus contains a "semantic" namespace that
// exists in no config.
func TestActivationRankedUnknownNamespaceUsesDefault(t *testing.T) {
	withRates(t, map[string]float64{"notes": 1.0}, 0.25)
	got := activationRanked(1, 86400, "semantic")
	want := activationRanked(1, 86400, "") // also unconfigured -> default
	if got != want {
		t.Errorf("unknown namespace = %v, empty = %v; both should use decay_default", got, want)
	}
}

// A fraction outside (0,1) must disable ranked mode rather than being clamped —
// silently demoting the whole corpus on a typo is the failure this guards.
func TestDemoteFractionOutOfRangeDisables(t *testing.T) {
	for _, body := range []string{
		"consolidation:\n  demote_fraction: 1.5\n",
		"consolidation:\n  demote_fraction: 1.0\n",
		"consolidation:\n  demote_fraction: -0.2\n",
	} {
		cfg, err := loadConfig(writeCfg(t, body))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Consolidation.DemoteFraction != 0 {
			t.Errorf("%q left DemoteFraction=%v; want 0 (disabled)", body, cfg.Consolidation.DemoteFraction)
		}
	}

	cfg, err := loadConfig(writeCfg(t, "consolidation:\n  demote_fraction: 0.2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Consolidation.DemoteFraction != 0.2 {
		t.Errorf("valid fraction became %v", cfg.Consolidation.DemoteFraction)
	}
	if cfg.Consolidation.DemoteFloor != 1000 {
		t.Errorf("DemoteFloor = %d, want default 1000", cfg.Consolidation.DemoteFloor)
	}
}

// parseCreatedAt must handle both formats present in the corpus. A document
// whose timestamp cannot be read must be unscoreable, so it is never demoted.
func TestParseCreatedAt(t *testing.T) {
	if _, ok := parseCreatedAt("2026-07-27T10:00:00Z"); !ok {
		t.Error("RFC3339 should parse")
	}
	if _, ok := parseCreatedAt("1784956765"); !ok {
		t.Error("unix seconds should parse")
	}
	for _, bad := range []string{"", "not-a-date", "2026-13-45"} {
		if _, ok := parseCreatedAt(bad); ok {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestAsString(t *testing.T) {
	if got := asString(nil); got != "" {
		t.Errorf("nil -> %q, want empty (absent HMGet field)", got)
	}
	if got := asString("x"); got != "x" {
		t.Errorf("string -> %q", got)
	}
	if got := asString(42); got != "" {
		t.Errorf("non-string -> %q, want empty", got)
	}
}
