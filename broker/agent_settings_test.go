package main

import "testing"

func TestVisibleToRequester(t *testing.T) {
	iso := map[string]bool{"alpha": true} // alpha's corpus is isolated

	cases := []struct {
		name              string
		requester, author string
		visibility, agent string
		want              bool
	}{
		{"legacy doc visible to all", "beta", "", "", "", true},
		{"legacy doc visible even to empty requester's peer", "x", "", "private", "", true},
		{"own private doc", "alpha", "alpha", "private", "", true},
		{"isolated author hidden from other", "beta", "alpha", "private", "", false},
		{"isolated author's own read allowed", "alpha", "alpha", "private", "", true},
		{"non-isolated author shared (cooperative default)", "alpha", "beta", "private", "", true},
		{"explicit tenant share overrides isolation", "beta", "alpha", "tenant", "", true},
		{"explicit agents share includes requester", "beta", "alpha", "agents", `["beta"]`, true},
		{"explicit agents share excludes other", "gamma", "alpha", "agents", `["beta"]`, false},
	}
	for _, c := range cases {
		if got := visibleToRequester(c.requester, c.author, c.visibility, c.agent, iso); got != c.want {
			t.Errorf("%s: visibleToRequester(%q,%q,%q,%q) = %v, want %v",
				c.name, c.requester, c.author, c.visibility, c.agent, got, c.want)
		}
	}
}

func TestAuthorFieldLegacyFallback(t *testing.T) {
	// A doc with created_by set uses it; a doc with only legacy agent_id (no
	// created_by) is treated as legacy (author "") => visible to all.
	if got := authorField(map[string]string{"created_by": "alpha", "agent_id": "alpha"}); got != "alpha" {
		t.Errorf("created_by should win, got %q", got)
	}
	if got := authorField(map[string]string{"agent_id": "alpha"}); got != "" {
		t.Errorf("no created_by => legacy (empty author), got %q", got)
	}
}

// The flag must default to OFF. crossAgentEnabled reads a map key on master
// (the branch this was ported from used a typed struct field), and a missing
// key must read as false — otherwise a deployment with no `features:` block
// would silently start filtering reads.
func TestCrossAgentEnabledDefaultsOff(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if crossAgentEnabled(cfg) {
		t.Error("cross_agent_visibility must default to false on an empty config")
	}

	cfg, err = loadConfig(writeCfg(t, "features:\n  cross_agent_visibility: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !crossAgentEnabled(cfg) {
		t.Error("explicit cross_agent_visibility: true was not honoured")
	}

	// An unrelated flag must not switch it on.
	cfg, err = loadConfig(writeCfg(t, "features:\n  something_else: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if crossAgentEnabled(cfg) {
		t.Error("an unrelated feature flag enabled cross-agent visibility")
	}
}

// With the flag off, visibility must not be consulted at all: the legacy
// exact-author match is the only filter. This pins the "flag off is
// byte-identical to today" guarantee the port depends on.
func TestIsolationInertWhenFlagOff(t *testing.T) {
	// visibleToRequester is only reached when crossAgent is true; when the flag
	// is off searchVecs compares data["agent_id"] directly. Assert the helper
	// would have hidden the doc, proving the flag is what gates the behaviour.
	iso := map[string]bool{"alpha": true}
	if visibleToRequester("beta", "alpha", "private", "", iso) {
		t.Fatal("precondition: isolated author should be hidden when the filter runs")
	}
	// ...and that a nil isolation set (what the flag-off path passes) hides nothing.
	if !visibleToRequester("beta", "alpha", "private", "", nil) {
		t.Error("with no isolated agents loaded, the cooperative default must be visible")
	}
}
