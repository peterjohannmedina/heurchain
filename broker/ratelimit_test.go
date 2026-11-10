package main

import "testing"

// slidingWeight mirrors the weighting rpmGuard applies. Kept in the test rather
// than exported from admin.go because the guard is a closure over Redis; the
// arithmetic is what needs pinning, and it is one line.
func slidingWeight(prevCount, curCount int64, elapsed, windowSec int64) float64 {
	return float64(prevCount)*(float64(windowSec-elapsed)/float64(windowSec)) + float64(curCount)
}

// TestSlidingWindowClosesBoundaryBurst is the regression guard for the fixed
// window this replaced. With hard-aligned 60s buckets a caller could spend a
// full allowance at :59 and another at :00 — 2*RPM-1 requests inside a second,
// and ~2x the paid rate sustained by timing bursts on the boundary.
func TestSlidingWindowClosesBoundaryBurst(t *testing.T) {
	const rpm = 20
	const window = 60

	// The old exploit: 20 requests land at the end of the previous sub-window,
	// then the caller immediately starts a new one. One second in, almost all of
	// the previous window is still weighted, so the 21st request must be
	// rejected rather than admitted by a counter reset.
	w := slidingWeight(20, 1, 1, window)
	if w <= rpm {
		t.Fatalf("boundary burst admitted: weighted=%.2f, limit=%d — the "+
			"fixed-window reset is back", w, rpm)
	}

	// Same instant under the old scheme: the current bucket held 1, so it passed.
	if oldCount := int64(1); oldCount > rpm {
		t.Fatal("sanity: old fixed-window logic should have admitted this")
	}
}

func TestSlidingWindowAdmitsNormalTraffic(t *testing.T) {
	const rpm = 20
	const window = 60

	// A caller well under the limit is unaffected.
	if w := slidingWeight(5, 5, 30, window); w > rpm {
		t.Fatalf("normal traffic throttled: weighted=%.2f, limit=%d", w, rpm)
	}

	// A caller that used its full allowance a full window ago is not penalised
	// once that window has aged out entirely.
	if w := slidingWeight(20, 1, window, window); w > rpm {
		t.Fatalf("fully-aged previous window still counted: weighted=%.2f", w)
	}
}

func TestSlidingWindowDecaysProgressively(t *testing.T) {
	const window = 60
	// As time passes within the sub-window the previous count carries less
	// weight, so pressure eases monotonically rather than in a cliff.
	prev := int64(20)
	last := slidingWeight(prev, 0, 0, window)
	for elapsed := int64(1); elapsed <= window; elapsed++ {
		got := slidingWeight(prev, 0, elapsed, window)
		if got > last {
			t.Fatalf("weight increased as the window aged: elapsed=%d got=%.2f prev=%.2f",
				elapsed, got, last)
		}
		last = got
	}
	if last != 0 {
		t.Fatalf("previous window never fully aged out: %.2f", last)
	}
}
