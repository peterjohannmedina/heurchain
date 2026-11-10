package main

import (
	"strings"
	"testing"
)

// TestValidIDRejectsOverlongIdentifiers guards the fix for the unbounded-docID
// amplification: validID constrained the character class but not the length, so
// a ~50 KB id inside a 512 KB /store body expanded into gigabytes of Redis key
// names (the id is embedded in a key name once per unique term) built in broker
// heap before Exec, killing the process with an unrecoverable OOM.
func TestValidIDRejectsOverlongIdentifiers(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want bool
	}{
		{"typical uuid", "550e8400-e29b-41d4-a716-446655440000", true},
		{"aggregator style", "agg-sha1-" + strings.Repeat("a", 40), true},
		{"exactly at the cap", strings.Repeat("a", maxIdentifierLength), true},
		{"one over the cap", strings.Repeat("a", maxIdentifierLength+1), false},
		{"amplification payload", strings.Repeat("a", 50*1024), false},
		{"empty", "", false},
		{"bad charset", "has spaces", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validID.MatchString(tc.id); got != tc.want {
				t.Fatalf("validID.MatchString(len=%d) = %v, want %v", len(tc.id), got, tc.want)
			}
		})
	}
}

// TestCapTermFreqsBoundsPipelineSize guards the second half of the same fix:
// even a short docID is dangerous if the term count is unbounded, because each
// indexed term costs three queued Redis commands.
func TestCapTermFreqsBoundsPipelineSize(t *testing.T) {
	// Under the cap: returned untouched, so ordinary documents score exactly
	// as they did before the change.
	small := map[string]int{"alpha": 3, "beta": 1}
	if got := capTermFreqs(small, maxTermsPerDoc); len(got) != 2 {
		t.Fatalf("small doc: got %d terms, want 2", len(got))
	}

	oversized := make(map[string]int, 10000)
	for i := 0; i < 10000; i++ {
		oversized[strings.Repeat("x", 3)+string(rune('a'+i%26))+itoa(i)] = i % 7
	}
	capped := capTermFreqs(oversized, maxTermsPerDoc)
	if len(capped) != maxTermsPerDoc {
		t.Fatalf("oversized doc: got %d terms, want %d", len(capped), maxTermsPerDoc)
	}

	// Retention must be by frequency, not map order, or scoring quality would
	// vary run to run for the same document.
	freqRanked := map[string]int{"rare": 1, "common": 500, "mid": 50}
	top := capTermFreqs(freqRanked, 2)
	if _, ok := top["common"]; !ok {
		t.Error("most frequent term was dropped")
	}
	if _, ok := top["rare"]; ok {
		t.Error("least frequent term was retained over a more frequent one")
	}
}

// TestCapTermFreqsIsDeterministic ensures the retained set does not depend on
// Go's randomised map iteration order.
func TestCapTermFreqsIsDeterministic(t *testing.T) {
	in := map[string]int{}
	for i := 0; i < 200; i++ {
		in["term"+itoa(i)] = i % 5 // many ties, so the tiebreak matters
	}
	first := capTermFreqs(in, 20)
	for i := 0; i < 20; i++ {
		next := capTermFreqs(in, 20)
		if len(next) != len(first) {
			t.Fatalf("run %d: size changed", i)
		}
		for term := range first {
			if _, ok := next[term]; !ok {
				t.Fatalf("run %d: retained set differs (missing %q)", i, term)
			}
		}
	}
}

// TestDedupeQueryTerms guards the /search fix: scoreBM25 issues one Get per
// (doc, term), so repeated or unbounded query terms multiply a full-corpus scan
// while adding no signal.
func TestDedupeQueryTerms(t *testing.T) {
	got := dedupeQueryTerms([]string{"a", "b", "a", "c", "b"}, maxQueryTerms)
	if len(got) != 3 {
		t.Fatalf("dedupe: got %d terms %v, want 3", len(got), got)
	}
	// Order of first appearance is preserved.
	if got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("dedupe did not preserve first-appearance order: %v", got)
	}

	long := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		long = append(long, "t"+itoa(i))
	}
	if capped := dedupeQueryTerms(long, maxQueryTerms); len(capped) != maxQueryTerms {
		t.Fatalf("cap: got %d terms, want %d", len(capped), maxQueryTerms)
	}
}

// itoa avoids pulling strconv into the test's import set for one call.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
