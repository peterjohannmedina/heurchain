package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A traversal that outlives the read budget must fail with the deadline error,
// which recall reports as graph_status "failed" and the ask endpoint as 503.
func TestGraphReadBudgetExpires(t *testing.T) {
	f := graphFixture(t)
	f.store(t, "doc", "evidence", "a", nil, 1)
	graphTestEdge(t, f, "a00", "alpha", "hub00", "doc")
	saved := graphReadBudget
	// A non-positive budget makes context.WithTimeout cancel at creation. A
	// tiny positive one (1ns) only arms a timer, and on coarse-clock hosts
	// (Windows) a one-edge walk can finish before it fires, returning nil.
	graphReadBudget = -time.Second
	t.Cleanup(func() { graphReadBudget = saved })
	_, err := traverseGraph(context.Background(), f.s.rdb, f.s.db, "t1", "alpha", "", nil, RecallOptions{GraphHops: 2}, nil, 100)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}
