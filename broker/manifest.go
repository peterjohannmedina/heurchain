package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// GET /memory/manifest
//
// Auth: same as /store, /query — X-HeurChain-API-Key + X-HeurChain-Tenant.
//
// Returns a structured summary of what's stored for the tenant. An agent
// should call this BEFORE asking the user for information that might already
// be stored. Single round-trip; do not poll.
//
// Response shape:
//   {
//     "total":      <int>,
//     "tags":       [{"tag": "credential", "count": 3}, ...],
//     "agents":     [{"agent_id": "kimi", "count": 17}, ...],
//     "recent":     [{"id": "...", "tags": [...], "agent_id": "...", "created_at": "..."}, ...],
//     "tag_taxonomy": ["credential", "decision", "fact", "project-context", "system-config"]
//   }
//
// Suggested-tag taxonomy is published here so agents converge on consistent
// tag names. The server does not enforce it — any validateTags()-passing tag
// is accepted.
// ---------------------------------------------------------------------------

const manifestRecentLimit = 10

// suggestedTagTaxonomy is the published list of conventional tag names agents
// should prefer when applicable. Server-side enforcement is not on; this is
// guidance only.
var suggestedTagTaxonomy = []string{
	"credential",
	"decision",
	"fact",
	"project-context",
	"system-config",
	"conversation",
	"code",
}

type manifestTagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

type manifestAgentCount struct {
	AgentID string `json:"agent_id"`
	Count   int    `json:"count"`
}

type manifestRecentItem struct {
	ID        string   `json:"id"`
	AgentID   string   `json:"agent_id,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	CreatedAt string   `json:"created_at"`
}

func (s *Server) handleMemoryManifest(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	setCORSHeaders(w)
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	apiKey := r.Header.Get(s.config.Auth.APIKeyHeader)
	tenantID := r.Header.Get(s.config.Auth.TenantHeader)
	if apiKey == "" {
		jsonError(w, "missing api key", http.StatusUnauthorized)
		return
	}
	if tenantID == "" || !validID.MatchString(tenantID) {
		jsonError(w, "missing or invalid tenant id", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if _, err := validateAPIKey(ctx, s.rdb, s.db, tenantID, apiKey); err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	summary, err := buildManifest(ctx, s.rdb, tenantID)
	if err != nil {
		log.Printf("manifest error tenant=%s: %v", tenantID, err)
		jsonError(w, "manifest failed", http.StatusInternalServerError)
		return
	}

	respondJSON(w, summary)
}

// buildManifest scans the tenant's vector set and produces aggregate counts.
// Shared by /memory/manifest and the tenant-aware discovery augmentation.
func buildManifest(ctx context.Context, rdb *redis.Client, tenantID string) (map[string]interface{}, error) {
	ids, err := rdb.SMembers(ctx, vecSetKey(tenantID)).Result()
	if err != nil {
		return nil, fmt.Errorf("list vectors: %w", err)
	}

	out := map[string]interface{}{
		"total":        len(ids),
		"tags":         []manifestTagCount{},
		"agents":       []manifestAgentCount{},
		"recent":       []manifestRecentItem{},
		"tag_taxonomy": suggestedTagTaxonomy,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}

	if len(ids) == 0 {
		return out, nil
	}

	pipe := rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	forgotten := make([]*redis.IntCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, vecKey(tenantID, id))
		forgotten[i] = pipe.Exists(ctx, memoryTombstoneKey(tenantID, id))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("pipeline fetch: %w", err)
	}

	tagCounts := make(map[string]int)
	agentCounts := make(map[string]int)
	recent := make([]manifestRecentItem, 0, len(ids))
	hidden := 0

	for i, id := range ids {
		// Lifecycle tombstone: not counted, not listed.
		if tombstoneHidden(forgotten[i]) {
			hidden++
			continue
		}
		data, err := cmds[i].Result()
		if err != nil || len(data) == 0 {
			continue
		}

		tags := parseTags(data["tags"])
		for _, t := range tags {
			tagCounts[t]++
		}
		if a := data["agent_id"]; a != "" {
			agentCounts[a]++
		}
		recent = append(recent, manifestRecentItem{
			ID:        id,
			AgentID:   data["agent_id"],
			Tags:      tags,
			CreatedAt: data["created_at"],
		})
	}

	// Sort recent by created_at descending (RFC3339 strings sort lexicographically).
	sort.Slice(recent, func(i, j int) bool {
		return recent[i].CreatedAt > recent[j].CreatedAt
	})
	if len(recent) > manifestRecentLimit {
		recent = recent[:manifestRecentLimit]
	}

	// Sort tag/agent counts: highest count first, stable name tiebreak.
	tagList := make([]manifestTagCount, 0, len(tagCounts))
	for t, c := range tagCounts {
		tagList = append(tagList, manifestTagCount{Tag: t, Count: c})
	}
	sort.Slice(tagList, func(i, j int) bool {
		if tagList[i].Count != tagList[j].Count {
			return tagList[i].Count > tagList[j].Count
		}
		return tagList[i].Tag < tagList[j].Tag
	})

	agentList := make([]manifestAgentCount, 0, len(agentCounts))
	for a, c := range agentCounts {
		agentList = append(agentList, manifestAgentCount{AgentID: a, Count: c})
	}
	sort.Slice(agentList, func(i, j int) bool {
		if agentList[i].Count != agentList[j].Count {
			return agentList[i].Count > agentList[j].Count
		}
		return agentList[i].AgentID < agentList[j].AgentID
	})

	out["total"] = len(ids) - hidden
	out["tags"] = tagList
	out["agents"] = agentList
	out["recent"] = recent
	return out, nil
}

// manifestSummaryLine renders a one-line natural-language summary suitable for
// embedding in the discovery markdown response. Returns "" when the tenant has
// no stored memories.
func manifestSummaryLine(m map[string]interface{}) string {
	total, _ := m["total"].(int)
	if total == 0 {
		return ""
	}
	tags, _ := m["tags"].([]manifestTagCount)
	pieces := make([]string, 0, len(tags))
	for i, tc := range tags {
		if i >= 5 {
			break
		}
		pieces = append(pieces, fmt.Sprintf("%s (%d)", tc.Tag, tc.Count))
	}
	if len(pieces) == 0 {
		return fmt.Sprintf("You have %d stored memories for this tenant (untagged). Query before asking the human.", total)
	}
	tagList, _ := json.Marshal(pieces)
	_ = tagList
	joined := ""
	for i, p := range pieces {
		if i > 0 {
			joined += ", "
		}
		joined += p
	}
	return fmt.Sprintf("You have %d stored memories across these tags: %s. Query the relevant tag before asking the human.", total, joined)
}
