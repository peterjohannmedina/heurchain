package main

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestSessionContextResolvesCorrectionsAndOmitsForgottenMemory(t *testing.T) {
	f := newRecallFixture(t)
	ctx := context.Background()
	sid, err := startSession(ctx, f.s.rdb, "t1", "codex", 3600)
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
			sid, err := startSession(ctx, f.s.rdb, "t1", "codex", 3600)
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
			ids, err := agentSessions(ctx, f.s.rdb, "t1", "codex")
			if err != nil || len(ids) != 0 {
				t.Fatalf("invalidated session was listed: %v, %v", ids, err)
			}
			if action != "expired" {
				if exists, _ := f.s.rdb.Exists(ctx, sessMemoryCtxKey("t1", sid)).Result(); exists != 0 {
					t.Fatal("invalidated session retained context references")
				}
			}
		})
	}
}

func TestSessionListTracksSlidingExpiry(t *testing.T) {
	rdb := newTestRedis(t)
	ctx := context.Background()
	const ttl = 48 * time.Hour
	long, err := startSession(ctx, rdb, "t1", "codex", int(ttl/time.Second))
	if err != nil {
		t.Fatal(err)
	}
	short, err := startSession(ctx, rdb, "t1", "codex", 0)
	if err != nil {
		t.Fatal(err)
	}
	agentKey := agentSessSetKey("t1", "codex")
	longKey := sessMetaKey("t1", long)
	shortKey := sessMetaKey("t1", short)
	checkTTL := func(key string, want time.Duration) {
		t.Helper()
		got, err := rdb.TTL(ctx, key).Result()
		if err != nil || got != want {
			t.Fatalf("TTL for %s = %s, %v; want %s", key, got, err, want)
		}
	}
	checkTTL(shortKey, time.Hour)
	checkTTL(agentKey, ttl)
	if err := rdb.Expire(ctx, longKey, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.Expire(ctx, agentKey, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := appendMemoryContext(ctx, rdb, "t1", long, "summary"); err != nil {
		t.Fatal(err)
	}
	checkTTL(longKey, ttl)
	checkTTL(sessMemoryCtxKey("t1", long), ttl)
	checkTTL(agentKey, ttl)
}
