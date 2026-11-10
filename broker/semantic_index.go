package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const semanticPendingKey = "broker:semantic-index:pending"

var errSemanticIndexInProgress = errors.New("index write already in progress")
var errSemanticSourceChanged = errors.New("source changed during indexing")

var semanticIndexMetrics struct {
	indexed   atomic.Uint64
	failed    atomic.Uint64
	discarded atomic.Uint64
}

func semanticMember(tenant, id string) string {
	b, _ := json.Marshal([2]string{tenant, id})
	return string(b)
}

// Hash only source fields, excluding access counts and index acknowledgments.
// Include the write revision but do not rely on it alone: a rolled-back legacy
// binary can replace source fields without changing the newer revision field.
func semanticRevision(data map[string]string) string {
	b, _ := json.Marshal([]string{data["index_revision"], data["revision"], data["embedding"], data["embedding_model"], data["agent_id"], data["tags"], data["text"], data["metadata"], data["created_at"]})
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func semanticPoint(id string, data map[string]string) (QdrantPoint, error) {
	var embedding []float32
	if err := json.Unmarshal([]byte(data["embedding"]), &embedding); err != nil {
		return QdrantPoint{}, fmt.Errorf("invalid stored embedding: %w", err)
	}
	if err := validateGeneratedEmbedding(embedding); err != nil {
		return QdrantPoint{}, err
	}
	payload := recallPayload(data)
	payload["source_id"] = id
	payload["embedding_model"] = data["embedding_model"]
	payload["index_revision"] = semanticRevision(data)
	return QdrantPoint{ID: id, Vector: embedding, Payload: payload}, nil
}

// Compare the source fields as well as revision: this also protects legacy
// writers that do not update index_revision. A concurrent update remains queued.
var acknowledgeSemanticIndex = redis.NewScript(`
if redis.call('SISMEMBER', KEYS[2], ARGV[1]) == 0 or redis.call('EXISTS', KEYS[1]) == 0 or redis.call('EXISTS', KEYS[4]) == 1 then return -1 end
for i = 4, #ARGV, 2 do
  if (redis.call('HGET', KEYS[1], ARGV[i]) or '') ~= ARGV[i+1] then return 0 end
end
redis.call('HSET', KEYS[1], 'indexed_revision', ARGV[3])
redis.call('ZREM', KEYS[3], ARGV[2])
return 1
`)

var clearInactiveSemanticWork = redis.NewScript(`
if redis.call('SISMEMBER', KEYS[2], ARGV[1]) == 0 or redis.call('EXISTS', KEYS[1]) == 0 or redis.call('EXISTS', KEYS[4]) == 1 then
  redis.call('ZREM', KEYS[3], ARGV[2])
  return 1
end
return 0
`)

func indexSemanticDocument(ctx context.Context, rdb *redis.Client, q *QdrantClient, tenant, id string, repairLexical ...bool) error {
	// Vault/import handlers and the background worker may synchronize the
	// same record concurrently. Serialize their external writes, with an
	// operation deadline shorter than the lease and token-checked unlock.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	lockKey := fmt.Sprintf("tenant:%s:semantic-lock:%s", tenant, id)
	token := uuid.NewString()
	locked, err := rdb.SetNX(ctx, lockKey, token, 30*time.Second).Result()
	if err != nil {
		return err
	}
	if !locked {
		return errSemanticIndexInProgress
	}
	defer func() {
		unlockCtx, unlockCancel := context.WithTimeout(context.Background(), time.Second)
		defer unlockCancel()
		_ = rdb.Eval(unlockCtx, `if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('DEL',KEYS[1]) end return 0`, []string{lockKey}, token).Err()
	}()
	data, err := rdb.HGetAll(ctx, vecKey(tenant, id)).Result()
	if err != nil {
		return err
	}
	active, err := rdb.SIsMember(ctx, vecSetKey(tenant), id).Result()
	if err != nil {
		return err
	}
	// A tombstoned ID is inactive even when residue (an active hash and set
	// membership written by a tombstone-unaware broker or an old restore) is
	// present: delete its point instead of projecting it again.
	forgotten, err := memoryForgotten(ctx, rdb, tenant, id)
	if err != nil {
		return err
	}
	keys := []string{vecKey(tenant, id), vecSetKey(tenant), semanticPendingKey, memoryTombstoneKey(tenant, id)}
	member := semanticMember(tenant, id)
	if !active || len(data) == 0 || forgotten {
		if err := q.DeletePoint(ctx, tenant, id); err != nil {
			return err
		}
		return clearInactiveSemanticWork.Run(ctx, rdb, keys, id, member).Err()
	}
	// The durable outbox also repairs a request whose best-effort lexical
	// update failed. Preserve text-less legacy TF indexes during migration.
	if (len(repairLexical) > 0 && repairLexical[0]) || data["index_revision"] != "" || data["text"] != "" {
		var metadata map[string]interface{}
		_ = json.Unmarshal([]byte(data["metadata"]), &metadata)
		if err := updateBM25Index(ctx, rdb, tenant, id, vaultRecordText(data, metadata)); err != nil {
			return err
		}
	}
	point, err := semanticPoint(id, data)
	if err != nil {
		return err
	}
	// The existing tenant collection owns its dimension. Never recreate it to
	// accommodate a mismatched vector; the outbox retains failed work.
	if err := q.EnsureCollection(ctx, tenant, len(point.Vector)); err != nil {
		return err
	}
	if err := q.UpsertPoint(ctx, tenant, point); err != nil {
		return err
	}
	args := []interface{}{id, member, semanticRevision(data)}
	for _, field := range []string{"index_revision", "revision", "embedding", "embedding_model", "agent_id", "tags", "text", "metadata", "created_at"} {
		args = append(args, field, data[field])
	}
	result, err := acknowledgeSemanticIndex.Run(ctx, rdb, keys, args...).Int()
	if err != nil {
		return err
	}
	if result == -1 {
		// Vaulting may have deleted the point while this HTTP upsert was in
		// flight. Delete that late write too; reads always check active Redis.
		if err := q.DeletePoint(ctx, tenant, id); err != nil {
			return err
		}
		return clearInactiveSemanticWork.Run(ctx, rdb, keys, id, member).Err()
	}
	if result == 0 {
		return errSemanticSourceChanged
	}
	semanticIndexMetrics.indexed.Add(1)
	return nil
}

// Reconciliation advances one SCAN/SSCAN page per tick, independent of Qdrant
// availability. Only active plaintext keyspaces are eligible; never silo keys.
type semanticReconciler struct {
	keyCursor       uint64
	keys            []string
	docCursor       uint64
	lexicalBackfill bool
}

func (s *semanticReconciler) step(ctx context.Context, rdb *redis.Client) error {
	if len(s.keys) == 0 {
		keys, next, err := rdb.Scan(ctx, s.keyCursor, "tenant:*:vecs", 32).Result()
		if err != nil {
			return err
		}
		s.keys, s.keyCursor = keys, next
		if len(keys) == 0 {
			return nil
		}
	}
	key := s.keys[0]
	tenant := strings.TrimSuffix(strings.TrimPrefix(key, "tenant:"), ":vecs")
	if !validID.MatchString(tenant) {
		s.keys = s.keys[1:]
		s.docCursor = 0
		return nil
	}
	if s.docCursor == 0 {
		ready, err := bm25PostingsReady(ctx, rdb, tenant)
		if err != nil {
			return err
		}
		s.lexicalBackfill = !ready
	}
	ids, next, err := rdb.SScan(ctx, key, s.docCursor, "", 64).Result()
	if err != nil {
		return err
	}
	// SSCAN COUNT is a hint: explicitly bound each Redis pipeline even when
	// a compact set is returned in one reply.
	for start := 0; start < len(ids); start += 64 {
		end := start + 64
		if end > len(ids) {
			end = len(ids)
		}
		if s.lexicalBackfill {
			if _, _, err := backfillBM25Page(ctx, rdb, tenant, ids[start:end]); err != nil {
				return err
			}
		}
		pipe := rdb.Pipeline()
		cmds := make([]*redis.MapStringStringCmd, end-start)
		for i, id := range ids[start:end] {
			cmds[i] = pipe.HGetAll(ctx, vecKey(tenant, id))
		}
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			return err
		}
		write := rdb.Pipeline()
		for i, id := range ids[start:end] {
			data, err := cmds[i].Result()
			if err != nil {
				return err
			}
			if len(data) > 0 && data["indexed_revision"] != semanticRevision(data) {
				write.ZAddNX(ctx, semanticPendingKey, redis.Z{Score: float64(time.Now().UnixMilli()), Member: semanticMember(tenant, id)})
			}
		}
		if _, err := write.Exec(ctx); err != nil {
			return err
		}
	}
	if next == 0 {
		if s.lexicalBackfill {
			if err := markBM25PostingsComplete(ctx, rdb, tenant); err != nil {
				return err
			}
		}
		s.keys = s.keys[1:]
	}
	s.docCursor = next
	return nil
}

