package main

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// POST /vault/flush-working-memory
// Auth: machine API key or admin basic auth.
// Body: {"tenant_id": "..."}
// Immediately runs one consolidation pass for a single tenant, using the
// configured selector (see consolidateOne).
// ---------------------------------------------------------------------------

// seenIDs is a bounded-memory membership set for a single SCAN pass.
//
// SCAN's contract is that every element present for the whole iteration is
// returned at least once — it explicitly does NOT promise exactly once, because
// rehashing can hand the same key back on two successive cursors. Both the
// export and the stats handler stream a page at a time precisely so the
// keyspace is never materialised, so a "collect into a map first" fix is not
// available: the set has to be populated as the scan proceeds, and it therefore
// has to be small per entry.
//
// A map[string]struct{} over docIDs would hold the full ID text for up to 2M
// vectors — ~100+ MB of string headers and backing arrays, right at the
// LimitRSS=1G ceiling the handlers are written around. A 128-bit FNV-1a digest
// per ID is a fixed 16 bytes plus map overhead, ~20x smaller, and collisions
// only matter between two IDs that both appear in the same single pass of one
// tenant's keys. A collision there would drop one duplicate entry from an
// export (or miss its contribution to a size estimate) — it cannot leak,
// cross tenants, or corrupt stored data, which is the trade this set takes for
// staying inside the memory budget.
type seenIDs map[[16]byte]struct{}

// add records id and reports whether it had not been seen in this pass.
func (s seenIDs) add(id string) bool {
	var h [16]byte
	fnv128a := fnv.New128a()
	_, _ = fnv128a.Write([]byte(id))
	copy(h[:], fnv128a.Sum(nil))
	if _, dup := s[h]; dup {
		return false
	}
	s[h] = struct{}{}
	return true
}

// resolveVaultCaller resolves the two auth paths shared by the vault and
// agents handlers — machine API key (header or Bearer) and admin basic auth —
// and returns the audit actor to record for the request. When the caller may
// not proceed, the response has already been written and ok is false.
//
// The API-key path exists because the MCP tool surface calls these handlers
// directly: invokeMCPTool sets the API-key and tenant headers (mcp.go).
//
// Scope: validateAPIKey is handed the requested tenant_id and validates the key
// against it, so a key for another tenant cannot authorise this one.
func (s *Server) resolveVaultCaller(w http.ResponseWriter, r *http.Request, tenantID string) (string, bool) {
	if key := s.requestAPIKey(r); key != "" {
		if _, err := validateAPIKey(r.Context(), s.rdb, s.db, tenantID, key); err != nil {
			jsonError(w, "unauthorized", http.StatusUnauthorized)
			return "", false
		}
		setCORSHeaders(w)
		return "apikey:" + tenantID, true
	}
	if !s.checkAdminAuth(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="heurchain-admin"`)
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	setCORSHeaders(w)
	return "admin", true
}

// adminAuditDetail names the admin path in the audit row, so a basic-auth
// action is distinguishable from an API-key one.
func adminAuditDetail(actor string) string {
	if actor == "admin" {
		return "admin basic auth"
	}
	return ""
}

func (s *Server) handleVaultFlush(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse body first so we have tenant_id available for the ownership check
	// regardless of which auth path we take.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		TenantID string `json:"tenant_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid json body", http.StatusBadRequest)
		return
	}
	if req.TenantID == "" || !validID.MatchString(req.TenantID) {
		jsonError(w, "missing or invalid tenant_id", http.StatusBadRequest)
		return
	}

	actor, ok := s.resolveVaultCaller(w, r, req.TenantID)
	if !ok {
		return
	}
	// Destructive and irreversible for the working set — audit the attempt on
	// every auth path, so the trail does not depend on how the caller got in.
	s.auditAction(r, req.TenantID, actor, "vault.flush_working_memory", "", true, adminAuditDetail(actor))

	ctx := r.Context()
	promoted, err := consolidateOne(ctx, s.rdb, req.TenantID)
	if err == nil && ctx.Err() != nil {
		// A pass ends early when its context ends and reports the partial count
		// with a nil error, because the background worker treats a per-tenant
		// timeout as an ordinary partial sweep. A request caller has no such
		// licence: answering 200 {"flushed": true} for a pass the deadline cut
		// short tells the caller the working set was flushed when part of it is
		// still in active memory.
		err = fmt.Errorf("flush aborted before completion: %w", ctx.Err())
	}
	if err != nil {
		log.Printf("vault flush error tenant=%s: %v", req.TenantID, err)
		jsonError(w, "flush failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Record usage event so the audit trail reflects the manual flush.
	if _, dbErr := s.db.ExecContext(ctx,
		`INSERT INTO usage_events (tenant_id, event_type, vectors_stored) VALUES (?, 'flush', 0)`,
		req.TenantID); dbErr != nil {
		log.Printf("usage_events write failed tenant=%s: %v", req.TenantID, dbErr)
	}

	respondJSON(w, map[string]interface{}{
		"flushed":  true,
		"promoted": promoted,
	})
}

// ---------------------------------------------------------------------------
// GET /vault/export?tenant_id=...
// Auth: machine API key or admin basic auth.
// Streams a zip archive; one Markdown file per stored vector.
// Embeddings are excluded — only human-readable fields are exported.
// ---------------------------------------------------------------------------

func (s *Server) handleVaultExport(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tenantID := r.URL.Query().Get("tenant_id")
	if tenantID == "" || !validID.MatchString(tenantID) {
		jsonError(w, "missing or invalid tenant_id", http.StatusBadRequest)
		return
	}

	// A full-corpus export is the highest-value response this API emits, and the
	// second link in the tenant-takeover chain. Audit before streaming: once the
	// ZIP starts the status code is committed and a late failure can no longer
	// be reported.
	actor, ok := s.resolveVaultCaller(w, r, tenantID)
	if !ok {
		return
	}
	s.auditAction(r, tenantID, actor, "vault.export", "", true, adminAuditDetail(actor))

	ctx := r.Context()

	filename := fmt.Sprintf("%s-vault-%d.zip", tenantID, time.Now().Unix())
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))

	// Stream the zip directly to the response writer — never buffer the full
	// archive in memory.
	zw := zip.NewWriter(w)
	defer func() {
		if err := zw.Close(); err != nil {
			log.Printf("vault export zip close error tenant=%s: %v", tenantID, err)
		}
	}()

	// Two item locations:
	//   tenant:<id>:vault:<docID>  — items promoted by the consolidation worker
	//   tenant:<id>:vec:<docID>    — items still in hot working memory (not flushed)
	// SCAN both and write each zip entry as we go, NEVER materialising the full
	// keyspace in memory — at the 2M-vector ceiling a docID map alone OOMs under
	// LimitRSS=1G. Promotion deletes the vec key (see consolidateTenant), so a
	// docID lives in exactly one location in steady state; to preserve the
	// "prefer vault" dedup against the transient promotion race, the vec pass
	// skips any docID whose vault hash still exists (an O(1) EXISTS check rather
	// than an O(keyspace) seen-set).
	vaultPrefix := fmt.Sprintf("tenant:%s:vault:", tenantID)
	vecPrefix := fmt.Sprintf("tenant:%s:vec:", tenantID)

	// writeEntry renders one Redis hash as an <id>.md zip entry. Best-effort:
	// per-entry errors are logged and skipped, matching the original handler.
	writeEntry := func(id, sourceKey string) {
		// The tenant was told this memory was deleted; never export residue.
		// Fails closed on Redis errors.
		if forgotten, err := memoryForgotten(ctx, s.rdb, tenantID, id); err != nil || forgotten {
			return
		}
		data, err := s.rdb.HGetAll(ctx, sourceKey).Result()
		if err != nil || len(data) == 0 {
			return
		}

		// Parse metadata so we can pretty-print it and extract the text field.
		rawMeta := data["metadata"]
		var metaMap map[string]interface{}
		_ = json.Unmarshal([]byte(rawMeta), &metaMap)

		// Resolve the body text from whichever field the writer used. Reading
		// only metadata.text here is what shipped every natively-stored memory
		// as an empty Markdown body.
		textContent := vaultRecordText(data, metaMap)

		agentID := data["agent_id"]
		if agentID == "" {
			agentID = "unspecified"
		}

		createdAt := data["created_at"]
		tags := parseTags(data["tags"])

		prettyMeta, err := json.MarshalIndent(metaMap, "", "  ")
		if err != nil {
			prettyMeta = []byte("{}")
		}

		mdContent := buildMarkdown(id, tenantID, agentID, createdAt, tags, textContent, prettyMeta)

		fw, err := zw.Create(id + ".md")
		if err != nil {
			log.Printf("vault export create entry error tenant=%s id=%s: %v", tenantID, id, err)
			return
		}
		if _, err := fw.Write([]byte(mdContent)); err != nil {
			log.Printf("vault export write error tenant=%s id=%s: %v", tenantID, id, err)
		}
	}

	// Pass 1: vault keys (authoritative). Stream one page at a time.
	// seen guards against SCAN returning one key twice, which would emit the
	// same <id>.md entry twice and make the archive ambiguous to restore.
	seen := seenIDs{}
	var cursor uint64
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		keys, next, err := s.rdb.Scan(ctx, cursor, vaultPrefix+"*", 200).Result()
		if err != nil {
			log.Printf("vault export scan vault error tenant=%s: %v", tenantID, err)
			return // headers already sent; partial zip is better than nothing
		}
		for _, k := range keys {
			if err := ctx.Err(); err != nil {
				return
			}
			id := k[len(vaultPrefix):]
			if !seen.add(id) {
				continue
			}
			writeEntry(id, k)
		}
		if next == 0 {
			break
		}
		cursor = next
	}

	// Pass 2: hot working-memory keys not yet promoted. Skip any docID whose
	// vault hash exists so a doc mid-promotion is never emitted twice. Pass 2
	// shares pass 1's seen set: the prefixes are disjoint, so membership can
	// only come from a repeated key within this pass.
	cursor = 0
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		keys, next, err := s.rdb.Scan(ctx, cursor, vecPrefix+"*", 200).Result()
		if err != nil {
			log.Printf("vault export scan vec error tenant=%s: %v", tenantID, err)
			return
		}
		for _, k := range keys {
			if err := ctx.Err(); err != nil {
				return
			}
			docID := k[len(vecPrefix):]
			if exists, _ := s.rdb.Exists(ctx, vaultPrefix+docID).Result(); exists > 0 {
				continue // already exported in pass 1
			}
			if !seen.add(docID) {
				continue // SCAN repeated this key within pass 2
			}
			writeEntry(docID, k)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
}

