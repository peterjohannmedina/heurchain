package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Phase 7 (cooperative model) — per-agent corpus controls.
//
// A tenant owns several cooperative agents that share one API key. By default
// every agent sees every memory in the tenant (today's behaviour, preserved).
// The dashboard lets a user pick an agent and ISOLATE its corpus — hiding that
// agent's memories from the OTHER agents in the workspace — and give the agent
// a friendly name + a short note.
//
// This is an ORGANIZATION control, NOT a security boundary: the requester's
// agent identity is self-declared (the tenant key is shared and trusted), so
// isolation keeps cooperative corpora tidy; it does not defend against a
// malicious key holder. All of this is gated by features.cross_agent_visibility
// and is a no-op when the flag is off.
// ---------------------------------------------------------------------------

// crossAgentEnabled reads the cooperative-visibility rollout flag.
//
// The originating branch used a typed Features struct; master decodes features
// into map[string]bool (a forward-compatibility bag, see config.go) so a flag
// for an unbuilt stage cannot refuse startup. Reading through one helper keeps
// every call site honest about which key actually gates this.
func crossAgentEnabled(cfg *Config) bool {
	return cfg.Features["cross_agent_visibility"]
}

func agentCfgKey(tenantID, agentID string) string {
	return "tenant:" + tenantID + ":agentcfg:" + agentID
}

func isolatedAgentsKey(tenantID string) string {
	return "tenant:" + tenantID + ":isolated-agents"
}

