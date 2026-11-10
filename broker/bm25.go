package main

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/redis/go-redis/v9"
)

const (
	bm25K1 = 1.5
	bm25B  = 0.75

	// maxTermsPerDoc bounds how many distinct terms a single document may
	// contribute to the index. Each indexed term costs three Redis commands
	// and one key whose name embeds tenantID+docID, all queued client-side
	// before Exec — so an unbounded term count is an unbounded allocation in
	// broker heap, independent of how short the docID is. Documents above the
	// cap are indexed on their most frequent terms, which is where BM25's
	// signal lives anyway.
	maxTermsPerDoc = 2000

	// maxQueryTerms bounds the query side. scoreBM25 issues one Get per
	// (doc, term) pair, so query length multiplies the corpus scan.
	maxQueryTerms = 64

	// maxBM25ScoreDocs is the largest document batch scoreBM25 sends in one
	// Redis pipeline. The public /search limit is smaller, but scoreBM25 is
	// also used by /query and remains callable for larger fixed document sets.
	maxBM25ScoreDocs = 128

	// bm25ScanBatchSize bounds the amount of active/posting-set work retained
	// between Redis round trips. SSCAN COUNT is a hint, so callers also split a
	// larger returned page before issuing any metadata or scoring pipeline.
	bm25ScanBatchSize = 128

	// Keep an internal caller mistake from turning the bounded candidate path
	// into an effectively unbounded heap. The parent /search path already caps
	// its request limits below this value.
	maxBM25CandidateLimit = 200
)

// tokenize splits text on whitespace, lowercases, and strips punctuation.
func tokenize(text string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Fields(b.String())
}

func termFreqs(tokens []string) map[string]int {
	freqs := make(map[string]int, len(tokens))
	for _, t := range tokens {
		freqs[t]++
	}
	return freqs
}

// capTermFreqs limits a document to its maxTermsPerDoc most frequent terms.
// Below the cap the map is returned untouched, so ordinary documents are
// unaffected and their scoring is bit-for-bit identical to before.
func capTermFreqs(freqs map[string]int, limit int) map[string]int {
	if len(freqs) <= limit {
		return freqs
	}
	type termFreq struct {
		term string
		freq int
	}
	ranked := make([]termFreq, 0, len(freqs))
	for term, freq := range freqs {
		ranked = append(ranked, termFreq{term, freq})
	}
	// Sort by frequency desc, then term asc so the retained set is
	// deterministic for a given document rather than map-order dependent.
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].freq != ranked[j].freq {
			return ranked[i].freq > ranked[j].freq
		}
		return ranked[i].term < ranked[j].term
	})
	capped := make(map[string]int, limit)
	for _, tf := range ranked[:limit] {
		capped[tf.term] = tf.freq
	}
	return capped
}

