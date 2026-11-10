package main

import (
	"context"
	"database/sql"
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
	graphMaxSeeds      = 32
	graphFanout        = 32
	graphMaxEdges      = 256
	graphMaxCandidates = 100
	graphReadTimeout   = 300 * time.Millisecond
)

// graphReadBudget is the deadline traverseGraph applies. It is
// graphReadTimeout in every production build; only race-instrumented test
// binaries widen it (graph_race_test.go).
var graphReadBudget = graphReadTimeout

type graphReadContext struct {
	db      *sql.DB
	enabled bool
}
type graphCitation struct {
	indexComplete bool
	RelationID    string  `json:"relation_id"`
	SourceID      string  `json:"source_id"`
	TargetID      string  `json:"target_id"`
	Predicate     string  `json:"predicate"`
	MemoryID      string  `json:"source_memory_id"`
	Evidence      string  `json:"evidence"`
	SourceHash    string  `json:"-"`
	Activation    float64 `json:"activation"`
}
type graphReadResult struct {
	Ranks     map[string]int
	Paths     map[string][]graphCitation
	Seeds     int
	Examined  int
	Truncated bool
}

// Use whole, normalized phrases: "ann" must never seed "joanne". The
// phrase and seed budgets also bound user-controlled SQL parameters.
func graphQueryPhrases(query string) []string {
	words := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' && r != '-' })
	if len(words) > 64 {
		words = words[:64]
	}
	seen := map[string]bool{}
	out := []string{}
	for width := 1; width <= min(8, len(words)); width++ {
		for start := 0; start+width <= len(words) && len(out) < 256; start++ {
			p := strings.Join(words[start:start+width], " ")
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

func graphSeeds(ctx context.Context, db *sql.DB, tenant, query string, explicit []string) ([]string, error) {
	if len(explicit) > graphMaxSeeds {
		return nil, fmt.Errorf("too many graph seeds")
	}
	var rows *sql.Rows
	var err error
	if len(explicit) > 0 {
		args := []interface{}{tenant}
		marks := []string{}
		for _, id := range explicit {
			args = append(args, id)
			marks = append(marks, "?")
		}
		rows, err = db.QueryContext(ctx, `SELECT id FROM graph_entities WHERE tenant_id=? AND id IN (`+strings.Join(marks, ",")+`) ORDER BY id LIMIT 32`, args...)
	} else {
		phrases := graphQueryPhrases(query)
		if len(phrases) == 0 {
			return nil, nil
		}
		args := []interface{}{tenant}
		marks := []string{}
		for _, p := range phrases {
			args = append(args, p)
			marks = append(marks, "?")
		}
		in := strings.Join(marks, ",")
		args = append(args, tenant)
		for _, p := range phrases {
			args = append(args, p)
		}
		rows, err = db.QueryContext(ctx, `SELECT id FROM graph_entities WHERE tenant_id=? AND lower(name) IN (`+in+`) UNION SELECT entity_id FROM graph_aliases WHERE tenant_id=? AND lower(alias) IN (`+in+`) ORDER BY 1 LIMIT 32`, args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seeds := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		seeds = append(seeds, id)
	}
	return seeds, rows.Err()
}

// Graph eligibility is checked on every edge before it can bridge to another
// entity. SQLite is a derived index; the active plaintext Redis corpus wins.
func graphSourceEligible(ctx context.Context, rdb *redis.Client, tenant, agent string, tags []string, scope RecallOptions, edge graphCitation) (bool, float64, error) {
	member, err := rdb.SIsMember(ctx, vecSetKey(tenant), edge.MemoryID).Result()
	if err != nil || !member {
		return false, 0, err
	}
	// A forgotten source never bridges entities, even from resurrected residue.
	if forgotten, err := memoryForgotten(ctx, rdb, tenant, edge.MemoryID); err != nil || forgotten {
		return false, 0, err
	}
	data, err := rdb.HGetAll(ctx, vecKey(tenant, edge.MemoryID)).Result()
	if err != nil {
		return false, 0, err
	}
	if !graphPlaintextEligible(data) || data["silo"] != "" || edge.SourceHash == "" || graphSourceFingerprint(data) != edge.SourceHash {
		return false, 0, nil
	}
	if (agent != "" && data["agent_id"] != agent) || !tagsIntersect(parseTags(data["tags"]), tags) || !recallMetadataEligible(data["metadata"], scope) || !matchesMemoryContext(ctx, edge.MemoryID, data) {
		return false, 0, nil
	}
	created, err := time.Parse(time.RFC3339, data["created_at"])
	if err != nil {
		return false, 0, nil
	}
	count, _ := strconv.Atoi(data["access_count"])
	activation := computeEdgeActivation(count, max(0, time.Since(created).Seconds()), data["namespace"])
	return activation >= heurgraphCfg.ActivationCutoff, activation, nil
}

func graphPathEligible(ctx context.Context, rdb *redis.Client, tenant, agent string, tags []string, scope RecallOptions, path []graphCitation) (bool, error) {
	for _, edge := range path {
		ok, _, err := graphSourceEligible(ctx, rdb, tenant, agent, tags, scope, edge)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func traverseGraph(ctx context.Context, rdb *redis.Client, db *sql.DB, tenant, query, agent string, tags []string, scope RecallOptions, explicit []string, limit int) (graphReadResult, error) {
	out := graphReadResult{Ranks: map[string]int{}, Paths: map[string][]graphCitation{}}
	if db == nil {
		return out, fmt.Errorf("graph database unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, graphReadBudget)
	defer cancel()
	seeds, err := graphSeeds(ctx, db, tenant, query, explicit)
	if err != nil {
		return out, err
	}
	out.Seeds = len(seeds)
	type node struct {
		id    string
		path  []graphCitation
		score float64
	}
	frontier := []node{}
	visited := map[string]bool{}
	for _, id := range seeds {
		frontier = append(frontier, node{id: id})
		visited[id] = true
	}
	scores := map[string]float64{}
	hops := scope.GraphHops
	if hops <= 0 {
		hops = 2
	}
	hops = min(hops, 2)
	limit = min(max(limit, 1), graphMaxCandidates)
	for depth := 0; depth < hops && len(frontier) > 0; depth++ {
		next := []node{}
		for _, current := range frontier {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			if out.Examined >= graphMaxEdges {
				out.Truncated = true
				break
			}
			// Fetch one extra row to expose fanout truncation. Both endpoint branches
			// use the tenant/source and tenant/target indexes. Incomplete writes
			// count against the budget, so filtering cannot hide fanout truncation.
			rows, err := db.QueryContext(ctx, `WITH outgoing AS (SELECT id FROM graph_relations WHERE tenant_id=? AND source_id=? ORDER BY id LIMIT ?), incoming AS (SELECT id FROM graph_relations WHERE tenant_id=? AND target_id=? ORDER BY id LIMIT ?), adjacent AS (SELECT id FROM outgoing UNION SELECT id FROM incoming) SELECT r.id,r.source_id,r.target_id,r.predicate,r.source_memory_id,r.source_hash,r.evidence,(x.source_memory_id IS NOT NULL AND r.src_vaulted=0) FROM adjacent a JOIN graph_relations r ON r.tenant_id=? AND r.id=a.id LEFT JOIN graph_extractions x ON x.tenant_id=r.tenant_id AND x.source_memory_id=r.source_memory_id AND x.source_hash=r.source_hash AND x.status='complete' ORDER BY r.id LIMIT ?`, tenant, current.id, graphFanout+1, tenant, current.id, graphFanout+1, tenant, graphFanout+1)
			if err != nil {
				return out, err
			}
			edges := []graphCitation{}
			for rows.Next() {
				var e graphCitation
				if err := rows.Scan(&e.RelationID, &e.SourceID, &e.TargetID, &e.Predicate, &e.MemoryID, &e.SourceHash, &e.Evidence, &e.indexComplete); err != nil {
					rows.Close()
					return out, err
				}
				edges = append(edges, e)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return out, err
			}
			if len(edges) > graphFanout {
				edges = edges[:graphFanout]
				out.Truncated = true
			}
			for _, edge := range edges {
				if out.Examined >= graphMaxEdges {
					out.Truncated = true
					break
				}
				out.Examined++
				if !edge.indexComplete {
					continue
				}
				ok, activation, err := graphSourceEligible(ctx, rdb, tenant, agent, tags, scope, edge)
				if err != nil {
					return out, err
				}
				if !ok {
					continue
				}
				edge.Activation = activation
				path := append(append([]graphCitation{}, current.path...), edge)
				score := activation - float64(depth)
				if depth > 0 {
					score = math.Min(score, current.score-1)
				}
				if old, found := scores[edge.MemoryID]; !found || score > old {
					scores[edge.MemoryID] = score
					out.Paths[edge.MemoryID] = path
				}
				neighbor := edge.TargetID
				if neighbor == current.id {
					neighbor = edge.SourceID
				}
				if !visited[neighbor] {
					visited[neighbor] = true
					next = append(next, node{neighbor, path, score})
				}
			}
		}
		frontier = next
	}
	ids := []string{}
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] == scores[ids[j]] {
			return ids[i] < ids[j]
		}
		return scores[ids[i]] > scores[ids[j]]
	})
	if len(ids) > limit {
		ids = ids[:limit]
		out.Truncated = true
	}
	paths := map[string][]graphCitation{}
	for _, id := range ids {
		ok, err := graphPathEligible(ctx, rdb, tenant, agent, tags, scope, out.Paths[id])
		if err != nil {
			return out, err
		}
		if ok {
			out.Ranks[id] = len(out.Ranks) + 1
			paths[id] = out.Paths[id]
		}
	}
	out.Paths = paths
	return out, nil
}