// buildMarkdown renders the frontmatter + body for a single exported vector.
// tags is rendered as a YAML flow-style array; omitted when empty.
func buildMarkdown(id, tenantID, agentID, createdAt string, tags []string, text string, metaJSON []byte) string {
	tagLine := ""
	if len(tags) > 0 {
		tagLine = "tags: ["
		for i, t := range tags {
			if i > 0 {
				tagLine += ", "
			}
			tagLine += t
		}
		tagLine += "]\n"
	}
	return fmt.Sprintf("---\nid: %s\ntenant: %s\nagent_id: %s\ncreated_at: %s\n%s---\n\n%s\n\n## Metadata\n\n```json\n%s\n```\n",
		id, tenantID, agentID, createdAt, tagLine, text, string(metaJSON))
}

// vaultRecordText resolves the body text of a stored record, which lives in one
// of two places depending on which path wrote it:
//
//   - storeVec (the /store path, i.e. every natively-stored memory) writes it to
//     the top-level Redis hash field "text".
//   - handleVaultImport historically wrote it only into metadata.text.
//
// A reader that checks just one convention silently yields an empty body for
// every record written by the other. /vault/export checked only metadata.text
// and so exported an empty Markdown body for the entire natively-stored corpus.
// Both readers now resolve through this function so they cannot drift again.
//
// When the body is found, the "text" key is deleted from metaMap in *both*
// branches — under the top-level convention so the metadata block cannot print
// a stale second copy of the body, and under metadata.text so it is not printed
// inside the block it was lifted out of. metaMap is left untouched only when
// neither location holds text (the record has no body), which is also when the
// empty string is returned. metaMap may be nil: delete on a nil map is a no-op,
// so the caller can pass one without a guard.
func vaultRecordText(data map[string]string, metaMap map[string]interface{}) string {
	if t := data["text"]; t != "" {
		delete(metaMap, "text")
		return t
	}
	if t, ok := metaMap["text"].(string); ok {
		delete(metaMap, "text")
		return t
	}
	return ""
}

// ---------------------------------------------------------------------------
// GET /vault/stats?tenant_id=...
// Returns counts + sizes for the tenant's vault contents.
// Used by the dashboard to render "Sources" panel statistics.
// ---------------------------------------------------------------------------

// vaultStatsTotals is the running accumulator for one /vault/stats request.
type vaultStatsTotals struct {
	vectors    int64
	bytes      int64
	tokenChars int64
}