// dedupeQueryTerms removes repeats and bounds query length. scoreBM25 issues a
// Get per (doc, term); repeated terms in the query multiplied that cost while
// contributing nothing, since BM25 scores a term once regardless of how often
// the *query* repeats it.
func dedupeQueryTerms(terms []string, limit int) []string {
	seen := make(map[string]struct{}, len(terms))
	out := make([]string, 0, len(terms))
	for _, t := range terms {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func bm25DFKey(tenantID, term string) string {
	return fmt.Sprintf("tenant:%s:bm25:df:%s", tenantID, term)
}

func bm25TFKey(tenantID, docID, term string) string {
	return fmt.Sprintf("tenant:%s:bm25:tf:%s:%s", tenantID, docID, term)
}

func bm25MetaDocCountKey(tenantID string) string {
	return fmt.Sprintf("tenant:%s:bm25:meta:doc_count", tenantID)
}

func bm25MetaAvgDLKey(tenantID string) string {
	return fmt.Sprintf("tenant:%s:bm25:meta:avgdl", tenantID)
}

func bm25DocTermsKey(tenantID, docID string) string {
	return fmt.Sprintf("tenant:%s:bm25:terms:%s", tenantID, docID)
}

func bm25DocLenKey(tenantID, docID string) string {
	return fmt.Sprintf("tenant:%s:bm25:dl:%s", tenantID, docID)
}

func bm25DFPrefix(tenantID string) string {
	return fmt.Sprintf("tenant:%s:bm25:df:", tenantID)
}

func bm25TFPrefix(tenantID, docID string) string {
	return fmt.Sprintf("tenant:%s:bm25:tf:%s:", tenantID, docID)
}

// bm25PostingKey is a lexicographically ordered set of document IDs for a term.
// Sorted postings let multi-term queries merge and deduplicate streams without
// a corpus-sized seen map or repeatedly scoring the same document.
// The existing per-document TF keys remain the source used by scoreBM25;
// postings only answer "which documents are worth scoring?".
func bm25PostingKey(tenantID, term string) string {
	return fmt.Sprintf("tenant:%s:bm25:postings:v2:%s", tenantID, term)
}

// bm25PostingsKey is kept as a descriptive alias for callers and tests.
func bm25PostingsKey(tenantID, term string) string {
	return bm25PostingKey(tenantID, term)
}

func bm25PostingsPrefix(tenantID string) string {
	return fmt.Sprintf("tenant:%s:bm25:postings:v2:", tenantID)
}

// A tenant is allowed to use the postings-only path only after a complete
// active-set backfill. Missing, "0", and any other value intentionally mean
// incomplete so legacy data falls back to paged scoring.
func bm25PostingsCompleteKey(tenantID string) string {
	return fmt.Sprintf("tenant:%s:bm25:meta:postings_complete:v2", tenantID)
}

func bm25BackfillCompleteKey(tenantID string) string {
	return bm25PostingsCompleteKey(tenantID)
}

var bm25UpdateScript = redis.NewScript(`
local termsKey, dlKey = KEYS[1], KEYS[2]
local countKey, avgKey = KEYS[3], KEYS[4]
local docID, newDL = ARGV[1], tonumber(ARGV[2])
local dfPrefix, tfPrefix, postingPrefix = ARGV[3], ARGV[4], ARGV[5]

-- /store writes the source hash before it updates BM25. If a slower older
-- request reaches this script after a newer replacement, do not let its text
-- roll the lexical projection back. Legacy maintenance/tests may seed only
-- BM25 keys, so the guard is intentionally bypassed when the source hash is
-- absent.
if redis.call('EXISTS', KEYS[6]) == 1 then return 0 end
if redis.call('EXISTS', KEYS[5]) == 0 and redis.call('EXISTS', KEYS[7]) == 1 then return 0 end
local sourceText = redis.call('HGET', KEYS[5], 'text') or ''
if sourceText == '' then
  local ok, meta = pcall(cjson.decode, redis.call('HGET', KEYS[5], 'metadata') or '{}')
  if ok and type(meta) == 'table' and type(meta.text) == 'string' then sourceText = meta.text end
end
if redis.call('EXISTS', KEYS[5]) == 1 and sourceText ~= ARGV[6] then
  return 0
end

local oldTerms = redis.call('SMEMBERS', termsKey)
local oldDL = tonumber(redis.call('GET', dlKey) or '0')
local docCount = tonumber(redis.call('GET', countKey) or '0')
local avgdl = tonumber(redis.call('GET', avgKey) or '0')

for _, term in ipairs(oldTerms) do
  local dfKey = dfPrefix .. term
  local nextDF = tonumber(redis.call('DECR', dfKey) or '0')
  if nextDF <= 0 then redis.call('DEL', dfKey) end
  redis.call('DEL', tfPrefix .. term)
  local postingKey = postingPrefix .. term
  redis.call('ZREM', postingKey, docID)
end
redis.call('DEL', termsKey)

local newCount = docCount
if oldDL <= 0 then newCount = docCount + 1 end
local totalTokens = avgdl * docCount - oldDL + newDL
local newAvg = 0
if newCount > 0 then newAvg = totalTokens / newCount end

local i = 7
while i <= #ARGV do
  local term, tf = ARGV[i], ARGV[i + 1]
  redis.call('INCR', dfPrefix .. term)
  redis.call('SET', tfPrefix .. term, tf)
  redis.call('SADD', termsKey, term)
  redis.call('ZADD', postingPrefix .. term, 0, docID)
  i = i + 2
end
redis.call('SET', dlKey, newDL)
redis.call('SET', countKey, newCount)
redis.call('SET', avgKey, newAvg)
return 1
`)

var bm25RemoveScript = redis.NewScript(`
local termsKey, dlKey = KEYS[1], KEYS[2]
local countKey, avgKey = KEYS[3], KEYS[4]
local docID = ARGV[1]
local dfPrefix, tfPrefix, postingPrefix = ARGV[2], ARGV[3], ARGV[4]

if redis.call('EXISTS', KEYS[6]) == 1 then return 0 end
if redis.call('EXISTS', KEYS[5]) == 0 and redis.call('EXISTS', KEYS[7]) == 1 then return 0 end
local sourceText = redis.call('HGET', KEYS[5], 'text') or ''
if sourceText == '' then
  local ok, meta = pcall(cjson.decode, redis.call('HGET', KEYS[5], 'metadata') or '{}')
  if ok and type(meta) == 'table' and type(meta.text) == 'string' then sourceText = meta.text end
end
if redis.call('EXISTS', KEYS[5]) == 1 and sourceText ~= ARGV[5] then
  return 0
end

local oldTerms = redis.call('SMEMBERS', termsKey)
local oldDL = tonumber(redis.call('GET', dlKey) or '0')
for _, term in ipairs(oldTerms) do
  local dfKey = dfPrefix .. term
  local nextDF = tonumber(redis.call('DECR', dfKey) or '0')
  if nextDF <= 0 then redis.call('DEL', dfKey) end
  redis.call('DEL', tfPrefix .. term)
  local postingKey = postingPrefix .. term
  redis.call('ZREM', postingKey, docID)
end
redis.call('DEL', termsKey, dlKey)

if oldDL > 0 then
  local docCount = tonumber(redis.call('GET', countKey) or '0')
  local avgdl = tonumber(redis.call('GET', avgKey) or '0')
  local newCount = docCount - 1
  if newCount < 0 then newCount = 0 end
  local newAvg = 0
  if newCount > 0 and docCount > 0 then
    newAvg = (avgdl * docCount - oldDL) / newCount
  end
  redis.call('SET', countKey, newCount)
  redis.call('SET', avgKey, newAvg)
end
return 1
`)

// updateBM25Index adds or updates a document in the tenant's BM25 index.
// The Lua transaction removes old TF/DF/posting state and writes the new state
// together with doc_count/avgdl. This avoids the read-compute-write race that
// previously let concurrent stores corrupt running statistics.
func updateBM25Index(ctx context.Context, rdb *redis.Client, tenantID, docID, text string) error {
	tokens := tokenize(text)
	dl := len(tokens)
	if dl == 0 {
		if err := removeBM25Index(ctx, rdb, tenantID, docID, text); err != nil {
			return fmt.Errorf("bm25 remove empty document: %w", err)
		}
		return nil
	}
	freqs := capTermFreqs(termFreqs(tokens), maxTermsPerDoc)
	args := make([]interface{}, 0, 6+len(freqs)*2)
	args = append(args, docID, dl, bm25DFPrefix(tenantID), bm25TFPrefix(tenantID, docID), bm25PostingsPrefix(tenantID), text)
	for term, freq := range freqs {
		args = append(args, term, freq)
	}
	keys := []string{
		bm25DocTermsKey(tenantID, docID),
		bm25DocLenKey(tenantID, docID),
		bm25MetaDocCountKey(tenantID),
		bm25MetaAvgDLKey(tenantID),
		vecKey(tenantID, docID),
		memoryTombstoneKey(tenantID, docID),
		memoryVaultKey(tenantID, docID),
	}
	result, err := bm25UpdateScript.Run(ctx, rdb, keys, args...).Int()
	if err != nil {
		return fmt.Errorf("bm25 atomic update: %w", err)
	}
	if result == 0 {
		// A newer active source revision won the race. Its own store path or the
		// semantic/index reconciler will perform the corresponding update.
		return nil
	}
	return nil
}

func removeBM25Index(ctx context.Context, rdb *redis.Client, tenantID, docID string, expectedText ...string) error {
	keys := []string{
		bm25DocTermsKey(tenantID, docID),
		bm25DocLenKey(tenantID, docID),
		bm25MetaDocCountKey(tenantID),
		bm25MetaAvgDLKey(tenantID),
		vecKey(tenantID, docID),
		memoryTombstoneKey(tenantID, docID),
		memoryVaultKey(tenantID, docID),
	}
	expected := ""
	if len(expectedText) > 0 {
		expected = expectedText[0]
	}
	args := []interface{}{docID, bm25DFPrefix(tenantID), bm25TFPrefix(tenantID, docID), bm25PostingsPrefix(tenantID), expected}
	result, err := bm25RemoveScript.Run(ctx, rdb, keys, args...).Int()
	if err != nil {
		return fmt.Errorf("bm25 atomic remove: %w", err)
	}
	if result == 0 {
		return nil
	}
	return nil
}

var bm25MoveScript = redis.NewScript(`
local fields = redis.call('HGETALL', KEYS[1])
local docID = ARGV[1]
local dfPrefix, tfPrefix, postingPrefix = ARGV[2], ARGV[3], ARGV[4]

-- Clean the lexical projection even if a stale vec-set member has already
-- lost its hash. This makes a retry/idempotent vault operation unable to leave
-- a searchable orphan behind.
local terms = redis.call('SMEMBERS', KEYS[4])
local dl = tonumber(redis.call('GET', KEYS[5]) or '0')
for _, term in ipairs(terms) do
  local dfKey = dfPrefix .. term
  local nextDF = tonumber(redis.call('DECR', dfKey) or '0')
  if nextDF <= 0 then redis.call('DEL', dfKey) end
  redis.call('DEL', tfPrefix .. term)
  local postingKey = postingPrefix .. term
  redis.call('ZREM', postingKey, docID)
end
redis.call('DEL', KEYS[4], KEYS[5])
if dl > 0 then
  local count = tonumber(redis.call('GET', KEYS[6]) or '0')
  local avg = tonumber(redis.call('GET', KEYS[7]) or '0')
  local newCount = count - 1
  if newCount < 0 then newCount = 0 end
  local newAvg = 0
  if newCount > 0 and count > 0 then
    newAvg = (avg * count - dl) / newCount
  end
  redis.call('SET', KEYS[6], newCount)
  redis.call('SET', KEYS[7], newAvg)
end

-- A vault transition also invalidates the semantic projection. Keep this
-- outbox write in the same script as the active-set move so a Qdrant outage
-- cannot turn an archive into a permanently stale vector hit. KEYS[8] is the
-- global semantic outbox, deliberately outside the lexical term loop above.
redis.call('ZADD', KEYS[8], ARGV[5], ARGV[6])

if #fields == 0 then
  redis.call('SREM', KEYS[3], docID)
  return 0
end
redis.call('DEL', KEYS[2])
redis.call('HSET', KEYS[2], unpack(fields))
redis.call('DEL', KEYS[1])
redis.call('SREM', KEYS[3], docID)
return 1
`)

// moveActiveToVault atomically moves the Redis working-set hash and removes
// all v1 BM25 state, including term postings. The cleanup happens inside the
// same script as the active-set removal, so a successful vault transition
// cannot leave a lexical result behind.
func moveActiveToVault(ctx context.Context, rdb *redis.Client, tenantID, docID string) (bool, error) {
	keys := []string{
		vecKey(tenantID, docID),
		fmt.Sprintf("tenant:%s:vault:%s", tenantID, docID),
		vecSetKey(tenantID),
		bm25DocTermsKey(tenantID, docID),
		bm25DocLenKey(tenantID, docID),
		bm25MetaDocCountKey(tenantID),
		bm25MetaAvgDLKey(tenantID),
		semanticPendingKey,
	}
	args := []interface{}{
		docID,
		bm25DFPrefix(tenantID),
		bm25TFPrefix(tenantID, docID),
		bm25PostingsPrefix(tenantID),
		time.Now().UnixMilli(),
		semanticMember(tenantID, docID),
	}
	result, err := bm25MoveScript.Run(ctx, rdb, keys, args...).Int()
	if err != nil {
		return false, fmt.Errorf("move active to vault: %w", err)
	}
	return result == 1, nil
}

// bm25TermScore is the shared BM25 contribution. It deliberately returns zero
// for malformed/missing values so candidate generation can retain only actual
// positive lexical matches.
func bm25TermScore(tf, df float64, dl, docCount int, avgdl float64) float64 {
	if tf <= 0 || df <= 0 || dl <= 0 || docCount <= 0 || avgdl <= 0 {
		return 0
	}
	idf := math.Log((float64(docCount)-df+0.5)/(df+0.5) + 1)
	numerator := tf * (bm25K1 + 1)
	denominator := tf + bm25K1*(1-bm25B+bm25B*(float64(dl)/avgdl))
	if denominator == 0 {
		return 0
	}
	return idf * (numerator / denominator)
}

// scoreBM25 computes BM25 scores for the given documents against query terms.
// It remains a fixed-document scorer for /query, but every Redis pipeline is
// capped at maxBM25ScoreDocs so callers cannot accidentally create a
// corpus-sized command batch.
func scoreBM25(ctx context.Context, rdb *redis.Client, tenantID string, queryTerms []string, docIDs []string) (map[string]float64, error) {
	queryTerms = dedupeQueryTerms(queryTerms, maxQueryTerms)
	scores := make(map[string]float64)
	const documentsPerBatch = 128
	for start := 0; start < len(docIDs); start += documentsPerBatch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+documentsPerBatch, len(docIDs))
		batch, err := scoreBM25Batch(ctx, rdb, tenantID, queryTerms, docIDs[start:end])
		if err != nil {
			return nil, err
		}
		for id, score := range batch {
			scores[id] = score
		}
	}
	return scores, nil
}

