package main

import (
	"sort"
	"strings"
	"unicode"
)

const maxRecallIdentifiers = 8

// Identifiers contain both letters and digits. Preserve internal '-', '_' and
// '.' rather than matching their fragments. Ordinary words and numeric dates
// do not activate this opt-in feature. Matching is case-insensitive, with whole
// token boundaries; a query for RX-12 must not promote RX-123 or RX-12.extra.
// Token buffers are bounded even when an input contains a very long word.
func visitRecallIdentifiers(text string, visit func(string)) {
	var token []rune
	overflow := false
	flush := func() {
		if !overflow {
			s := strings.Trim(string(token), "-_.")
			letter, digit := false, false
			for _, r := range s {
				letter = letter || unicode.IsLetter(r)
				digit = digit || unicode.IsDigit(r)
			}
			if len(s) >= 3 && letter && digit {
				visit(strings.ToLower(s))
			}
		}
		token = token[:0]
		overflow = false
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			if len(token) < 128 {
				token = append(token, r)
			} else {
				overflow = true
			}
		} else {
			flush()
		}
	}
	flush()
}

func recallQueryIdentifiers(text string) (map[string]bool, string) {
	ids := map[string]bool{}
	visitRecallIdentifiers(text, func(id string) {
		if len(ids) <= maxRecallIdentifiers {
			ids[id] = true
		}
	})
	if len(ids) > maxRecallIdentifiers {
		return nil, "too_many_identifiers"
	}
	if len(ids) == 0 {
		return nil, "no_identifiers"
	}
	return ids, "applied"
}

// Promote exact matches only among the already eligible, hydrated candidate
// pool. Stable ties preserve RRF order, including when no identifier matches.
// This neither adds candidates nor filters out semantic matches. RRF scores
// remain the original fusion scores, not purported calibrated probabilities.
func rerankRecallIdentifiers(results []SearchResult, ids map[string]bool) {
	for i := range results {
		seen := map[string]bool{}
		visitRecallIdentifiers(results[i].Text, func(id string) {
			if ids[id] {
				seen[id] = true
			}
		})
		count := len(seen)
		results[i].IdentifierMatches = &count
	}
	sort.SliceStable(results, func(i, j int) bool {
		return *results[i].IdentifierMatches > *results[j].IdentifierMatches
	})
}