// vaultStatsScript folds one SCAN page into per-key totals inside Redis.
//
// The previous version read HKEYS in one pipeline and then the field lengths
// in a second. Between those two round trips a hash could be demoted, deleted
// or forgotten, so a key could be counted with a subset of its fields (or a
// vanished key could contribute zero bytes but still be counted). Redis runs a
// whole script atomically, so HKEYS and every HSTRLEN for a key see the same
// snapshot and a record is either counted in full or not at all.
//
// HSTRLEN returns the value's byte length, so the multi-kilobyte embedding
// blob is never transferred — the handler only needs sum(len(field)+len(value))
// and the byte counts of the human-readable fields.
//
// KEYS are the hash keys. For KEYS[i], ARGV holds its tombstone key and (for
// the vec pass) its vault key; an empty vault key means the vault pass. Each
// key contributes five values: {exists, bytes, human_chars, tombstoned,
// vault_exists}. Missing, empty and non-hash (WRONGTYPE) keys report exists=0;
// they are skipped rather than failing the page.
var vaultStatsScript = redis.NewScript(`
local out = {}
for i = 1, #KEYS do
  local key = KEYS[i]
  local tombKey = ARGV[(i - 1) * 2 + 1]
  local vaultKey = ARGV[(i - 1) * 2 + 2]
  local tomb, vault = 0, 0
  if tombKey ~= '' and redis.call('EXISTS', tombKey) == 1 then tomb = 1 end
  if vaultKey ~= '' and redis.call('EXISTS', vaultKey) == 1 then vault = 1 end

  local exists, bytes, chars = 0, 0, 0
  if tomb == 0 and vault == 0 then
    local kind = redis.pcall('TYPE', key)
    if type(kind) == 'table' and kind['ok'] == 'hash' then
      local fields = redis.pcall('HKEYS', key)
      if type(fields) == 'table' and fields['err'] == nil and #fields > 0 then
        exists = 1
        for j = 1, #fields do
          local field = fields[j]
          local n = redis.pcall('HSTRLEN', key, field)
          if type(n) ~= 'number' then
            exists, bytes, chars = 0, 0, 0
            break
          end
          bytes = bytes + #field + n
          if field == 'metadata' or field == 'text' or field == 'cue' then
            chars = chars + n
          end
        end
      end
    end
  end

  out[#out+1] = exists
  out[#out+1] = bytes
  out[#out+1] = chars
  out[#out+1] = tomb
  out[#out+1] = vault
end
return out
`)

// accumulateVaultStatsPage folds one SCAN page into totals with one atomic
// script round trip.
//
// vaultPrefix is empty for the authoritative vault pass and set for the vec
// pass, where it mirrors the export's "prefer vault" dedup. Callers share one
// seenIDs across both passes; it absorbs SCAN's at-least-once delivery.
func (s *Server) accumulateVaultStatsPage(ctx context.Context, tenantID, prefix string, keys []string, seen seenIDs, vaultPrefix string, totals *vaultStatsTotals) error {
	if len(keys) == 0 {
		return nil
	}

	args := make([]interface{}, 0, len(keys)*2)
	for _, key := range keys {
		id := key[len(prefix):]
		args = append(args, memoryTombstoneKey(tenantID, id))
		if vaultPrefix != "" {
			args = append(args, vaultPrefix+id)
		} else {
			args = append(args, "")
		}
	}
	values, err := vaultStatsScript.Run(ctx, s.rdb, keys, args...).Int64Slice()
	if err != nil {
		return fmt.Errorf("script: %w", err)
	}
	if len(values) != len(keys)*5 {
		return fmt.Errorf("script returned %d values for %d keys", len(values), len(keys))
	}

	for i, key := range keys {
		id := key[len(prefix):]
		base := i * 5
		// Pass 2 checks the vault copy before touching seen, matching the old
		// order: a promoted doc is skipped without being recorded as seen.
		if vaultPrefix != "" && values[base+4] == 1 {
			continue // already counted in pass 1
		}
		if !seen.add(id) {
			continue // SCAN repeated this key within the pass
		}
		if values[base+3] == 1 {
			continue // lifecycle tombstone: residue is not counted
		}
		if values[base] != 1 {
			continue // missing, empty or non-hash key
		}
		totals.vectors++
		totals.bytes += values[base+1]
		totals.tokenChars += values[base+2]
	}
	return nil
}

func (s *Server) handleVaultStats(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tenantID := r.URL.Query().Get("tenant_id")
	if tenantID == "" || !validID.MatchString(tenantID) {
		jsonError(w, "missing or invalid tenant_id", http.StatusBadRequest)
		return
	}

	// Same three auth paths as export and flush, including the machine API-key
	// path the MCP tool surface uses.
	if _, ok := s.resolveVaultCaller(w, r, tenantID); !ok {
		return
	}

	ctx := r.Context()

	// SCAN both vault and vec keys, accumulating counts and sizes as we go.
	// NEVER materialise the keyspace in memory — at the 2M-vector ceiling a
	// docID map alone OOMs under LimitRSS=1G. Promotion deletes the vec key, so
	// a docID lives in exactly one location in steady state; the vec pass skips
	// any docID whose vault hash exists (O(1) EXISTS) to mirror the export's
	// "prefer vault" dedup. The per-pass seenIDs set is not a keyspace map: it
	// holds one fixed 16-byte digest per key actually returned, and exists only
	// to absorb SCAN's at-least-once delivery (see its doc comment).
	vaultPrefix := fmt.Sprintf("tenant:%s:vault:", tenantID)
	vecPrefix := fmt.Sprintf("tenant:%s:vec:", tenantID)

	var totals vaultStatsTotals

	// Pass 1: vault keys (authoritative). seen absorbs SCAN's documented
	// at-least-once delivery, which would otherwise count one vector twice and
	// over-report both the item count and the size totals.
	seen := seenIDs{}
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, vaultPrefix+"*", 200).Result()
		if err != nil {
			log.Printf("vault stats scan vault error tenant=%s: %v", tenantID, err)
			jsonError(w, "failed to compute stats", http.StatusInternalServerError)
			return
		}
		if err := s.accumulateVaultStatsPage(ctx, tenantID, vaultPrefix, keys, seen, "", &totals); err != nil {
			log.Printf("vault stats accumulate vault error tenant=%s: %v", tenantID, err)
			jsonError(w, "failed to compute stats", http.StatusInternalServerError)
			return
		}
		if next == 0 {
			break
		}
		cursor = next
	}

	// Pass 2: hot working-memory keys not yet promoted.
	cursor = 0
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, vecPrefix+"*", 200).Result()
		if err != nil {
			log.Printf("vault stats scan vec error tenant=%s: %v", tenantID, err)
			jsonError(w, "failed to compute stats", http.StatusInternalServerError)
			return
		}
		if err := s.accumulateVaultStatsPage(ctx, tenantID, vecPrefix, keys, seen, vaultPrefix, &totals); err != nil {
			log.Printf("vault stats accumulate vec error tenant=%s: %v", tenantID, err)
			jsonError(w, "failed to compute stats", http.StatusInternalServerError)
			return
		}
		if next == 0 {
			break
		}
		cursor = next
	}

	tokensEstimate := totals.tokenChars / 4
	mb := float64(totals.bytes) / (1024.0 * 1024.0)

	respondJSON(w, map[string]interface{}{
		"vectors_total":         totals.vectors,
		"bytes_total":           totals.bytes,
		"size_mb":               fmt.Sprintf("%.3f", mb),
		"tokens_estimate":       tokensEstimate,
		"consolidation_metrics": consolidationMetricsSnapshot(),
		"semantic_index": map[string]interface{}{
			"indexed":   semanticIndexMetrics.indexed.Load(),
			"failed":    semanticIndexMetrics.failed.Load(),
			"discarded": semanticIndexMetrics.discarded.Load(),
		},
	})
}

// ---------------------------------------------------------------------------
// POST /admin/vault/import?tenant_id=... (moved under /admin/ for edge auth)
// Auth: admin basic auth.
// Body: a zip archive in the exact format produced by handleVaultExport, i.e.
// one Markdown file per record with YAML frontmatter (id, tenant, agent_id,
// created_at, tags) + body text + a ```json metadata block.
//
// Embeddings are NOT present in the export format, so each record's body text
// is re-embedded server-side and the record is restored into active working
// memory via the same storeVec / semantic-index / updateBM25Index paths the
// /store handler uses — leaving the imported memory fully queryable. Duplicate
// ids (already present in this tenant's vec set) are skipped.
//
// The archive is streamed to a temp file and entries are read one at a time so
// a large import never materialises the full archive in memory.
// ---------------------------------------------------------------------------