func scoreBM25Batch(ctx context.Context, rdb *redis.Client, tenantID string, queryTerms []string, docIDs []string) (map[string]float64, error) {
	pipe := rdb.Pipeline()
	docCountCmd := pipe.Get(ctx, bm25MetaDocCountKey(tenantID))
	avgdlCmd := pipe.Get(ctx, bm25MetaAvgDLKey(tenantID))

	dfCmds := make([]*redis.StringCmd, len(queryTerms))
	for i, term := range queryTerms {
		dfCmds[i] = pipe.Get(ctx, bm25DFKey(tenantID, term))
	}

	tfCmds := make([]*redis.StringCmd, 0, len(docIDs)*len(queryTerms))
	for _, docID := range docIDs {
		for _, term := range queryTerms {
			tfCmds = append(tfCmds, pipe.Get(ctx, bm25TFKey(tenantID, docID, term)))
		}
	}

	dlCmds := make([]*redis.StringCmd, len(docIDs))
	for i, docID := range docIDs {
		dlCmds[i] = pipe.Get(ctx, bm25DocLenKey(tenantID, docID))
	}

	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("bm25 score pipeline: %w", err)
	}

	docCountStr, err := docCountCmd.Result()
	if err != nil && err != redis.Nil {
		return nil, fmt.Errorf("bm25 doc count: %w", err)
	}
	avgdlStr, err := avgdlCmd.Result()
	if err != nil && err != redis.Nil {
		return nil, fmt.Errorf("bm25 average length: %w", err)
	}
	docCount := 0
	if docCountStr != "" {
		docCount, _ = strconv.Atoi(docCountStr)
	}
	avgdl := 0.0
	if avgdlStr != "" {
		avgdl, _ = strconv.ParseFloat(avgdlStr, 64)
	}
	if docCount == 0 || avgdl == 0 {
		return map[string]float64{}, nil
	}

	dfs := make([]float64, len(queryTerms))
	for i, cmd := range dfCmds {
		value, err := cmd.Result()
		if err != nil && err != redis.Nil {
			return nil, fmt.Errorf("bm25 document frequency: %w", err)
		}
		if value != "" {
			dfs[i], _ = strconv.ParseFloat(value, 64)
		}
	}

	scores := make(map[string]float64, len(docIDs))
	tfIdx := 0
	for docIdx, docID := range docIDs {
		dl := 0
		value, err := dlCmds[docIdx].Result()
		if err != nil && err != redis.Nil {
			return nil, fmt.Errorf("bm25 document length: %w", err)
		}
		if value != "" {
			dl, _ = strconv.Atoi(value)
		}
		if dl == 0 {
			tfIdx += len(queryTerms)
			continue
		}

		score := 0.0
		for termIdx := range queryTerms {
			value, err := tfCmds[tfIdx].Result()
			if err != nil && err != redis.Nil {
				return nil, fmt.Errorf("bm25 term frequency: %w", err)
			}
			if value != "" {
				tf, _ := strconv.ParseFloat(value, 64)
				score += bm25TermScore(tf, dfs[termIdx], dl, docCount, avgdl)
			}
			tfIdx++
		}
		if score > 0 {
			scores[docID] = score
		}
	}
	return scores, nil
}

