package main

import (
	"context"
	"reflect"
	"testing"
)

func TestSessionContextResolvesCorrectionsAndOmitsForgottenMemory(t *testing.T) {
	f := newRecallFixture(t)
	ctx := context.Background()
	sid, err := startSession(ctx, f.s.rdb, "t1", "codex", 3600, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := lifecycleCall(t, f, f.s.handleStore, map[string]interface{}{
		"id": "session-memory", "text": "old preference", "agent_id": "codex",
		"metadata": map[string]interface{}{"session_id": sid},
	})
	if w.Code != 200 {
		t.Fatalf("store: %d %s", w.Code, w.Body)
	}
	assertContext := func(want []string) {
		t.Helper()
		got, err := getContext(ctx, f.s.rdb, "t1", sid, 20)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("context = %v, %v; want %v", got, err, want)
		}
	}
	assertContext([]string{"old preference"})
	w = lifecycleCall(t, f, f.s.handleMemoryUpdate, map[string]interface{}{
		"id": "session-memory", "expected_revision": 1, "text": "current preference", "reason": "correction",
	})
	if w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	assertContext([]string{"current preference"})
	w = lifecycleCall(t, f, f.s.handleMemoryForget, map[string]interface{}{
		"id": "session-memory", "expected_revision": 2, "confirm": true,
	})
	if w.Code != 200 {
		t.Fatalf("forget: %d %s", w.Code, w.Body)
	}
	assertContext([]string{})
	refs, _ := f.s.rdb.LRange(ctx, sessMemoryCtxKey("t1", sid), 0, -1).Result()
	if !reflect.DeepEqual(refs, []string{"session-memory"}) {
		t.Fatalf("session retains content instead of only an ID: %v", refs)
	}
}

func TestSessionContextDoesNotRecreateMissingSession(t *testing.T) {
	rdb := newTestRedis(t)
	if err := appendMemoryContext(context.Background(), rdb, "t1", "expired-session", "memory-id"); err != nil {
		t.Fatal(err)
	}
	keys, err := rdb.Keys(context.Background(), "tenant:t1:sess:expired-session:*").Result()
	if err != nil || len(keys) != 0 {
		t.Fatalf("orphan session keys: %v, %v", keys, err)
	}
}

func TestSessionContextStopsAtSessionInvalidation(t *testing.T) {
	for _, action := range []string{"end", "expired"} {
		t.Run(action, func(t *testing.T) {
			f := newRecallFixture(t)
			ctx := context.Background()
			f.store(t, "session-memory", "private context", "codex", nil, 1)
			sid, err := startSession(ctx, f.s.rdb, "t1", "codex", 3600, &sessionBinding{})
			if err != nil {
				t.Fatal(err)
			}
			if err := appendMemoryContext(ctx, f.s.rdb, "t1", sid, "session-memory"); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "end":
				if err := endSession(ctx, f.s.rdb, "t1", sid); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if err := f.s.rdb.Del(ctx, sessMetaKey("t1", sid)).Err(); err != nil {
					t.Fatal(err)
				}
			}
			items, err := getContext(ctx, f.s.rdb, "t1", sid, 20)
			if err != nil || len(items) != 0 {
				t.Fatalf("invalidated session returned context: %v, %v", items, err)
			}
			if action != "expired" {
				if exists, _ := f.s.rdb.Exists(ctx, sessMemoryCtxKey("t1", sid)).Result(); exists != 0 {
					t.Fatal("invalidated session retained context references")
				}
			}
		})
	}
}