const maxVaultImportBytes = 1 << 30 // 1 GiB cap on the uploaded archive

// maxVaultEntryBytes caps the *uncompressed* size of a single archive entry.
// maxVaultImportBytes bounds the compressed upload only, so a small archive can
// still expand to an enormous amount of Markdown when read entry by entry (a zip
// bomb). The central directory's declared size is attacker-controlled, but it is
// checked rather than trusted, and that check holds: archive/zip will not
// decompress past the declared size, so an understated header fails the read
// instead of walking an oversize entry through. The per-entry read is bounded as
// well, as defence that does not rest on that reader behaviour — see
// parseVaultMarkdown for why it is kept but not relied on.
const maxVaultEntryBytes = 64 << 20 // 64 MiB per entry

var errVaultRehydrateConflict = errors.New("active document differs from vaulted document")

// isBodyTooLarge reports whether a read from a MaxBytesReader-capped body failed
// because the upload exceeded the limit. Such a request was well-formed but too
// large, so it maps to 413 rather than the generic 400 for a bad body.
func isBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// restoreActiveFromVault claims the active Redis record only when it is
// absent or already carries the vaulted version. The check and HSET/SADD are
// one Redis operation, so a concurrent store cannot slip between a Go-side
// read and the restore write and be overwritten by an older vault copy.
func restoreActiveFromVault(ctx context.Context, rdb *redis.Client, tenantID, docID string, data map[string]string) error {
	revision := data["revision"]
	if revision == "" {
		revision = "1"
	}
	args := []interface{}{data["created_at"], docID, revision, time.Now().UnixMilli(), semanticMember(tenantID, docID)}
	for field, value := range data {
		args = append(args, field, value)
	}
	const script = `
if redis.call('EXISTS', KEYS[3]) == 1 then return -2 end
local exists = redis.call('EXISTS', KEYS[1])
local current = redis.call('HGET', KEYS[1], 'created_at')
if exists == 1 and (not current or current ~= ARGV[1]) then return -1 end
if exists == 1 and (redis.call('HGET', KEYS[1], 'revision') or '1') ~= ARGV[3] then return -1 end
local fields = {}
for i = 6, #ARGV do fields[i - 5] = ARGV[i] end
if #fields > 0 then redis.call('HSET', KEYS[1], unpack(fields)) end
redis.call('SADD', KEYS[2], ARGV[2])
redis.call('ZADD', KEYS[4], ARGV[4], ARGV[5])
return 1`
	result, err := redis.NewScript(script).Run(ctx, rdb, []string{
		vecKey(tenantID, docID),
		vecSetKey(tenantID),
		memoryTombstoneKey(tenantID, docID),
		semanticPendingKey,
	}, args...).Int()
	if err != nil {
		return fmt.Errorf("restore active redis record: %w", err)
	}
	if result == -1 {
		return errVaultRehydrateConflict
	}
	if result == -2 {
		return errMemoryForgotten
	}
	return nil
}

// rehydrateVaultDoc restores one vaulted document to the active working set.
// It uses the preserved embedding, avoiding a slow and lossy export/import
// round-trip. The vault record remains until all active projections succeed,
// so retrying after an index outage is safe.
func rehydrateVaultDoc(ctx context.Context, rdb *redis.Client, tenantID, docID string) error {
	unlock := lockMemoryTenant(tenantID)
	defer unlock()
	return rehydrateVaultDocWithAfterProjection(ctx, rdb, tenantID, docID, nil)
}

// rehydrateVaultDocWithAfterProjection exposes the post-projection race window
// to deterministic tests. Production callers always use rehydrateVaultDoc.
func rehydrateVaultDocWithAfterProjection(
	ctx context.Context,
	rdb *redis.Client,
	tenantID, docID string,
	afterProjection func() error,
) error {
	vaultKey := fmt.Sprintf("tenant:%s:vault:%s", tenantID, docID)
	data, err := rdb.HGetAll(ctx, vaultKey).Result()
	if err != nil {
		return fmt.Errorf("read vault record: %w", err)
	}
	if len(data) == 0 {
		return fmt.Errorf("vault document not found")
	}

	var embedding []float32
	if err := json.Unmarshal([]byte(data["embedding"]), &embedding); err != nil || len(embedding) == 0 {
		return fmt.Errorf("vault document has invalid embedding")
	}
	var metadata map[string]interface{}
	_ = json.Unmarshal([]byte(data["metadata"]), &metadata)
	text := vaultRecordText(data, metadata)

	if err := restoreActiveFromVault(ctx, rdb, tenantID, docID, data); err != nil {
		return err
	}
	if text != "" {
		if err := updateBM25Index(ctx, rdb, tenantID, docID, text); err != nil {
			return fmt.Errorf("restore bm25 index: %w", err)
		}
	}
	if err := indexSemanticDocument(ctx, rdb, defaultQdrantClient, tenantID, docID); err != nil {
		return fmt.Errorf("restore semantic index: %w", err)
	}
	if afterProjection != nil {
		if err := afterProjection(); err != nil {
			return fmt.Errorf("after vault projection: %w", err)
		}
	}
	currentCreatedAt, err := rdb.HGet(ctx, vecKey(tenantID, docID), "created_at").Result()
	if err != nil {
		return fmt.Errorf("verify active record after restore: %w", err)
	}
	currentRevision, _ := rdb.HGet(ctx, vecKey(tenantID, docID), "revision").Result()
	if currentCreatedAt != data["created_at"] || currentRevision != data["revision"] {
		// A concurrent /store replaced the active record after the Lua guard
		// admitted this restore. The vault copy is intentionally left in
		// place, but BM25 and Qdrant were already written with the OLD text
		// and embedding above, so they now describe a document that no longer
		// exists — stale postings and a stale vector under a live doc id.
		//
		// Returning the conflict without repairing that would leave lexical
		// and vector search disagreeing with Redis, on a code path that only
		// runs during recovery, i.e. when the operator is already dealing
		// with damage. Re-project from whatever is actually active now.
		if repairErr := reindexActiveDoc(ctx, rdb, tenantID, docID); repairErr != nil {
			return fmt.Errorf("%w (and reindexing the superseding record failed: %v)",
				errVaultRehydrateConflict, repairErr)
		}
		return errVaultRehydrateConflict
	}
	if err := rdb.Del(ctx, vaultKey).Err(); err != nil {
		return fmt.Errorf("remove vault record after restore: %w", err)
	}
	return nil
}