type bm25Candidate struct {
	docID string
	score float64
}

// bm25CandidateHeap is a min-heap whose root is the worst retained result:
// lower scores lose first, and larger IDs lose deterministic ties first.
// Its size is the only cross-page candidate state kept by the inverted read.
type bm25CandidateHeap []bm25Candidate

func (h bm25CandidateHeap) Len() int      { return len(h) }
func (h bm25CandidateHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h bm25CandidateHeap) Less(i, j int) bool {
	if h[i].score != h[j].score {
		return h[i].score < h[j].score
	}
	return h[i].docID > h[j].docID
}

func (h *bm25CandidateHeap) Push(value interface{}) {
	*h = append(*h, value.(bm25Candidate))
}

func (h *bm25CandidateHeap) Pop() interface{} {
	old := *h
	n := len(old)
	value := old[n-1]
	*h = old[:n-1]
	return value
}

func bm25CandidateBetter(a, b bm25Candidate) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	return a.docID < b.docID
}

func retainBM25Candidate(h *bm25CandidateHeap, candidate bm25Candidate, limit int) {
	if limit <= 0 || candidate.score <= 0 || math.IsNaN(candidate.score) || math.IsInf(candidate.score, 0) {
		return
	}
	for i, existing := range *h {
		if existing.docID != candidate.docID {
			continue
		}
		if bm25CandidateBetter(candidate, existing) {
			(*h)[i] = candidate
			heap.Init(h)
		}
		return
	}
	if len(*h) < limit {
		heap.Push(h, candidate)
		return
	}
	if len(*h) > 0 && bm25CandidateBetter(candidate, (*h)[0]) {
		heap.Pop(h)
		heap.Push(h, candidate)
	}
}

