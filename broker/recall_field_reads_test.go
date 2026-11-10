package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
)

// commandRecorder captures every command the client sends, pipelined or not,
// so a test can assert which hash fields a read path touches.
type commandRecorder struct {
	mu   sync.Mutex
	cmds []redis.Cmder
}

func (r *commandRecorder) DialHook(next redis.DialHook) redis.DialHook { return next }

func (r *commandRecorder) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		r.add(cmd)
		return next(ctx, cmd)
	}
}

func (r *commandRecorder) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		r.add(cmds...)
		return next(ctx, cmds)
	}
}

func (r *commandRecorder) add(cmds ...redis.Cmder) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, cmds...)
}

// vecHashReads summarises the reads against one tenant's vec hashes: how many
// were HGETALL, and which fields HMGET or HGET requested.
func (r *commandRecorder) vecHashReads(tenant string) (hgetall int, fields map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fields = map[string]int{}
	prefix := vecKey(tenant, "")
	for _, cmd := range r.cmds {
		args := cmd.Args()
		if len(args) < 2 {
			continue
		}
		if key, _ := args[1].(string); !strings.HasPrefix(key, prefix) {
			continue
		}
		switch cmd.Name() {
		case "hgetall":
			hgetall++
		case "hmget", "hget":
			for _, arg := range args[2:] {
				if field, ok := arg.(string); ok {
					fields[field]++
				}
			}
		}
	}
	return hgetall, fields
}

// seedFilterCorpus stores three memories that differ in agent, tags,
// namespace, metadata and timestamps, each with a full-size embedding.
func seedFilterCorpus(t *testing.T, rdb *redis.Client) {
	t.Helper()
	ctx := context.Background()
	embedding := make([]float32, 768)
	for i := range embedding {
		embedding[i] = float32(i%7) / 7
	}
	docs := []struct {
		id, agent, namespace, metadata, at string
		tags                               []string
	}{
		{"a1", "alpha", "ops", `{"project":"x","memory_kind":"factual","valid_from":"2026-01-01T00:00:00Z"}`, "2026-02-01T00:00:00Z", []string{"ops"}},
		{"b1", "beta", "notes", `{"project":"y","memory_kind":"episodic"}`, "2026-03-01T00:00:00Z", []string{"notes"}},
		{"a2", "alpha", "notes", `{"project":"x"}`, "2026-04-01T00:00:00Z", []string{"notes"}},
	}
	for _, d := range docs {
		if err := storeVec(ctx, rdb, "t1", storedVector{ID: d.id, AgentID: d.agent, Tags: d.tags, Namespace: d.namespace, Metadata: json.RawMessage(d.metadata), Text: "evidence " + d.id, CreatedAt: d.at, Embedding: embedding}); err != nil {
			t.Fatal(err)
		}
		// storeMemoryFields stamps updated_at from the wall clock; pin it so
		// the updated-window cases are deterministic.
		if err := rdb.HSet(ctx, vecKey("t1", d.id), "updated_at", d.at).Err(); err != nil {
			t.Fatal(err)
		}
	}
}

// hgetallEligible is the pre-change eligibility read, one HGETALL per
// document. The field-scoped read must select exactly the same documents.
func hgetallEligible(ctx context.Context, rdb *redis.Client, ids []string, agent string, tags []string, scope RecallOptions) []string {
	var out []string
	for _, id := range ids {
		data := rdb.HGetAll(ctx, vecKey("t1", id)).Val()
		if (agent == "" || data["agent_id"] == agent) && tagsIntersect(parseTags(data["tags"]), tags) && matchesMemoryContext(ctx, id, data) && recallMetadataEligible(data["metadata"], scope) {
			out = append(out, id)
		}
	}
	return out
}

func assertFilterFieldReads(t *testing.T, recorder *commandRecorder) {
	t.Helper()
	hgetall, fields := recorder.vecHashReads("t1")
	if hgetall != 0 {
		t.Fatalf("filtered recall issued %d HGETALL reads", hgetall)
	}
	if fields["embedding"] != 0 || fields["text"] != 0 {
		t.Fatalf("filtered recall read bulky fields: %v", fields)
	}
	if fields["agent_id"] == 0 || fields["metadata"] == 0 {
		t.Fatalf("filtered recall did not read the filter fields: %v", fields)
	}
}

