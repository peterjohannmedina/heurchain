package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

func forgetMemory(ctx context.Context, rdb *redis.Client, tenant, id string, expected int64) (int64, error) {
	keys := []string{vecKey(tenant, id), memoryVaultKey(tenant, id), vecSetKey(tenant), memoryVersionsKey(tenant, id), memoryTombstoneKey(tenant, id), bm25DocTermsKey(tenant, id), bm25DocLenKey(tenant, id), bm25MetaDocCountKey(tenant), bm25MetaAvgDLKey(tenant), semanticPendingKey}
	const script = `
local forgotten = redis.call('GET', KEYS[5])
if forgotten then return tonumber(cjson.decode(forgotten).revision) end
local current = KEYS[1]
if redis.call('EXISTS', current) == 0 then current = KEYS[2] end
if redis.call('EXISTS', current) == 0 then return -3 end
local revision = tonumber(redis.call('HGET', current, 'revision') or '1')
if revision ~= tonumber(ARGV[2]) then return -1 end
local receipt = cjson.encode({id=ARGV[1], revision=revision, forgotten_at=ARGV[3]})
redis.call('SET', KEYS[5], receipt)
redis.call('DEL', KEYS[1], KEYS[2], KEYS[4])
redis.call('SREM', KEYS[3], ARGV[1])
local dl = tonumber(redis.call('GET', KEYS[7]) or '0')
if dl > 0 then
  local count = tonumber(redis.call('GET', KEYS[8]) or '0')
  local avg = tonumber(redis.call('GET', KEYS[9]) or '0')
  local new_count = math.max(count - 1, 0)
  local new_avg = 0
  if new_count > 0 then new_avg = math.max((avg * count - dl) / new_count, 0) end
  redis.call('SET', KEYS[8], new_count)
  redis.call('SET', KEYS[9], new_avg)
end
local terms = redis.call('SMEMBERS', KEYS[6])
for _, term in ipairs(terms) do
  local df = ARGV[6] .. term
  local count = tonumber(redis.call('GET', df) or '0')
  if count <= 1 then redis.call('DEL', df) else redis.call('DECR', df) end
  redis.call('DEL', ARGV[7] .. term)
  redis.call('ZREM', ARGV[8] .. term, ARGV[1])
end
redis.call('DEL', KEYS[6], KEYS[7])
redis.call('ZADD', KEYS[10], ARGV[4], ARGV[5])
return revision`
	revision, err := redis.NewScript(script).Run(ctx, rdb, keys, id, expected, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UnixMilli(), semanticMember(tenant, id), bm25DFPrefix(tenant), bm25TFPrefix(tenant, id), bm25PostingsPrefix(tenant)).Int64()
	if err != nil {
		return 0, err
	}
	if revision == -3 {
		return 0, errMemoryNotFound
	}
	if revision == -1 {
		return 0, errMemoryConflict
	}
	return revision, nil
}

// Graph extraction is opt-in, but existing graph rows/caches
// must not retain a deleted source. Clear affected caches and orphan entities.
func (s *Server) removeMemoryGraph(ctx context.Context, tenant, id string) error {
	return removeGraphSource(ctx, s.rdb, s.db, tenant, id)
}

func (s *Server) handleMemoryForget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID               string `json:"id"`
		ExpectedRevision int64  `json:"expected_revision"`
		Confirm          bool   `json:"confirm"`
	}
	tenant, key, ok := s.memoryRequest(w, r, &req)
	if !ok {
		return
	}
	if !validID.MatchString(req.ID) || req.ExpectedRevision < 1 || !req.Confirm {
		jsonError(w, "id, expected_revision and confirm=true required", 400)
		return
	}
	unlock := lockMemoryTenant(tenant)
	defer unlock()
	revision, err := forgetMemory(r.Context(), s.rdb, tenant, req.ID, req.ExpectedRevision)
	if err != nil {
		memoryError(w, err)
		return
	}
	denseErr := defaultQdrantClient.DeletePoint(r.Context(), tenant, req.ID)
	graphErr := s.removeMemoryGraph(r.Context(), tenant, req.ID)
	complete := denseErr == nil && graphErr == nil
	s.auditMemoryChange(r, tenant, key, "memory.forget", req.ID, revision)
	if !complete {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
	}
	// Deleted content cannot be returned by broker recall even if an external
	// projection is unavailable. Retrying this ID continues physical cleanup.
	respondJSON(w, map[string]interface{}{"id": req.ID, "revision": revision, "forgotten": true, "retrievable": false, "cleanup_complete": complete, "cleanup": map[string]bool{"active_and_archive": true, "revision_content": true, "bm25": true, "dense": denseErr == nil, "graph": graphErr == nil}, "scope": "this memory ID and its revisions in broker storage/indexes; backups, independently copied records and previously returned client context are not erased", "retry_required": !complete})
}

// Retained for API/readback tooling: the tombstone contains only identity and
// time, never the forgotten text, metadata or revision snapshots.
func forgottenReceipt(ctx context.Context, rdb *redis.Client, tenant, id string) (map[string]interface{}, error) {
	raw, err := rdb.Get(ctx, memoryTombstoneKey(tenant, id)).Result()
	if err != nil {
		return nil, err
	}
	var receipt map[string]interface{}
	if err = json.Unmarshal([]byte(raw), &receipt); err != nil {
		return nil, fmt.Errorf("invalid tombstone")
	}
	return receipt, nil
}
