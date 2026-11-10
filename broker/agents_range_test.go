package main

import "testing"

// The ?days= value is interpolated into a SQLite datetime modifier rather than
// bound (SQLite treats a bound modifier as a literal), so the allowlist is the
// only thing standing between a caller and the statement. These cases pin that.
func TestParseAgentDays(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		ok   bool
	}{
		{"", 7, true}, // absent -> historical default, existing callers unaffected
		{"1", 1, true},
		{"7", 7, true},
		{"30", 30, true},
		{"90", 90, true},
		{"365", 365, true},
		{"0", 0, true},   // all time
		{"14", 0, false}, // plausible but not allowlisted
		{"-1", 0, false},
		{"9999", 0, false},
		{"abc", 0, false},
		{"7; DROP TABLE usage_events", 0, false},
		{"7 days') OR 1=1 --", 0, false},
		{" 7", 0, false}, // Atoi rejects the space; no silent trimming
	} {
		got, err := parseAgentDays(tc.in)
		if tc.ok && err != nil {
			t.Errorf("parseAgentDays(%q) errored: %v", tc.in, err)
			continue
		}
		if !tc.ok {
			if err == nil {
				t.Errorf("parseAgentDays(%q) = %d, want rejection", tc.in, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("parseAgentDays(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Every allowlisted value must render a modifier SQLite accepts, and 0 must
// produce no clause at all rather than "-0 days".
func TestAgentDaysRenderSafely(t *testing.T) {
	for d := range agentDaysAllowed {
		if d < 0 {
			t.Errorf("allowlist contains negative window %d", d)
		}
	}
	if !agentDaysAllowed[7] {
		t.Error("7 must stay allowlisted — it is the default when ?days= is absent")
	}
	if !agentDaysAllowed[0] {
		t.Error("0 (all time) must be allowlisted")
	}
}