func TestEligibleRecallIDsReadOnlyFilterFields(t *testing.T) {
	rdb := newTestRedis(t)
	seedFilterCorpus(t, rdb)
	plain := redis.NewClient(&redis.Options{Addr: rdb.Options().Addr})
	t.Cleanup(func() { _ = plain.Close() })
	recorder := &commandRecorder{}
	rdb.AddHook(recorder)

	ids := []string{"a1", "b1", "a2", "missing"}
	cases := []struct {
		name    string
		agent   string
		tags    []string
		scope   RecallOptions
		filters *memoryFilters
		want    string
	}{
		{name: "agent", agent: "alpha", want: "a1,a2"},
		{name: "tags", tags: []string{"notes"}, want: "b1,a2"},
		{name: "memory_kind", scope: RecallOptions{MemoryKind: "factual"}, want: "a1"},
		{name: "as_of before validity", scope: RecallOptions{AsOf: "2025-12-01T00:00:00Z"}, want: "b1,a2"},
		{name: "namespace and metadata", filters: &memoryFilters{Namespace: "notes", Metadata: map[string]interface{}{"project": "x"}}, want: "a2"},
		{name: "created window", filters: &memoryFilters{CreatedAfter: "2026-02-15T00:00:00Z"}, want: "b1,a2"},
		{name: "updated window", filters: &memoryFilters{UpdatedBefore: "2026-02-15T00:00:00Z"}, want: "a1"},
		{name: "combined", agent: "alpha", tags: []string{"ops"}, filters: &memoryFilters{Namespace: "ops"}, want: "a1"},
	}
	for _, tc := range cases {
		ctx := context.Background()
		if tc.filters != nil {
			ctx = context.WithValue(ctx, memoryFilterContextKey{}, tc.filters)
		}
		got, err := eligibleRecallIDs(ctx, rdb, "t1", ids, tc.agent, tc.tags, tc.scope)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if joined := strings.Join(got, ","); joined != tc.want {
			t.Errorf("%s: eligible %q, want %q", tc.name, joined, tc.want)
		}
		if legacy := strings.Join(hgetallEligible(ctx, plain, ids, tc.agent, tc.tags, tc.scope), ","); legacy != strings.Join(got, ",") {
			t.Errorf("%s: field-scoped read selected %v, HGETALL read selects %q", tc.name, got, legacy)
		}
	}
	assertFilterFieldReads(t, recorder)
}

func TestFilterBM25ActiveDocsStructuredFiltersReadOnlyFilterFields(t *testing.T) {
	rdb := newTestRedis(t)
	seedFilterCorpus(t, rdb)
	recorder := &commandRecorder{}
	rdb.AddHook(recorder)

	page := []string{"a1", "b1", "a2", "missing", "a1"}
	ctx := context.WithValue(context.Background(), recallScopeContextKey{}, RecallOptions{MemoryKind: "factual"})
	got, err := filterBM25ActiveDocs(ctx, rdb, "t1", "", nil, page)
	if err != nil || strings.Join(got, ",") != "a1" {
		t.Fatalf("memory_kind scope: %v %v", got, err)
	}
	ctx = context.WithValue(context.Background(), memoryFilterContextKey{}, &memoryFilters{Metadata: map[string]interface{}{"project": "x"}, CreatedAfter: "2026-02-15T00:00:00Z"})
	got, err = filterBM25ActiveDocs(ctx, rdb, "t1", "alpha", []string{"notes"}, page)
	if err != nil || strings.Join(got, ",") != "a2" {
		t.Fatalf("structured filters with agent and tags: %v %v", got, err)
	}
	assertFilterFieldReads(t, recorder)
}

func TestMemoryFilterFieldsCoverMatches(t *testing.T) {
	// Every filter dimension must be served by memoryFilterFields. A new
	// memoryFilters field needs its hash fields added there and a case below.
	known := map[string]bool{"AgentID": true, "Tags": true, "Namespace": true, "Status": true, "Metadata": true, "CreatedAfter": true, "CreatedBefore": true, "UpdatedAfter": true, "UpdatedBefore": true}
	typ := reflect.TypeOf(memoryFilters{})
	for i := 0; i < typ.NumField(); i++ {
		if !known[typ.Field(i).Name] {
			t.Fatalf("memoryFilters.%s is not covered by memoryFilterFields; extend the field list and this test", typ.Field(i).Name)
		}
	}
	full := map[string]string{
		"agent_id": "alpha", "tags": `["ops"]`, "namespace": "ops", "metadata": `{"project":"x"}`,
		"created_at": "2026-02-01T00:00:00Z", "updated_at": "2026-02-02T00:00:00Z",
		"text": "evidence", "embedding": "[0.1,0.2]", "revision": "3", "change_reason": "edit",
		"access_count": "4", "embedding_model": "nomic", "index_revision": "r1", "indexed_revision": "r1",
	}
	projected := map[string]string{}
	for _, field := range memoryFilterFields {
		if value, ok := full[field]; ok {
			projected[field] = value
		}
	}
	// Pairs: one matching, one rejecting case per dimension.
	filters := []*memoryFilters{
		{AgentID: "alpha"}, {AgentID: "beta"},
		{Tags: []string{"ops"}}, {Tags: []string{"other"}},
		{Namespace: "ops"}, {Namespace: "notes"},
		{Status: "active"}, {Status: "archived"},
		{Metadata: map[string]interface{}{"project": "x"}}, {Metadata: map[string]interface{}{"project": "y"}},
		{CreatedAfter: "2026-01-01T00:00:00Z"}, {CreatedAfter: "2026-03-01T00:00:00Z"},
		{CreatedBefore: "2026-03-01T00:00:00Z"}, {CreatedBefore: "2026-01-01T00:00:00Z"},
		{UpdatedAfter: "2026-02-01T12:00:00Z"}, {UpdatedAfter: "2026-03-01T00:00:00Z"},
		{UpdatedBefore: "2026-03-01T00:00:00Z"}, {UpdatedBefore: "2026-02-01T12:00:00Z"},
	}
	for i, f := range filters {
		ctx := context.WithValue(context.Background(), memoryFilterContextKey{}, f)
		want := i%2 == 0
		if got := matchesMemoryContext(ctx, "doc", full); got != want {
			t.Errorf("filter %+v on the full hash: %v, want %v", *f, got, want)
		}
		if got := matchesMemoryContext(ctx, "doc", projected); got != want {
			t.Errorf("filter %+v on the memoryFilterFields projection: %v, want %v", *f, got, want)
		}
	}
}