// activeDocFingerprint identifies the exact contents of an active vector hash.
//
// It covers the fields the projections are built from — the body text, the
// metadata blob, the embedding, the agent and the tags — because those are what
// a projection would get wrong if they changed underneath it. created_at is
// deliberately not the guard: /store generates it server-side, but
// handleVaultImport writes one taken from the archive's frontmatter, so it is
// not reliably a write counter. Hashing the content is stronger anyway — it
// detects a change to any field that matters, not just a change that also
// bumped a timestamp.
func activeDocFingerprint(data map[string]string) string {
	h := fnv.New128a()
	for _, field := range []string{"text", "metadata", "embedding", "agent_id", "tags", "namespace", "embedding_model", "revision", "index_revision"} {
		_, _ = h.Write([]byte(field))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(data[field]))
		_, _ = h.Write([]byte{0})
	}
	return string(h.Sum(nil))
}

// reindexActiveDoc rebuilds the BM25 and Qdrant projections for whatever is
// currently in the active hash. Used to repair the indexes after a rehydration
// loses a race to a concurrent write; safe to call when the document has since
// been removed entirely, in which case there is nothing to project.
//
// This function exists to repair a race, so it must not itself be racy. The
// first version read the hash once and projected that snapshot, which left the
// same stale-index outcome available one step later: a /store landing between
// the read and the two projections would be partially overwritten — BM25
// rebuilt from the old text, then Qdrant upserted with the old vector, or vice
// versa — and lexical and vector search would disagree again, now with the
// repair having "succeeded". Each attempt therefore projects a snapshot and
// then re-reads to confirm the document still fingerprints the same; if it
// changed, the next attempt projects the newer contents.
//
// The retry is bounded rather than unbounded: a document being written
// continuously cannot be repaired by a read-then-compare loop, and spinning
// forever on the recovery path is worse than reporting that the repair could
// not converge. Losing that race is not permanent damage — every /store updates
// both projections itself, so the next write corrects them — which is why
// exhausting the attempts returns an error for the caller to surface rather
// than retrying without limit.
func reindexActiveDoc(ctx context.Context, rdb *redis.Client, tenantID, docID string) error {
	// indexSemanticDocument treats a forgotten ID as inactive, so residue is
	// never re-projected and any surviving Qdrant point is deleted.
	return reindexActiveDocWith(ctx, rdb, tenantID, docID, func(_ map[string]string) error {
		return indexSemanticDocument(ctx, rdb, defaultQdrantClient, tenantID, docID, true)
	})
}

// reindexActiveDocWith is reindexActiveDoc with the projection step injected.
//
// The retry only earns its keep if a write really can land mid-projection, and
// that interleaving cannot be produced from outside the function — the window
// is between two calls it makes itself. Taking the projection as a parameter
// lets a test perform the concurrent write *inside* that window, which is the
// only way to exercise the guard rather than assume it.
func reindexActiveDocWith(ctx context.Context, rdb *redis.Client, tenantID, docID string, project func(data map[string]string) error) error {
	const attempts = 3
	for attempt := 0; attempt < attempts; attempt++ {
		data, err := rdb.HGetAll(ctx, vecKey(tenantID, docID)).Result()
		if err != nil {
			return fmt.Errorf("read active record: %w", err)
		}
		if len(data) == 0 {
			return nil // nothing active to project
		}
		before := activeDocFingerprint(data)
		if err := project(data); err != nil {
			if errors.Is(err, errSemanticSourceChanged) {
				continue
			}
			return err
		}

		current, err := rdb.HGetAll(ctx, vecKey(tenantID, docID)).Result()
		if err != nil {
			return fmt.Errorf("re-read active record: %w", err)
		}
		if activeDocFingerprint(current) == before {
			return nil // the projected snapshot is still the active one
		}
	}
	return fmt.Errorf("active record for %s kept changing across %d reindex attempts", docID, attempts)
}