func bm25CandidateScores(h bm25CandidateHeap) map[string]float64 {
	scores := make(map[string]float64, len(h))
	for _, candidate := range h {
		scores[candidate.docID] = candidate.score
	}
	return scores
}

func normalizeBM25QueryTerms(terms []string) []string {
	deduped := dedupeQueryTerms(terms, maxQueryTerms)
	if len(deduped) == 0 {
		return nil
	}
	clean := make([]string, 0, len(deduped))
	for _, term := range deduped {
		if strings.TrimSpace(term) != "" {
			clean = append(clean, term)
		}
	}
	return clean
}

func bm25PostingsReady(ctx context.Context, rdb *redis.Client, tenantID string) (bool, error) {
	value, err := rdb.Get(ctx, bm25PostingsCompleteKey(tenantID)).Result()
	if err == redis.Nil {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read bm25 postings readiness: %w", err)
	}
	return value == "1", nil
}

// BM25PostingsReady is the read-only migration readiness check. It is true
// only after BackfillBM25Postings has completed a full active-set SSCAN and
// committed the marker; callers must use the legacy fallback while false.
func BM25PostingsReady(ctx context.Context, rdb *redis.Client, tenantID string) (bool, error) {
	return bm25PostingsReady(ctx, rdb, tenantID)
}

// markBM25PostingsComplete is intentionally separate from the page worker.
// An incremental worker must call it only after its tenant SSCAN reaches
// cursor zero and every page has succeeded; a partial page must never make
// the postings-only reader look complete.
func markBM25PostingsComplete(ctx context.Context, rdb *redis.Client, tenantID string) error {
	if strings.TrimSpace(tenantID) == "" {
		return fmt.Errorf("bm25 readiness requires one tenant id")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("mark bm25 postings complete: %w", err)
	}
	if err := rdb.Set(ctx, bm25PostingsCompleteKey(tenantID), "1", 0).Err(); err != nil {
		return fmt.Errorf("mark bm25 postings complete: %w", err)
	}
	return nil
}

// MarkBM25PostingsComplete is the worker hook for an incremental backfill:
// call it once, and only once, after backfillBM25Page has succeeded for every
// active-set page through cursor zero.
func MarkBM25PostingsComplete(ctx context.Context, rdb *redis.Client, tenantID string) error {
	return markBM25PostingsComplete(ctx, rdb, tenantID)
}

var bm25EnsurePostingsScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[4]) == 1 then return -1 end
if redis.call('SISMEMBER', KEYS[2], ARGV[1]) == 0 or redis.call('EXISTS', KEYS[3]) == 0 then
  return -1
end
local terms = redis.call('SMEMBERS', KEYS[1])
local added = 0
for _, term in ipairs(terms) do
  redis.call('ZADD', ARGV[2] .. term, 0, ARGV[1])
  added = added + 1
