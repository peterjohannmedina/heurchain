package main

import "sync/atomic"

// consolidationMetrics is intentionally process-local. It provides operators
// useful evidence through /vault/stats without introducing a new metrics
// backend; counters reset on broker restart and should be scraped alongside
// the broker release/health identity.
var consolidationMetrics struct {
	rankScanned           atomic.Uint64
	rankIncomplete        atomic.Uint64
	dryRunSelected        atomic.Uint64
	promotions            atomic.Uint64
	promotionFailures     atomic.Uint64
	qdrantDeleteFailures  atomic.Uint64
	rehydrations          atomic.Uint64
	rehydrationFailures   atomic.Uint64
	rehydrationConflicts  atomic.Uint64
	rehydrationDurationMS atomic.Uint64
}

func consolidationMetricsSnapshot() map[string]uint64 {
	return map[string]uint64{
		"rank_scanned":            consolidationMetrics.rankScanned.Load(),
		"rank_incomplete":         consolidationMetrics.rankIncomplete.Load(),
		"dry_run_selected":        consolidationMetrics.dryRunSelected.Load(),
		"promotions":              consolidationMetrics.promotions.Load(),
		"promotion_failures":      consolidationMetrics.promotionFailures.Load(),
		"qdrant_delete_failures":  consolidationMetrics.qdrantDeleteFailures.Load(),
		"rehydrations":            consolidationMetrics.rehydrations.Load(),
		"rehydration_failures":    consolidationMetrics.rehydrationFailures.Load(),
		"rehydration_conflicts":   consolidationMetrics.rehydrationConflicts.Load(),
		"rehydration_duration_ms": consolidationMetrics.rehydrationDurationMS.Load(),
	}
}
