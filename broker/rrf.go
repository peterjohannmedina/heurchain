package main

import (
	"encoding/json"
	"sort"
)

const defaultVectorSize = 768

var defaultQdrantClient = NewQdrantClient("http://localhost:6333", defaultVectorSize)

// SearchResult is a single document returned by the fused RRF search.
type SearchResult struct {
	ID                string                 `json:"id"`
	RRFScore          float64                `json:"rrf_score"`
	IdentifierMatches *int                   `json:"identifier_matches,omitempty"`
	BM25Score         *float64               `json:"bm25_score,omitempty"`
	QdrantScore       *float32               `json:"qdrant_score,omitempty"`
	BM25Rank          int                    `json:"bm25_rank,omitempty"`
	QdrantRank        int                    `json:"qdrant_rank,omitempty"`
	AgentID           string                 `json:"agent_id,omitempty"`
	Tags              []string               `json:"tags,omitempty"`
	Text              string                 `json:"text,omitempty"`
	Metadata          json.RawMessage        `json:"metadata,omitempty"`
	Payload           map[string]interface{} `json:"payload,omitempty"`
}

// rrfScore computes the RRF score given the two ranks (1-indexed, 0 means absent).
func rrfScore(bm25Rank, qdrantRank int) float64 {
	const k = 60.0
	var score float64
	if bm25Rank > 0 {
		score += 1.0 / (k + float64(bm25Rank))
	}
	if qdrantRank > 0 {
		score += 1.0 / (k + float64(qdrantRank))
	}
	return score
}

// fuseResults merges BM25 and Qdrant ranked lists using RRF.
// Returns results sorted by RRFScore descending, top-n only.
func fuseResults(bm25Scores map[string]float64, qdrantHits []QdrantSearchResult, topN int) []SearchResult {
	options := RecallOptions{}
	_ = options.normalize()
	return fuseResultsWithOptions(bm25Scores, qdrantHits, topN, options)
}

func fuseResultsWithOptions(bm25Scores map[string]float64, qdrantHits []QdrantSearchResult, topN int, options RecallOptions) []SearchResult {
	// Build BM25 rank map (1-indexed, sorted by score descending).
	type bm25Doc struct {
		id    string
		score float64
	}
	bm25Docs := make([]bm25Doc, 0, len(bm25Scores))
	for id, score := range bm25Scores {
		if score <= 0 {
			continue
		}
		bm25Docs = append(bm25Docs, bm25Doc{id: id, score: score})
	}
	sort.Slice(bm25Docs, func(i, j int) bool {
		if bm25Docs[i].score == bm25Docs[j].score {
			return bm25Docs[i].id < bm25Docs[j].id
		}
		return bm25Docs[i].score > bm25Docs[j].score
	})
	bm25Ranks := make(map[string]int, len(bm25Docs))
	for i, doc := range bm25Docs {
		bm25Ranks[doc.id] = i + 1
	}

	// Build Qdrant rank map (1-indexed, already ranked by score descending).
	qdrantRanks := make(map[string]int, len(qdrantHits))
	qdrantByID := make(map[string]QdrantSearchResult, len(qdrantHits))
	for i, hit := range qdrantHits {
		qdrantRanks[hit.ID] = i + 1
		if _, exists := qdrantByID[hit.ID]; !exists {
			qdrantByID[hit.ID] = hit
		}
	}

	// Union of all document IDs.
	allDocs := make(map[string]struct{}, len(bm25Scores)+len(qdrantHits))
	for id := range bm25Ranks {
		allDocs[id] = struct{}{}
	}
	for _, hit := range qdrantHits {
		allDocs[hit.ID] = struct{}{}
	}

	results := make([]SearchResult, 0, len(allDocs))
	for id := range allDocs {
		sr := SearchResult{ID: id}

		bm25Rank, hasBM25 := bm25Ranks[id]
		qdrantRank, hasQdrant := qdrantRanks[id]

		if hasBM25 {
			score := bm25Scores[id]
			sr.BM25Score = &score
			sr.BM25Rank = bm25Rank
		}
		if hasQdrant {
			hit := qdrantByID[id]
			score := hit.Score
			sr.QdrantScore = &score
			sr.QdrantRank = qdrantRank
			sr.Payload = hit.Payload
		}

		if hasBM25 {
			sr.RRFScore += options.LexicalWeight / (options.RRFK + float64(bm25Rank))
		}
		if hasQdrant {
			sr.RRFScore += options.DenseWeight / (options.RRFK + float64(qdrantRank))
		}
		results = append(results, sr)
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].RRFScore == results[j].RRFScore {
			return results[i].ID < results[j].ID
		}
		return results[i].RRFScore > results[j].RRFScore
	})

	if topN > 0 && len(results) > topN {
		results = results[:topN]
	}
	return results
}