// handleVaultRehydrate POST /admin/vault/rehydrate?tenant_id=... body:
// {"doc_id":"..."}. Admin-only because it changes working-set membership.
func (s *Server) handleVaultRehydrate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.checkAdminAuth(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="heurchain-admin"`)
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	tenantID := r.URL.Query().Get("tenant_id")
	if tenantID == "" || !validID.MatchString(tenantID) {
		jsonError(w, "missing or invalid tenant_id", http.StatusBadRequest)
		return
	}
	var req struct {
		DocID string `json:"doc_id"`
	}
	if !decodeJSONBody(w, r, &req, 0) {
		return
	}
	if req.DocID == "" || !validID.MatchString(req.DocID) {
		jsonError(w, "missing or invalid doc_id", http.StatusBadRequest)
		return
	}
	started := time.Now()
	if err := rehydrateVaultDoc(r.Context(), s.rdb, tenantID, req.DocID); err != nil {
		consolidationMetrics.rehydrationFailures.Add(1)
		s.auditAction(r, tenantID, "admin", "vault.rehydrate", req.DocID, false, err.Error())
		if errors.Is(err, errVaultRehydrateConflict) {
			consolidationMetrics.rehydrationConflicts.Add(1)
			jsonError(w, err.Error(), http.StatusConflict)
		} else {
			jsonError(w, "rehydrate failed: "+err.Error(), http.StatusInternalServerError)
		}
		return
	}
	durationMS := time.Since(started).Milliseconds()
	consolidationMetrics.rehydrations.Add(1)
	consolidationMetrics.rehydrationDurationMS.Add(uint64(durationMS))
	log.Printf("vault rehydrate tenant=%s doc=%s duration_ms=%d", tenantID, req.DocID, durationMS)
	s.auditAction(r, tenantID, "admin", "vault.rehydrate", req.DocID, true, "")
	respondJSON(w, map[string]interface{}{"rehydrated": true, "doc_id": req.DocID, "duration_ms": durationMS})
}

func (s *Server) handleVaultImport(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tenantID := r.URL.Query().Get("tenant_id")
	if tenantID == "" || !validID.MatchString(tenantID) {
		jsonError(w, "missing or invalid tenant_id", http.StatusBadRequest)
		return
	}

	// Admin basic auth: import is restore tooling.
	if !s.checkAdminAuth(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="heurchain-admin"`)
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	setCORSHeaders(w)
	auditActor := "admin"

	ctx := r.Context()

	// Stream the upload to a temp file so we never buffer the whole archive in
	// memory; zip.NewReader needs random access (io.ReaderAt + size).
	r.Body = http.MaxBytesReader(w, r.Body, maxVaultImportBytes)
	tmp, err := os.CreateTemp("", "vault-import-*.zip")
	if err != nil {
		log.Printf("vault import temp file error tenant=%s: %v", tenantID, err)
		jsonError(w, "import failed", http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	size, err := io.Copy(tmp, r.Body)
	if err != nil {
		// MaxBytesReader reports an over-limit upload as *http.MaxBytesError. That is
		// a 413, not a 400: the request was well-formed, just too large, and a client
		// that sees 400 has no signal to split or shrink the archive.
		if isBodyTooLarge(err) {
			log.Printf("vault import over size limit tenant=%s limit=%d: %v",
				tenantID, maxVaultImportBytes, err)
			jsonError(w, "upload exceeds the "+strconv.Itoa(maxVaultImportBytes)+" byte limit",
				http.StatusRequestEntityTooLarge)
			return
		}
		log.Printf("vault import read body error tenant=%s: %v", tenantID, err)
		jsonError(w, "failed to read upload", http.StatusBadRequest)
		return
	}
	if size == 0 {
		jsonError(w, "empty upload", http.StatusBadRequest)
		return
	}

	zr, err := zip.NewReader(tmp, size)
	if err != nil {
		log.Printf("vault import open zip error tenant=%s: %v", tenantID, err)
		jsonError(w, "invalid zip archive", http.StatusBadRequest)
		return
	}

	s.auditAction(r, tenantID, auditActor, "vault.import", "", true, "admin basic auth")

	var imported, skipped, failed int
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return
		}
		if f.FileInfo().IsDir() || !strings.HasSuffix(f.Name, ".md") {
			continue
		}

		rec, perr := parseVaultMarkdown(f)
		if perr != nil {
			log.Printf("vault import parse error tenant=%s entry=%s: %v", tenantID, f.Name, perr)
			failed++
			continue
		}
		if rec.ID == "" || !validID.MatchString(rec.ID) {
			failed++
			continue
		}

		// Tenant ownership: the export tags each record with its origin tenant.
		// Refuse records whose frontmatter tenant disagrees with the target so a
		// caller can't smuggle another tenant's data in under their own header.
		if rec.Tenant != "" && rec.Tenant != tenantID {
			skipped++
			continue
		}
		if rec.AgentID != "" && rec.AgentID != "unspecified" && !validID.MatchString(rec.AgentID) {
			failed++
			continue
		}

		// Skip duplicates already present in active working memory.
		if exists, _ := s.rdb.SIsMember(ctx, vecSetKey(tenantID), rec.ID).Result(); exists {
			skipped++
			continue
		}

		// Embeddings are not part of the export format — re-embed the body text
		// so the restored record is vector-searchable. Records without text
		// cannot be embedded; skip them rather than store a dead vector.
		if rec.Text == "" {
			skipped++
			continue
		}
		emb, model, eerr := s.embedTextWithModel(ctx, rec.Text)
		if eerr != nil {
			log.Printf("vault import embed error tenant=%s id=%s: %v", tenantID, rec.ID, eerr)
			failed++
			continue
		}

		agentID := rec.AgentID
		if agentID == "unspecified" {
			agentID = ""
		}

		// The body is NOT also written into metadata.text.
		//
		// An earlier import wrote rec.Text to both places, so a restored record
		// carried a second full copy of its body inside the metadata blob —
		// doubling the Redis footprint of every imported memory. No reader
		// needed it: the export resolves the body through vaultRecordText,
		// which prefers the canonical top-level "text" field and only falls
		// back to metadata.text for records an older import wrote that way, and
		// the canonical field is set on the storedVector below. The metadata
		// blob is the caller's own metadata, so it is stored as the archive
		// carried it, untouched.
		metaMap := rec.Metadata
		if metaMap == nil {
			metaMap = map[string]interface{}{}
		}
		// A stale "text" from the archive's own metadata block is dropped: the
		// body is recovered from the Markdown body, and leaving the archived
		// copy in place would resurrect exactly the duplicate this avoids.
		delete(metaMap, "text")
		metaJSON, merr := json.Marshal(metaMap)
		if merr != nil {
			metaJSON = []byte(`{}`)
		}

		// The archive is caller-supplied, so its frontmatter tags reach the same
		// store path as /store input and must clear the same taxonomy. Without
		// this, an import could write tag sets that /store refuses — tags
		// stored only because they arrived through the restore door, and
		// thereafter a permanent exception to the rule the API enforces. The
		// record counts as failed (not skipped) so the caller sees it in the
		// summary rather than silently losing data it believed it restored.
		if err := validateTags(rec.Tags); err != nil {
			log.Printf("vault import invalid tags tenant=%s id=%s: %v", tenantID, rec.ID, err)
			failed++
			continue
		}

		// Normalize created_at before it is stored.
		//
		// The value comes from the archive's YAML frontmatter and is written to
		// Redis, used as the promotion-ordering key, and echoed back on export,
		// so anything that is not a real timestamp becomes a permanent
		// unorderable record. Two distinct cases:
		//
		//   - empty: the export omits created_at only for records written before
		//     the field existed. Substituting now is the same default /store
		//     applies, and there is no information to report, so it is silent.
		//   - non-empty but unparseable: the archive says something about when
		//     this record was made and we cannot honour it. Falling back to now
		//     keeps the record usable; the log line is what makes the loss
		//     visible, because the alternative (rejecting the record) would fail
		//     an entire restore over one bad frontmatter line.
		createdAt := normalizeImportedCreatedAt(rec.CreatedAt, func(orig, fallback string) {
			log.Printf("vault import unparseable created_at tenant=%s id=%s was=%q using=%q",
				tenantID, rec.ID, orig, fallback)
		})

		sv := storedVector{
			ID:      rec.ID,
			AgentID: agentID,
			Tags:    rec.Tags,
			// Populate the canonical top-level "text" hash field. Without it an
			// imported record carries its body only under metadata.text, and
			// every other reader — /search and /query result hydration,
			// /session/<id> context — reads data["text"] and returns an empty
			// body for it.
			Text:      rec.Text,
			Embedding: emb,
			Metadata:  json.RawMessage(metaJSON),
			CreatedAt: createdAt,
			Model:     model,
		}

		// Share lifecycle and projection serialization with ordinary writes and
		// forget, so an in-flight import cannot recreate a deleted projection.
		storeErr, indexErr := s.storeWithIndexes(ctx, tenantID, sv)
		if err := storeErr; err != nil {
			log.Printf("vault import storeVec error tenant=%s id=%s: %v", tenantID, rec.ID, err)
			failed++
			continue
		}
		if indexErr != nil {
			log.Printf("vault import indexing incomplete tenant=%s id=%s: %v", tenantID, rec.ID, indexErr)
		}

		imported++
	}

	// Record a usage event so the audit trail reflects the import. Imported
	// records are counted as stores ('import' is not in the usage_events
	// event_type CHECK constraint — using 'store' avoids a schema migration
	// and a runtime INSERT failure).
	if _, dbErr := s.db.ExecContext(ctx,
		`INSERT INTO usage_events (tenant_id, event_type, vectors_stored) VALUES (?, 'store', ?)`,
		tenantID, imported); dbErr != nil {
		log.Printf("usage_events write failed tenant=%s: %v", tenantID, dbErr)
	}

	log.Printf("vault import tenant=%s imported=%d skipped=%d failed=%d",
		tenantID, imported, skipped, failed)
	respondJSON(w, map[string]interface{}{
		"imported": imported,
		"skipped":  skipped,
		"failed":   failed,
	})
}

// vaultRecord is a single record parsed back out of an export Markdown file.
type vaultRecord struct {
	ID        string
	Tenant    string
	AgentID   string
	CreatedAt string
	Tags      []string
	Text      string
	Metadata  map[string]interface{}
}

