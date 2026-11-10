package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Lifecycle metadata follows the existing Redis persistence/backup boundary.
// Tombstones deliberately have no TTL: a stale import must not resurrect a
// forgotten ID. Historical content is bounded and purged by forget.
const memoryRevisionRetention = 100
const memoryRevisionByteLimit = 1 << 20

var errMemoryForgotten = errors.New("memory was forgotten; this ID cannot be reused")
var errMemoryConflict = errors.New("memory revision changed; fetch the current memory before correcting it")
var errMemoryNotFound = errors.New("memory not found")
var errCheckpointImmutable = errors.New("checkpoint snapshots cannot be corrected; save a new checkpoint ID")

func memoryTombstoneKey(tenant, id string) string {
	return "tenant:" + tenant + ":memory:forgotten:" + id
}
func memoryVersionsKey(tenant, id string) string {
	return "tenant:" + tenant + ":memory:versions:" + id
}
func memoryVaultKey(tenant, id string) string { return "tenant:" + tenant + ":vault:" + id }

// Read-side tombstone filter. Forget removes every copy it can see, but
// residue can come back without passing a tombstone-aware write: an older
// broker running after a forget (0.4.4 accepts the ID again), or a Redis
// restore from before the forget. Every path that returns, counts, exports,
// projects or cites memory content therefore checks the tombstone itself
// instead of trusting that cleanup was final.

// memoryForgotten reports whether id has a lifecycle tombstone.
func memoryForgotten(ctx context.Context, rdb *redis.Client, tenant, id string) (bool, error) {
	n, err := rdb.Exists(ctx, memoryTombstoneKey(tenant, id)).Result()
	if err != nil {
		return false, fmt.Errorf("read memory tombstone: %w", err)
	}
	return n > 0, nil
}

// tombstoneHidden is the read-side check for a pipelined EXISTS on a
// tombstone key. It fails closed: a Redis error hides the record.
func tombstoneHidden(cmd *redis.IntCmd) bool {
	n, err := cmd.Result()
	return err != nil || n > 0
}

type memoryWriteReceipt struct {
	Revision int64 `json:"revision"`
	Changed  bool  `json:"changed"`
}

