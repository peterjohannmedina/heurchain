package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/redis/go-redis/v9"
)

// sequentialVaultStats is the pre-pipelining implementation, kept here as the
// oracle: sequential round trips per key (tombstone EXISTS, vault EXISTS in
// pass 2, HGETALL) and the same SCAN/dedup/prefer-vault semantics.
func sequentialVaultStats(t *testing.T, rdb *redis.Client, tenantID string) (vectors, bytesTotal, tokenChars int64) {
	t.Helper()
	ctx := context.Background()
	vaultPrefix := "tenant:" + tenantID + ":vault:"
	vecPrefix := "tenant:" + tenantID + ":vec:"
	seen := seenIDs{}

	accumulate := func(id, key string) {
		if forgotten, err := memoryForgotten(ctx, rdb, tenantID, id); err != nil || forgotten {
			return
		}
		data, err := rdb.HGetAll(ctx, key).Result()
		if err != nil || len(data) == 0 {
			return
		}
		vectors++
		for field, val := range data {
			bytesTotal += int64(len(field)) + int64(len(val))
			if field == "metadata" || field == "text" || field == "cue" {
				tokenChars += int64(len(val))
			}
		}
	}

	var cursor uint64
	for {
		keys, next, err := rdb.Scan(ctx, cursor, vaultPrefix+"*", 200).Result()
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			if !seen.add(k[len(vaultPrefix):]) {
				continue
			}
			accumulate(k[len(vaultPrefix):], k)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	cursor = 0
	for {
		keys, next, err := rdb.Scan(ctx, cursor, vecPrefix+"*", 200).Result()
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			docID := k[len(vecPrefix):]
			if exists, _ := rdb.Exists(ctx, vaultPrefix+docID).Result(); exists > 0 {
				continue
			}
			if !seen.add(docID) {
				continue
			}
			accumulate(docID, k)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return vectors, bytesTotal, tokenChars
}

func TestVaultStatsPipelinedTotalsMatchSequential(t *testing.T) {
	s := newExportTestServer(t)
	ctx := context.Background()

	// A large embedding is the field the old HGETALL transferred and the new
	// HSTRLEN path must not need. Its length still counts towards bytes_total.
	bigEmbedding := "["
	for i := 0; i < 4096; i++ {
		if i > 0 {
			bigEmbedding += ","
		}
		bigEmbedding += "0.123456"
	}
	bigEmbedding += "]"

	seed := []struct {
		key    string
		fields map[string]interface{}
	}{
		{memoryVaultKey("t1", "vaulted"), map[string]interface{}{
			"embedding":  bigEmbedding,
			"metadata":   `{"text":"vaulted body ✓","tags":["x"]}`,
			"text":       "vaulted body ✓",
			"cue":        "a short cue",
			"tags":       `["x"]`,
			"created_at": "2026-01-01T00:00:00Z",
		}},
		{vecKey("t1", "hot"), map[string]interface{}{
			"embedding": bigEmbedding,
			"metadata":  `{"text":"hot body"}`,
			"text":      "hot body",
			"namespace": "notes",
		}},
		// Present in both tiers: pass 1 counts the vault copy; pass 2 must not
		// count the vec copy as well.
		{memoryVaultKey("t1", "both"), map[string]interface{}{
			"embedding": "[1,2]",
			"metadata":  `{"text":"vault copy wins"}`,
			"text":      "vault copy wins",
		}},
		{vecKey("t1", "both"), map[string]interface{}{
			"embedding": "[3,4]",
			"metadata":  `{"text":"stale working copy"}`,
			"text":      "stale working copy",
		}},
		// Tombstoned residue in both tiers: hidden and uncounted wherever it sits.
		{vecKey("t1", "forgotten"), map[string]interface{}{
			"embedding": "[9,9]",
			"metadata":  `{"text":"should not count"}`,
			"text":      "should not count",
		}},
		{memoryVaultKey("t1", "forgotten-vault"), map[string]interface{}{
			"embedding": "[8,8]",
			"metadata":  `{"text":"vault residue"}`,
			"text":      "vault residue",
		}},
		// Enough keys to require at least two SCAN pages at the production page
		// size. miniredis ignores COUNT and returns the whole match set from one
		// SCAN call, so the page *boundary* cannot be exercised here; the totals
		// still have to match the oracle over the full set.
	}
	for i := 0; i < 240; i++ {
		seed = append(seed, struct {
			key    string
			fields map[string]interface{}
		}{vecKey("t1", fmt.Sprintf("bulk-%03d", i)), map[string]interface{}{
			"embedding": "[0.1,0.2]",
			"text":      fmt.Sprintf("bulk %d", i),
		}})
	}
	for _, row := range seed {
		if err := s.rdb.HSet(ctx, row.key, row.fields).Err(); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"forgotten", "forgotten-vault"} {
		if err := s.rdb.Set(ctx, memoryTombstoneKey("t1", id), `{"id":"`+id+`"}`, 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	// A non-hash key under the vec prefix: WRONGTYPE must be skipped, not fail
	// the page. (Redis does not persist an empty hash — deleting the last field
	// removes the key — so "missing" is the empty-hash case; the script test
	// below covers it directly.)
	if err := s.rdb.Set(ctx, vecKey("t1", "stringy"), "not a hash", 0).Err(); err != nil {
		t.Fatal(err)
	}

	wantVectors, wantBytes, wantChars := sequentialVaultStats(t, s.rdb, "t1")

	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASS", "hunter2")
	req := httptest.NewRequest(http.MethodGet, "/vault/stats?tenant_id=t1", nil)
	req.SetBasicAuth("admin", "hunter2")
	rec := httptest.NewRecorder()
	s.handleVaultStats(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var got struct {
		VectorsTotal   int64 `json:"vectors_total"`
		BytesTotal     int64 `json:"bytes_total"`
		TokensEstimate int64 `json:"tokens_estimate"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	if got.VectorsTotal != wantVectors {
		t.Errorf("vectors_total = %d, want %d (the sequential oracle)", got.VectorsTotal, wantVectors)
	}
	if got.BytesTotal != wantBytes {
		t.Errorf("bytes_total = %d, want %d (the sequential oracle)", got.BytesTotal, wantBytes)
	}
	if got.TokensEstimate != wantChars/4 {
		t.Errorf("tokens_estimate = %d, want %d", got.TokensEstimate, wantChars/4)
	}
	// vaulted + both (vault copy) + hot + 240 bulk = 243.
	if wantVectors != 243 {
		t.Errorf("fixture sanity: sequential oracle counted %d vectors, want 243", wantVectors)
	}
}

// TestVaultStatsScriptSkipsMissingAndNonHashKeys checks the per-key contract
// directly. Deleting a hash mid-computation cannot be interleaved from a test:
// Redis executes the script atomically, which is the property being relied on,
// and miniredis is single-threaded too. A key that is already gone when the
// script starts is the observable version of that race, and it must report
// exists=0 rather than a partial byte count.
func TestVaultStatsScriptSkipsMissingAndNonHashKeys(t *testing.T) {
	s := newExportTestServer(t)
	ctx := context.Background()

	hashKey := vecKey("t1", "hashed")
	if err := s.rdb.HSet(ctx, hashKey, map[string]interface{}{
		"text":      "abc",
		"metadata":  "xy",
		"embedding": "[1,2,3]",
	}).Err(); err != nil {
		t.Fatal(err)
	}
	stringKey := vecKey("t1", "stringy")
	if err := s.rdb.Set(ctx, stringKey, "not a hash", 0).Err(); err != nil {
		t.Fatal(err)
	}
	missingKey := vecKey("t1", "missing")

	keys := []string{hashKey, stringKey, missingKey}
	args := []interface{}{
		memoryTombstoneKey("t1", "hashed"), "",
		memoryTombstoneKey("t1", "stringy"), "",
		memoryTombstoneKey("t1", "missing"), "",
	}
	values, err := vaultStatsScript.Run(ctx, s.rdb, keys, args...).Int64Slice()
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	if len(values) != 15 {
		t.Fatalf("script returned %d values, want 15", len(values))
	}

	// text:4+3, metadata:8+2, embedding:9+7 → 33 bytes; human fields 3+2=5.
	if got := values[0:5]; got[0] != 1 || got[1] != 33 || got[2] != 5 || got[3] != 0 || got[4] != 0 {
		t.Errorf("hash row = %v, want [1 33 5 0 0]", got)
	}
	if got := values[5:10]; got[0] != 0 || got[1] != 0 || got[2] != 0 {
		t.Errorf("non-hash row = %v, want exists/bytes/chars all zero", got)
	}
	if got := values[10:15]; got[0] != 0 || got[1] != 0 || got[2] != 0 {
		t.Errorf("missing row = %v, want exists/bytes/chars all zero", got)
	}
}