// parseVaultMarkdown reverses buildMarkdown: it reads one zip entry and extracts
// the frontmatter fields, body text, and the trailing ```json metadata block.
// The reader is closed before returning. Robust to missing optional fields.
//
// The read is bounded by maxVaultEntryBytes: the archive-level cap covers the
// compressed upload, so without a per-entry limit a small zip could decompress
// into unbounded memory here. The declared-size check below is what rejects an
// oversize entry in practice; see the comment on it for why the bounded read is
// retained as defence rather than relied on as the gate.
func parseVaultMarkdown(f *zip.File) (vaultRecord, error) {
	var rec vaultRecord
	rc, err := f.Open()
	if err != nil {
		return rec, err
	}
	defer rc.Close()

	// The declared size is the check that actually rejects an oversize entry, and
	// it is sufficient on its own — but not for the reason a per-entry cap usually
	// is. archive/zip refuses to decompress past the size the entry declares:
	// checksumReader.Read returns ErrFormat the moment it has produced more bytes
	// than f.UncompressedSize64 (archive/zip/reader.go). So an attacker-controlled
	// header that *understates* the size cannot smuggle an oversize payload in —
	// the read ends at the declared boundary and fails, and this function rejects
	// the record on that error. An honest header is caught here directly.
	//
	// The bounded read below is therefore defence in depth rather than the
	// enforcement, and it is deliberately not tested as though it were: while
	// archive/zip enforces the declared size, no input can reach it. It is kept
	// because it does not depend on that reader behaviour, so it still bounds the
	// buffer if archive/zip ever stops enforcing it. Reading one byte past the cap
	// distinguishes "exactly at the limit" from "over it" instead of silently
	// truncating, which would corrupt the record rather than reject it.
	//
	// Mutation-checked, because "defence in depth" is also what an unreachable
	// guard looks like: delete any one guard on its own — the declared-size
	// check, the LimitReader, or the len(raw) check — and the suite stays
	// green, the remaining guards catching each fixture alone. Delete the
	// LimitReader and the len(raw) check together and it still stays green:
	// the declared-size check rejects the honest oversize fixture, and
	// archive/zip itself errors on the understated one. Delete all three and
	// TestParseVaultMarkdownRejectsOversizeEntry fails, the entry then being
	// accepted whole. So what the tests pin is that at least one guard always
	// rejects an honest oversize entry; no single guard is load-bearing.
	if f.UncompressedSize64 > maxVaultEntryBytes {
		return rec, fmt.Errorf("entry %q declares %d bytes, over the %d byte limit",
			f.Name, f.UncompressedSize64, maxVaultEntryBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(rc, maxVaultEntryBytes+1))
	if err != nil {
		return rec, err
	}
	if int64(len(raw)) > maxVaultEntryBytes {
		return rec, fmt.Errorf("entry %q exceeds the %d byte limit", f.Name, maxVaultEntryBytes)
	}
	content := string(raw)

	// Split off the YAML frontmatter delimited by the leading "---\n ... \n---".
	if !strings.HasPrefix(content, "---\n") {
		return rec, fmt.Errorf("missing frontmatter")
	}
	rest := content[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return rec, fmt.Errorf("unterminated frontmatter")
	}
	frontmatter := rest[:end]
	body := rest[end+len("\n---\n"):]

	for _, line := range strings.Split(frontmatter, "\n") {
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		switch key {
		case "id":
			rec.ID = val
		case "tenant":
			rec.Tenant = val
		case "agent_id":
			rec.AgentID = val
		case "created_at":
			rec.CreatedAt = val
		case "tags":
			rec.Tags = parseFrontmatterTags(val)
		}
	}

	// Body holds the text followed by a "## Metadata" section with a json block.
	// Split on the metadata marker; everything before it (trimmed) is the text.
	metaJSON := ""
	if mi := strings.Index(body, "\n## Metadata\n"); mi >= 0 {
		rec.Text = strings.TrimSpace(body[:mi])
		metaSection := body[mi:]
		if open := strings.Index(metaSection, "```json\n"); open >= 0 {
			after := metaSection[open+len("```json\n"):]
			if fence := strings.Index(after, "\n```"); fence >= 0 {
				metaJSON = after[:fence]
			}
		}
	} else {
		rec.Text = strings.TrimSpace(body)
	}

	if metaJSON != "" {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(metaJSON), &m); err == nil {
			rec.Metadata = m
		}
	}

	return rec, nil
}