end
return added
`)

// ensureBM25Postings adds postings for a document whose existing per-document
// TF/terms state is authoritative. It does not touch DF, TF, doc length, or
// average length, which is what makes legacy backfill safe when the body text
// is absent or lives only in metadata.
func ensureBM25Postings(ctx context.Context, rdb *redis.Client, tenantID, docID string) error {
	_, err := bm25EnsurePostingsScript.Run(ctx, rdb, []string{
		bm25DocTermsKey(tenantID, docID),
		vecSetKey(tenantID),
		vecKey(tenantID, docID),
		memoryTombstoneKey(tenantID, docID),
	}, docID, bm25PostingsPrefix(tenantID)).Int()
	if err != nil {
		return fmt.Errorf("ensure bm25 postings for %q: %w", docID, err)
	}
	return nil
}

// filterBM25ActiveDocs verifies the source hash and active-set membership,
// then applies the optional filters. It runs before a document reaches the
// bounded heap, so low-quality/unwanted early postings cannot consume the
// caller's limit. All command batches are page-sized, and hash reads are
// field-scoped (memoryFilterFields) so the embedding never leaves Redis here.
func filterBM25ActiveDocs(ctx context.Context, rdb *redis.Client, tenantID, agentID string, tags []string, docIDs []string) ([]string, error) {
	if len(docIDs) == 0 {
		return nil, nil
	}

	// SSCAN may repeat an ID. De-duplicate only this bounded page; never build
	// a tenant-sized union of postings in broker memory.
	unique := make([]string, 0, len(docIDs))
	seen := make(map[string]struct{}, len(docIDs))
	for _, docID := range docIDs {
		if _, ok := seen[docID]; ok {
			continue
		}
		seen[docID] = struct{}{}
		unique = append(unique, docID)
	}

	pipe := rdb.Pipeline()
	activeCmds := make([]*redis.BoolCmd, len(unique))
	existsCmds := make([]*redis.IntCmd, len(unique))
	var metadataCmds []*redis.SliceCmd
	var sourceCmds []*redis.SliceCmd
	scope, _ := ctx.Value(recallScopeContextKey{}).(RecallOptions)
	if ctx.Value(memoryFilterContextKey{}) != nil || scope.AsOf != "" || scope.MemoryKind != "" {
		sourceCmds = make([]*redis.SliceCmd, len(unique))
	}
	if agentID != "" || len(tags) > 0 {
		metadataCmds = make([]*redis.SliceCmd, len(unique))
	}
	for i, docID := range unique {
		activeCmds[i] = pipe.SIsMember(ctx, vecSetKey(tenantID), docID)
		existsCmds[i] = pipe.Exists(ctx, vecKey(tenantID, docID))
		if metadataCmds != nil {
			metadataCmds[i] = pipe.HMGet(ctx, vecKey(tenantID, docID), "agent_id", "tags")
		}
		if sourceCmds != nil {
			sourceCmds[i] = pipe.HMGet(ctx, vecKey(tenantID, docID), memoryFilterFields...)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("filter bm25 candidates: %w", err)
	}

	filtered := make([]string, 0, len(unique))
	for i, docID := range unique {
		active, err := activeCmds[i].Result()
		if err != nil {
			return nil, fmt.Errorf("check active bm25 candidate %q: %w", docID, err)
		}
		if !active {
			continue
		}
		exists, err := existsCmds[i].Result()
		if err != nil {
			return nil, fmt.Errorf("check bm25 candidate hash %q: %w", docID, err)
		}
		if exists == 0 {
			continue
		}
		if sourceCmds != nil {
			values, err := sourceCmds[i].Result()
			if err != nil {
				return nil, fmt.Errorf("read structured bm25 filters: %w", err)
			}
			data := hashFieldMap(memoryFilterFields, values)
			if !matchesMemoryContext(ctx, docID, data) || !recallMetadataEligible(data["metadata"], scope) {
				continue
			}
		}
		if metadataCmds == nil {
			filtered = append(filtered, docID)
			continue
		}

		values, err := metadataCmds[i].Result()
		if err != nil {
			return nil, fmt.Errorf("read bm25 candidate metadata %q: %w", docID, err)
		}
		var agent string
		var rawTags string
		if len(values) > 0 {
			agent, _ = values[0].(string)
		}
		if len(values) > 1 {
			rawTags, _ = values[1].(string)
		}
		if agentID != "" && agent != agentID {
			continue
		}
		if !tagsIntersect(parseTags(rawTags), tags) {
			continue
		}
		filtered = append(filtered, docID)
	}
	return filtered, nil
}

func scoreBM25CandidatePage(ctx context.Context, rdb *redis.Client, tenantID, agentID string, tags, terms, docIDs []string, limit int, top *bm25CandidateHeap) error {
	filtered, err := filterBM25ActiveDocs(ctx, rdb, tenantID, agentID, tags, docIDs)
	if err != nil {
		return err
	}
	if len(filtered) == 0 {
		return nil
	}
	scores, err := scoreBM25(ctx, rdb, tenantID, terms, filtered)
	if err != nil {
		return fmt.Errorf("score bm25 candidate page: %w", err)
	}
	for _, docID := range filtered {
		if score, ok := scores[docID]; ok {
			retainBM25Candidate(top, bm25Candidate{docID: docID, score: score}, limit)
		}
	}
	return nil
}

func scanBM25CandidateSet(ctx context.Context, rdb *redis.Client, tenantID, agentID string, tags, terms []string, limit int, top *bm25CandidateHeap, keyForTerm func(string) string) error {
	streams := make([]bm25PostingStream, len(terms))
	for i, term := range terms {
		streams[i] = bm25PostingStream{key: keyForTerm(term), min: "-"}
	}
	page := make([]string, 0, bm25ScanBatchSize)
	for {
		best := ""
		for i := range streams {
			id, err := streams[i].peek(ctx, rdb)
			if err != nil {
				return err
			}
			if id != "" && (best == "" || id < best) {
				best = id
			}
		}
		if best == "" {
			return scoreBM25CandidatePage(ctx, rdb, tenantID, agentID, tags, terms, page, limit, top)
		}
		// Consume the ID from every matching stream. Every document is scored
		// once even if it contains every query term; buffers are <=64*128 IDs.
		for i := range streams {
			s := &streams[i]
			if s.at < len(s.ids) && s.ids[s.at] == best {
				s.at++
				s.min = "(" + best
			}
		}
		page = append(page, best)
		if len(page) == bm25ScanBatchSize {
			if err := scoreBM25CandidatePage(ctx, rdb, tenantID, agentID, tags, terms, page, limit, top); err != nil {
				return err
			}
			page = page[:0]
		}
	}
}

type bm25PostingStream struct {
	key, min string
	ids      []string
	at       int
	done     bool
}

func (s *bm25PostingStream) peek(ctx context.Context, rdb *redis.Client) (string, error) {
	if s.at == len(s.ids) && !s.done {
		ids, err := rdb.ZRangeByLex(ctx, s.key, &redis.ZRangeBy{Min: s.min, Max: "+", Count: bm25ScanBatchSize}).Result()
		if err != nil {
			return "", fmt.Errorf("read bm25 posting page: %w", err)
		}
		s.ids, s.at = ids, 0
		s.done = len(ids) == 0
	}
	if s.done {
		return "", nil
	}
	return s.ids[s.at], nil
}

func scanBM25LegacyCandidates(ctx context.Context, rdb *redis.Client, tenantID, agentID string, tags, terms []string, limit int, top *bm25CandidateHeap) error {
	var cursor uint64
	for {
		docIDs, next, err := rdb.SScan(ctx, vecSetKey(tenantID), cursor, "", int64(bm25ScanBatchSize)).Result()
		if err != nil {
			return fmt.Errorf("scan legacy bm25 corpus: %w", err)
		}
		for start := 0; start < len(docIDs); start += bm25ScanBatchSize {
			end := start + bm25ScanBatchSize
			if end > len(docIDs) {
				end = len(docIDs)
			}
			if err := scoreBM25CandidatePage(ctx, rdb, tenantID, agentID, tags, terms, docIDs[start:end], limit, top); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return nil
}

// searchBM25Candidates returns positive BM25 scores for at most limit active
// documents. Once the explicit completeness marker is present it scans only
// term postings. Before then it scans the active vector set with SSCAN pages,
// preserving legacy lexical recall without an all-document allocation.
func searchBM25Candidates(ctx context.Context, rdb *redis.Client, tenantID, agentID string, tags, terms []string, limit int) (map[string]float64, error) {
	limit = normalizeBM25CandidateLimit(limit)
	if limit == 0 {
		return map[string]float64{}, nil
	}
	terms = normalizeBM25QueryTerms(terms)
	if len(terms) == 0 {
		return map[string]float64{}, nil
	}
	ready, err := bm25PostingsReady(ctx, rdb, tenantID)
	if err != nil {
		return nil, err
	}

	top := bm25CandidateHeap{}
	heap.Init(&top)
	if ready {
		err = scanBM25CandidateSet(ctx, rdb, tenantID, agentID, tags, terms, limit, &top,
			func(term string) string { return bm25PostingKey(tenantID, term) })
	} else {
		err = scanBM25LegacyCandidates(ctx, rdb, tenantID, agentID, tags, terms, limit, &top)
	}
	if err != nil {
		return nil, err
	}
	return bm25CandidateScores(top), nil
}

func normalizeBM25CandidateLimit(limit int) int {
	if limit <= 0 {
		return 0
	}
	if limit > maxBM25CandidateLimit {
		return maxBM25CandidateLimit
	}
	return limit
}

// backfillBM25Postings rebuilds the term posting sets from active Redis
// hashes. It is intentionally tenant-scoped and paged. The readiness marker
// is deleted before work starts and is set to "1" only after the complete
// SSCAN, every active hash read, and every BM25 update succeeds. Cancellation,
// Redis errors, or a missing active hash leave the tenant on the safe legacy
// fallback path.
func backfillBM25Postings(ctx context.Context, rdb *redis.Client, tenantID string) (scanned, indexed int, err error) {
	if strings.TrimSpace(tenantID) == "" {
		return 0, 0, fmt.Errorf("bm25 backfill requires one tenant id")
	}
	if err := rdb.Del(ctx, bm25PostingsCompleteKey(tenantID)).Err(); err != nil {
		return 0, 0, fmt.Errorf("start bm25 backfill: %w", err)
	}

	var cursor uint64
	for {
		docIDs, next, scanErr := rdb.SScan(ctx, vecSetKey(tenantID), cursor, "", int64(bm25ScanBatchSize)).Result()
		if scanErr != nil {
			return scanned, indexed, fmt.Errorf("scan bm25 backfill corpus: %w", scanErr)
		}
		for start := 0; start < len(docIDs); start += bm25ScanBatchSize {
			end := start + bm25ScanBatchSize
			if end > len(docIDs) {
				end = len(docIDs)
			}
			pageScanned, pageIndexed, pageErr := backfillBM25Page(ctx, rdb, tenantID, docIDs[start:end])
			scanned += pageScanned
			indexed += pageIndexed
			if pageErr != nil {
				return scanned, indexed, pageErr
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return scanned, indexed, fmt.Errorf("bm25 backfill incomplete: %w", err)
	}
	if err := markBM25PostingsComplete(ctx, rdb, tenantID); err != nil {
		return scanned, indexed, err
	}
	return scanned, indexed, nil
}

func backfillBM25Page(ctx context.Context, rdb *redis.Client, tenantID string, docIDs []string) (scanned, indexed int, err error) {
	if len(docIDs) == 0 {
		return 0, 0, nil
	}
	unique := make([]string, 0, len(docIDs))
	seen := make(map[string]struct{}, len(docIDs))
	for _, docID := range docIDs {
		if _, ok := seen[docID]; ok {
			continue
		}
		seen[docID] = struct{}{}
		unique = append(unique, docID)
	}

	pipe := rdb.Pipeline()
	activeCmds := make([]*redis.BoolCmd, len(unique))
	existsCmds := make([]*redis.IntCmd, len(unique))
	metadataCmds := make([]*redis.SliceCmd, len(unique))
	oldTermsCmds := make([]*redis.StringSliceCmd, len(unique))
	oldDLCmds := make([]*redis.StringCmd, len(unique))
	for i, docID := range unique {
		activeCmds[i] = pipe.SIsMember(ctx, vecSetKey(tenantID), docID)
		existsCmds[i] = pipe.Exists(ctx, vecKey(tenantID, docID))
		metadataCmds[i] = pipe.HMGet(ctx, vecKey(tenantID, docID), "text", "metadata")
		oldTermsCmds[i] = pipe.SMembers(ctx, bm25DocTermsKey(tenantID, docID))
		oldDLCmds[i] = pipe.Get(ctx, bm25DocLenKey(tenantID, docID))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return len(unique), 0, fmt.Errorf("read bm25 backfill page: %w", err)
	}

	for i, docID := range unique {
		scanned++
		active, err := activeCmds[i].Result()
		if err != nil {
			return scanned, indexed, fmt.Errorf("check bm25 backfill membership %q: %w", docID, err)
		}
		if !active {
			continue
		}
		exists, err := existsCmds[i].Result()
		if err != nil {
			return scanned, indexed, fmt.Errorf("check bm25 backfill hash %q: %w", docID, err)
		}
		if exists == 0 {
			return scanned, indexed, fmt.Errorf("bm25 backfill active document %q has no Redis hash", docID)
		}
		oldTerms, err := oldTermsCmds[i].Result()
		if err != nil && err != redis.Nil {
			return scanned, indexed, fmt.Errorf("read bm25 backfill terms %q: %w", docID, err)
		}
		oldDL, err := oldDLCmds[i].Result()
		if err != nil && err != redis.Nil {
			return scanned, indexed, fmt.Errorf("read bm25 backfill length %q: %w", docID, err)
		}
		if len(oldTerms) > 0 || oldDL != "" {
			// Existing TF/terms state is authoritative. Only fill the new
			// term->doc projection; recomputing from a missing/stale body would
			// corrupt legacy statistics or erase a text-less lexical index.
			if err := ensureBM25Postings(ctx, rdb, tenantID, docID); err != nil {
				return scanned, indexed, err
			}
			indexed++
			continue
		}

		values, err := metadataCmds[i].Result()
		if err != nil {
			return scanned, indexed, fmt.Errorf("read bm25 backfill source %q: %w", docID, err)
		}
		text := ""
		metadata := ""
		if len(values) > 0 {
			text, _ = values[0].(string)
		}
		if len(values) > 1 {
			metadata, _ = values[1].(string)
		}
		if text == "" && metadata != "" {
			var meta map[string]interface{}
			if json.Unmarshal([]byte(metadata), &meta) == nil {
				text, _ = meta["text"].(string)
			}
		}
		if strings.TrimSpace(text) == "" {
			// No lexical body and no old index: this is a valid embedding-only
			// document, not a reason to manufacture zero-score postings.
			continue
		}
		if err := updateBM25Index(ctx, rdb, tenantID, docID, text); err != nil {
			return scanned, indexed, fmt.Errorf("backfill bm25 document %q: %w", docID, err)
		}
		indexed++
	}
	return scanned, indexed, nil
}