// storeMemoryFields atomically checks the revision, retains the previous
// content, and replaces the current record. Every ordinary store/import uses
// this boundary, including clients that predate the lifecycle tools.
func storeMemoryFields(ctx context.Context, rdb *redis.Client, tenant string, sv storedVector, fields map[string]interface{}) error {
	fields["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	fields["change_reason"] = sv.ChangeReason
	fields["agent_id"] = sv.AgentID
	fields["text"] = sv.Text
	fields["record_kind"] = sv.RecordKind
	fields["index_revision"] = uuid.NewString()
	tags, _ := json.Marshal(sv.Tags)
	fields["tags"] = string(tags)
	expected := int64(-1) // legacy stores retain upsert semantics, with revisions
	if sv.ExpectedRevision != nil {
		expected = *sv.ExpectedRevision
	}
	args := []interface{}{sv.ID, expected, memoryRevisionRetention, sv.ChangeReason, time.Now().UnixMilli(), semanticMember(tenant, sv.ID)}
	for k, v := range fields {
		args = append(args, k, v)
	}
	const script = `
if redis.call('EXISTS', KEYS[5]) == 1 then return {-2, 0} end
local old = redis.call('HGETALL', KEYS[1])
if #old == 0 then old = redis.call('HGETALL', KEYS[2]) end
local previous = {}
for i = 1, #old, 2 do previous[old[i]] = old[i+1] end
local revision = tonumber(previous.revision or '1')
if #old == 0 then revision = 0 end
if tonumber(ARGV[2]) >= 0 and tonumber(ARGV[2]) ~= revision then return {-1, revision} end
local incoming = {}
for i = 7, #ARGV, 2 do incoming[ARGV[i]] = ARGV[i+1] end
if previous.record_kind == 'checkpoint' and (incoming.record_kind ~= 'checkpoint' or previous.text ~= incoming.text or previous.agent_id ~= incoming.agent_id) then return {-3, revision} end
local same = #old > 0
for _, k in ipairs({'text','agent_id','tags','metadata','namespace','embedding_model','embedding'}) do
  if (previous[k] or '') ~= (incoming[k] or '') then same = false end
end
if same and redis.call('EXISTS', KEYS[1]) == 1 then
  redis.call('ZADD', KEYS[6], ARGV[5], ARGV[6])
  return {revision, 0}
end
if #old > 0 then
  previous.embedding = nil
  previous.access_count = nil
  previous.revision = tostring(revision)
  redis.call('LPUSH', KEYS[4], cjson.encode(previous))
  redis.call('LTRIM', KEYS[4], 0, tonumber(ARGV[3]) - 1)
  local bytes = 0
  local versions = redis.call('LRANGE', KEYS[4], 0, -1)
  for i, value in ipairs(versions) do
    bytes = bytes + string.len(value)
    if bytes > 1048576 then
      if i == 1 then redis.call('DEL', KEYS[4]) else redis.call('LTRIM', KEYS[4], 0, i - 2) end
      break
    end
  end
  incoming.created_at = previous.created_at or incoming.created_at
end
incoming.revision = tostring(revision + 1)
local data = {}
for k, v in pairs(incoming) do table.insert(data, k); table.insert(data, v) end
redis.call('DEL', KEYS[1], KEYS[2])
redis.call('HSET', KEYS[1], unpack(data))
redis.call('SADD', KEYS[3], ARGV[1])
redis.call('ZADD', KEYS[6], ARGV[5], ARGV[6])
return {revision + 1, 1}`
	result, err := redis.NewScript(script).Run(ctx, rdb, []string{vecKey(tenant, sv.ID), memoryVaultKey(tenant, sv.ID), vecSetKey(tenant), memoryVersionsKey(tenant, sv.ID), memoryTombstoneKey(tenant, sv.ID), semanticPendingKey}, args...).Int64Slice()
	if err != nil {
		return fmt.Errorf("store memory revision: %w", err)
	}
	if result[0] == -2 {
		return errMemoryForgotten
	}
	if result[0] == -1 {
		return errMemoryConflict
	}
	if result[0] == -3 {
		return errCheckpointImmutable
	}
	if sv.Receipt != nil {
		sv.Receipt.Revision, sv.Receipt.Changed = result[0], result[1] == 1
	}
	return nil
}

type memoryRecord struct {
	ID           string          `json:"id"`
	Revision     int64           `json:"revision"`
	Status       string          `json:"status"`
	Text         string          `json:"text"`
	AgentID      string          `json:"agent_id,omitempty"`
	Tags         []string        `json:"tags"`
	Metadata     json.RawMessage `json:"metadata"`
	Namespace    string          `json:"namespace"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
	ChangeReason string          `json:"change_reason,omitempty"`
}

func memoryRecordFromFields(id, status string, fields map[string]string) memoryRecord {
	revision, _ := strconv.ParseInt(fields["revision"], 10, 64)
	if revision < 1 {
		revision = 1
	}
	meta := json.RawMessage(fields["metadata"])
	if !json.Valid(meta) {
		meta = json.RawMessage(`{}`)
	}
	var metadata map[string]interface{}
	_ = json.Unmarshal(meta, &metadata)
	updated := fields["updated_at"]
	if updated == "" {
		updated = fields["created_at"]
	}
	namespace := fields["namespace"]
	if namespace == "" {
		namespace = "notes"
	}
	return memoryRecord{ID: id, Revision: revision, Status: status, Text: vaultRecordText(fields, metadata), AgentID: fields["agent_id"], Tags: parseTags(fields["tags"]), Metadata: meta, Namespace: namespace, CreatedAt: fields["created_at"], UpdatedAt: updated, ChangeReason: fields["change_reason"]}
}

// One Redis operation prevents a move/delete between active and vault reads.
func readMemoryFields(ctx context.Context, rdb *redis.Client, tenant, id string) (map[string]string, string, error) {
	const script = `
if redis.call('EXISTS', KEYS[3]) == 1 then return {'forgotten'} end
local data = redis.call('HGETALL', KEYS[1])
local status = 'active'
if #data == 0 then data = redis.call('HGETALL', KEYS[2]); status = 'archived' end
if #data == 0 then return {'missing'} end
table.insert(data, 1, status)
return data`
	values, err := redis.NewScript(script).Run(ctx, rdb, []string{vecKey(tenant, id), memoryVaultKey(tenant, id), memoryTombstoneKey(tenant, id)}).StringSlice()
	if err != nil {
		return nil, "", err
	}
	if values[0] == "forgotten" {
		return nil, "", errMemoryForgotten
	}
	if values[0] == "missing" {
		return nil, "", errMemoryNotFound
	}
	fields := map[string]string{}
	for i := 1; i+1 < len(values); i += 2 {
		fields[values[i]] = values[i+1]
	}
	return fields, values[0], nil
}