func drainSemanticIndex(ctx context.Context, rdb *redis.Client, q *QdrantClient) error {
	now := time.Now().UnixMilli()
	items, err := rdb.ZRangeByScoreWithScores(ctx, semanticPendingKey, &redis.ZRangeBy{Min: "-inf", Max: strconv.FormatInt(now, 10), Count: 16}).Result()
	if err != nil {
		return err
	}
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		member, ok := item.Member.(string)
		if !ok {
			if err := discardMalformedSemanticWork(ctx, rdb, item.Member, "non-string member"); err != nil {
				return err
			}
			continue
		}
		var doc [2]string
		if err := json.Unmarshal([]byte(member), &doc); err != nil {
			if err := discardMalformedSemanticWork(ctx, rdb, member, "invalid JSON"); err != nil {
				return err
			}
			continue
		}
		if !validID.MatchString(doc[0]) || !validID.MatchString(doc[1]) {
			if err := discardMalformedSemanticWork(ctx, rdb, member, "invalid tenant or document ID"); err != nil {
				return err
			}
			continue
		}
		itemCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		// Ordinary writes, correction, forget and vault transitions share this
		// tenant lock. Take it only at the worker boundary: synchronous callers
		// already hold it while reindexActiveDoc acquires the distributed lease.
		indexErr := func() error {
			unlock := lockMemoryTenant(doc[0])
			defer unlock()
			return indexSemanticDocument(itemCtx, rdb, q, doc[0], doc[1])
		}()
		cancel()
		if errors.Is(indexErr, errSemanticIndexInProgress) {
			// Another request owns the writer lease and should acknowledge this
			// outbox item. Leave it due so the next worker tick can verify that.
			continue
		}
		if err := indexErr; err != nil {
			semanticIndexMetrics.failed.Add(1)
			log.Printf("semantic index failed tenant=%s doc=%s: %v", doc[0], doc[1], err)
			// Compare the observed due time so retries cannot postpone a newer
			// store's outbox entry. Failed tenants do not block later entries.
			_, retryErr := rdb.Eval(ctx, `if tonumber(redis.call('ZSCORE',KEYS[1],ARGV[1])) == tonumber(ARGV[2]) then redis.call('ZADD',KEYS[1],ARGV[3],ARGV[1]) end return 0`, []string{semanticPendingKey}, member, item.Score, time.Now().Add(time.Minute).UnixMilli()).Result()
			if retryErr != nil {
				return retryErr
			}
		}
	}
	return nil
}

func discardMalformedSemanticWork(ctx context.Context, rdb *redis.Client, member interface{}, reason string) error {
	if err := rdb.ZRem(ctx, semanticPendingKey, member).Err(); err != nil {
		return fmt.Errorf("discard malformed semantic outbox entry: %w", err)
	}
	semanticIndexMetrics.discarded.Add(1)
	log.Printf("semantic index discarded malformed outbox entry: %s", reason)
	return nil
}

func RunSemanticIndexWorker(ctx context.Context, rdb *redis.Client, q *QdrantClient) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		reconciler := new(semanticReconciler)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Printf("semantic index worker panic: %v", rec)
					}
				}()
				workCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				if err := reconciler.step(workCtx, rdb); err != nil {
					log.Printf("semantic reconciliation: %v", err)
				}
				if err := drainSemanticIndex(workCtx, rdb, q); err != nil && ctx.Err() == nil {
					log.Printf("semantic indexing: %v", err)
				}
			}()
		}
	}()
	return done
}