// parseFrontmatterTags decodes the YAML flow-style array written by
// buildMarkdown, e.g. "[a, b, c]". Returns nil for an empty or malformed value.
// normalizeImportedCreatedAt returns a stored-form created_at for a record
// being restored, and reports unparseable input through onFallback so the
// caller can log it with the record id.
//
// Stored form is RFC3339, the format /store writes and the format every later
// reader parses (consolidation's promotion ordering, demote's activation age).
// The accepted inputs beyond RFC3339 are the shapes an archive in the wild
// actually carries: RFC3339 with a lowercase t/z, a date with no time, and
// SQLite's own "YYYY-MM-DD HH:MM:SS". Everything is emitted as RFC3339 UTC so
// the stored value is uniform regardless of what the frontmatter held.
//
// Unparseable input is not an error: a restore is a bulk operation and one bad
// frontmatter line must not fail it, and rejecting the record would discard the
// memory the caller is trying to recover. onFallback may be nil.
func normalizeImportedCreatedAt(raw string, onFallback func(orig, fallback string)) string {
	fallback := time.Now().UTC().Format(time.RFC3339)
	if raw == "" {
		return fallback
	}
	// Legacy corpus exports preserve Unix seconds; keep their age/provenance
	// instead of treating a valid old timestamp as a newly created memory.
	if parsed, ok := parseCreatedAt(raw); ok {
		return parsed.UTC().Format(time.RFC3339)
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05", // RFC3339 without a zone
		"2006-01-02 15:04:05", // SQLite datetime()
		"2006-01-02",          // date only
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	if onFallback != nil {
		onFallback(raw, fallback)
	}
	return fallback
}

func parseFrontmatterTags(val string) []string {
	val = strings.TrimSpace(val)
	val = strings.TrimPrefix(val, "[")
	val = strings.TrimSuffix(val, "]")
	if strings.TrimSpace(val) == "" {
		return nil
	}
	parts := strings.Split(val, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// /agents — method dispatcher
// GET    → handleListAgents  (returns distinct agents seen in last 7 days)
// DELETE → handleDeleteAgent (removes usage_events + audit_log rows for one agent)
// ---------------------------------------------------------------------------

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		handleCORSPreflight(w, r)
	case http.MethodGet:
		s.handleListAgents(w, r)
	case http.MethodDelete:
		s.handleDeleteAgent(w, r)
	default:
		setCORSHeaders(w)
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---------------------------------------------------------------------------
// GET /agents?tenant_id=...
// Returns distinct agent_ids seen in usage_events over the last 7 days.
// Rows with NULL agent_id are reported as "unspecified".
// last_seen_at is emitted as ISO-8601 with a Z suffix so JS always parses it
// as UTC regardless of the client's local timezone.
// ---------------------------------------------------------------------------

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	// CORS preflight is handled by the dispatcher; guard against direct calls.
	if handleCORSPreflight(w, r) {
		return
	}

	tenantID := r.URL.Query().Get("tenant_id")
	if tenantID == "" || !validID.MatchString(tenantID) {
		jsonError(w, "missing or invalid tenant_id", http.StatusBadRequest)
		return
	}

	// Same auth paths as the vault handlers (see resolveVaultCaller).
	if _, ok := s.resolveVaultCaller(w, r, tenantID); !ok {
		return
	}

	ctx := r.Context()
	crossAgent := crossAgentEnabled(s.config)

	// Lookback window. Was hardcoded to 7 days, which left the dashboard's
	// "last 7 days" label as the only possible view. days=0 means all time.
	days, err := parseAgentDays(r.URL.Query().Get("days"))
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	// strftime with '%Y-%m-%dT%H:%M:%SZ' emits ISO-8601 UTC so the JS client
	// parses the timestamp unambiguously regardless of local timezone.
	//
	// The window is interpolated as a SQLite modifier string rather than bound
	// as a parameter because datetime('now', ?) with a bound value is not
	// usable here -- SQLite treats the modifier as a literal. parseAgentDays
	// therefore returns an int from a fixed allowlist, never caller text, so
	// nothing user-controlled reaches the statement. tenant_id stays bound.
	window := "AND created_at >= datetime('now', '-" + strconv.Itoa(days) + " days')"
	if days == 0 {
		window = "" // all time
	}
	query := `
		SELECT
			COALESCE(agent_id, 'unspecified')                        AS agent_id,
			strftime('%Y-%m-%dT%H:%M:%SZ', MAX(created_at))         AS last_seen_at,
			COUNT(*)                                                  AS events
		FROM usage_events
		WHERE tenant_id = ?
		  ` + window + `
		GROUP BY agent_id
		ORDER BY last_seen_at DESC
	`

	rows, err := s.db.QueryContext(ctx, query, tenantID)
	if err != nil {
		log.Printf("agents query error tenant=%s: %v", tenantID, err)
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type agentRow struct {
		AgentID    string `json:"agent_id"`
		LastSeenAt string `json:"last_seen_at"`
		Events     int64  `json:"events"`

		// Phase 7 (cooperative) corpus controls — populated only when the flag
		// is on, so the dashboard can render toggle/name/note. Omitted when off.
		Isolated    bool   `json:"isolated,omitempty"`
		DisplayName string `json:"display_name,omitempty"`
		Note        string `json:"note,omitempty"`
	}

	agents := make([]agentRow, 0)
	for rows.Next() {
		var a agentRow
		if err := rows.Scan(&a.AgentID, &a.LastSeenAt, &a.Events); err != nil {
			log.Printf("agents scan error tenant=%s: %v", tenantID, err)
			continue
		}
		if crossAgent && a.AgentID != "" && a.AgentID != "unspecified" {
			st := getAgentSettings(ctx, s.rdb, tenantID, a.AgentID)
			a.Isolated, a.DisplayName, a.Note = st.Isolated, st.DisplayName, st.Note
		}
		agents = append(agents, a)
	}
	if err := rows.Err(); err != nil {
		log.Printf("agents rows error tenant=%s: %v", tenantID, err)
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}

	// visibility_enabled lets the dashboard gate the per-agent isolation
	// controls so it never renders a toggle that would silently no-op / 404
	// when the feature is off. Honest UI: no control unless enforcement is live.
	respondJSON(w, map[string]interface{}{
		"agents":             agents,
		"visibility_enabled": crossAgent,
		"days":               days,
	})
}

// ---------------------------------------------------------------------------
// DELETE /agents?tenant_id=<uuid>&agent_id=<string>
// Removes all usage_events and audit_log rows for the given agent within the
// tenant. The special value "unspecified" maps to agent_id IS NULL rows.
// Redis vector data is intentionally NOT touched in this iteration.
// ---------------------------------------------------------------------------

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	// CORS preflight is handled by the dispatcher; guard against direct calls.
	if handleCORSPreflight(w, r) {
		return
	}

	tenantID := r.URL.Query().Get("tenant_id")
	agentID := r.URL.Query().Get("agent_id")

	if tenantID == "" || !validID.MatchString(tenantID) {
		setCORSHeaders(w)
		jsonError(w, "missing or invalid tenant_id", http.StatusBadRequest)
		return
	}
	// agent_id must be non-empty; "unspecified" is accepted as a sentinel.
	if agentID == "" {
		setCORSHeaders(w)
		jsonError(w, "missing agent_id", http.StatusBadRequest)
		return
	}
	// Validate charset for all values except the sentinel.
	if agentID != "unspecified" && !validID.MatchString(agentID) {
		setCORSHeaders(w)
		jsonError(w, "invalid agent_id", http.StatusBadRequest)
		return
	}

	if _, ok := s.resolveVaultCaller(w, r, tenantID); !ok {
		return
	}

	ctx := r.Context()

	// "unspecified" is a UI sentinel for rows where agent_id IS NULL.
	// Build parameterised queries that handle both cases without string
	// interpolation.
	var (
		usageResult sql.Result
		auditResult sql.Result
		err         error
	)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("delete agent begin tx error tenant=%s agent=%s: %v", tenantID, agentID, err)
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	if agentID == "unspecified" {
		usageResult, err = tx.ExecContext(ctx,
			`DELETE FROM usage_events WHERE tenant_id = ? AND agent_id IS NULL`,
			tenantID)
		if err == nil {
			auditResult, err = tx.ExecContext(ctx,
				`DELETE FROM audit_log WHERE tenant_id = ? AND agent_id IS NULL`,
				tenantID)
		}
	} else {
		usageResult, err = tx.ExecContext(ctx,
			`DELETE FROM usage_events WHERE tenant_id = ? AND agent_id = ?`,
			tenantID, agentID)
		if err == nil {
			auditResult, err = tx.ExecContext(ctx,
				`DELETE FROM audit_log WHERE tenant_id = ? AND agent_id = ?`,
				tenantID, agentID)
		}
	}

	if err != nil {
		log.Printf("delete agent exec error tenant=%s agent=%s: %v", tenantID, agentID, err)
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("delete agent commit error tenant=%s agent=%s: %v", tenantID, agentID, err)
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}

	usageDeleted, _ := usageResult.RowsAffected()
	auditDeleted, _ := auditResult.RowsAffected()

	log.Printf("delete agent tenant=%s agent=%s usage_events=%d audit_log=%d",
		tenantID, agentID, usageDeleted, auditDeleted)

	respondJSON(w, map[string]interface{}{
		"removed":              true,
		"usage_events_deleted": usageDeleted,
		"audit_log_deleted":    auditDeleted,
	})
}

// agentDaysAllowed is the set of lookback windows GET /agents accepts.
//
// An allowlist rather than a numeric range because the value is interpolated
// into a SQLite datetime modifier (see handleAgents): keeping it to known-good
// integers means no caller-supplied text can reach the statement, and it also
// stops an unbounded window from turning the query into a full-table scan on a
// large usage_events table. 0 = all time.
var agentDaysAllowed = map[int]bool{0: true, 1: true, 7: true, 30: true, 90: true, 365: true}

// parseAgentDays validates the ?days= parameter. Empty means the historical
// default of 7, so existing callers are unaffected.
func parseAgentDays(raw string) (int, error) {
	if raw == "" {
		return 7, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || !agentDaysAllowed[n] {
		return 0, fmt.Errorf("invalid days (allowed: 0 for all time, 1, 7, 30, 90, 365)")
	}
	return n, nil
}