// AgentSettings is the per-agent corpus config surfaced in the dashboard.
type AgentSettings struct {
	AgentID     string `json:"agent_id"`
	Isolated    bool   `json:"isolated"`
	DisplayName string `json:"display_name,omitempty"`
	Note        string `json:"note,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

func getAgentSettings(ctx context.Context, rdb *redis.Client, tenantID, agentID string) AgentSettings {
	s := AgentSettings{AgentID: agentID}
	data, err := rdb.HGetAll(ctx, agentCfgKey(tenantID, agentID)).Result()
	if err != nil || len(data) == 0 {
		return s
	}
	s.Isolated = data["isolated"] == "1"
	s.DisplayName = data["display_name"]
	s.Note = data["note"]
	s.UpdatedAt = data["updated_at"]
	return s
}

func setAgentSettings(ctx context.Context, rdb *redis.Client, tenantID string, s AgentSettings) error {
	pipe := rdb.Pipeline()
	pipe.HSet(ctx, agentCfgKey(tenantID, s.AgentID), map[string]interface{}{
		"isolated":     boolField(s.Isolated),
		"display_name": s.DisplayName,
		"note":         s.Note,
		"updated_at":   time.Now().UTC().Format(time.RFC3339),
	})
	// Maintain a set of isolated agents so the read filter can load the whole
	// isolation state in one round-trip instead of a per-doc HGET.
	if s.Isolated {
		pipe.SAdd(ctx, isolatedAgentsKey(tenantID), s.AgentID)
	} else {
		pipe.SRem(ctx, isolatedAgentsKey(tenantID), s.AgentID)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func boolField(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// loadIsolatedAgents returns the set of agent_ids whose corpus is isolated.
// Called once per read request (when the flag is on) so the hot-path filter is
// a map lookup, not a Redis call per document.
func loadIsolatedAgents(ctx context.Context, rdb *redis.Client, tenantID string) map[string]bool {
	members, err := rdb.SMembers(ctx, isolatedAgentsKey(tenantID)).Result()
	if err != nil || len(members) == 0 {
		return nil
	}
	m := make(map[string]bool, len(members))
	for _, a := range members {
		m[a] = true
	}
	return m
}

// visibleToRequester decides whether `requester` may see a memory authored by
// `author` (its created_by), given the per-memory visibility and the set of
// isolated agents. Cooperative + back-compat semantics:
//   - legacy record (author == "")            -> visible to everyone
//   - own record (requester == author)        -> visible
//   - explicit share (visibility tenant/agents) -> visible per the share
//   - author's corpus isolated                -> hidden from others
//   - otherwise (author not isolated)         -> visible (cooperative default)
//
// requester == "" means the caller did not declare an agent; callers skip the
// filter entirely in that case (show everything, as today).
func visibleToRequester(requester, author, visibility, visibilityAgentsJSON string, isolated map[string]bool) bool {
	if author == "" {
		return true // legacy corpus — never hidden by enabling the flag
	}
	if requester == author {
		return true
	}
	switch visibility {
	case "tenant":
		return true
	case "agents":
		var agents []string
		if visibilityAgentsJSON != "" {
			_ = json.Unmarshal([]byte(visibilityAgentsJSON), &agents)
		}
		for _, a := range agents {
			if a == requester {
				return true
			}
		}
	}
	// No explicit share: visible unless the author's whole corpus is isolated.
	return !isolated[author]
}

// authorField extracts the authoring agent for visibility purposes: the Phase-7
// created_by provenance, falling back to the legacy agent_id field (older docs
// stamped agent_id but not created_by are still treated as legacy => visible).
func authorField(data map[string]string) string {
	if cb := data["created_by"]; cb != "" {
		return cb
	}
	return "" // no provenance => legacy => visible to all
}

// ---------------------------------------------------------------------------
// HTTP: GET/PUT /agent-settings — API-key authenticated. Gated by the flag.
// ---------------------------------------------------------------------------

func (s *Server) handleAgentSettings(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	setCORSHeaders(w)
	if !crossAgentEnabled(s.config) {
		jsonError(w, "cross-agent visibility is not enabled", http.StatusNotFound)
		return
	}
	tenantID := r.URL.Query().Get("tenant_id")
	if tenantID == "" {
		tenantID = r.Header.Get(s.config.Auth.TenantHeader)
	}
	if tenantID == "" || !validID.MatchString(tenantID) {
		jsonError(w, "missing or invalid tenant_id", http.StatusBadRequest)
		return
	}
	apiKey := s.requestAPIKey(r)
	if apiKey == "" {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if _, kerr := validateAPIKey(r.Context(), s.rdb, s.db, tenantID, apiKey); kerr != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()

	switch r.Method {
	case http.MethodGet:
		agentID := r.URL.Query().Get("agent_id")
		if agentID != "" {
			respondJSON(w, getAgentSettings(ctx, s.rdb, tenantID, agentID))
			return
		}
		// No agent_id: return settings for all isolated agents (the ones with state).
		isolated := loadIsolatedAgents(ctx, s.rdb, tenantID)
		out := make([]AgentSettings, 0, len(isolated))
		for a := range isolated {
			out = append(out, getAgentSettings(ctx, s.rdb, tenantID, a))
		}
		respondJSON(w, map[string]interface{}{"settings": out})

	case http.MethodPut, http.MethodPost:
		var req struct {
			AgentID     string `json:"agent_id"`
			Isolated    bool   `json:"isolated"`
			DisplayName string `json:"display_name"`
			Note        string `json:"note"`
		}
		if !decodeJSONBody(w, r, &req, 16*1024) {
			return
		}
		req.AgentID = strings.TrimSpace(req.AgentID)
		if req.AgentID == "" || !validID.MatchString(req.AgentID) {
			jsonError(w, "invalid agent_id", http.StatusBadRequest)
			return
		}
		if len(req.DisplayName) > 80 || len(req.Note) > 500 {
			jsonError(w, "display_name (<=80) or note (<=500) too long", http.StatusBadRequest)
			return
		}
		st := AgentSettings{
			AgentID:     req.AgentID,
			Isolated:    req.Isolated,
			DisplayName: strings.TrimSpace(req.DisplayName),
			Note:        strings.TrimSpace(req.Note),
		}
		if err := setAgentSettings(ctx, s.rdb, tenantID, st); err != nil {
			jsonError(w, "storage error", http.StatusInternalServerError)
			return
		}
		respondJSON(w, getAgentSettings(ctx, s.rdb, tenantID, req.AgentID))

	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
