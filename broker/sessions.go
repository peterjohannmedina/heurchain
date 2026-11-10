package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------
// Key helpers
// ---------------------------------------------------------------------------

func sessMetaKey(tenantID, sessionID string) string {
	return fmt.Sprintf("tenant:%s:sess:%s:meta", tenantID, sessionID)
}

func sessCtxKey(tenantID, sessionID string) string {
	return fmt.Sprintf("tenant:%s:sess:%s:ctx", tenantID, sessionID)
}

func sessMemoryCtxKey(tenantID, sessionID string) string {
	return fmt.Sprintf("tenant:%s:sess:%s:memory-ctx", tenantID, sessionID)
}

func agentSessSetKey(tenantID, agentID string) string {
	return fmt.Sprintf("tenant:%s:agent:%s:sessions", tenantID, agentID)
}

// ---------------------------------------------------------------------------
// Session lifecycle
// ---------------------------------------------------------------------------

// sessionBinding carries the optional silo/engagement pinning plus the
// creating key id (sessions are only usable by the key that created them).
type sessionBinding struct {
	KeyID        int64
	Silo         string // silo NAME
	EngagementID string
}

func startSession(ctx context.Context, rdb *redis.Client, tenantID, agentID string, ttlSeconds int, bind *sessionBinding) (string, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = 3600
	}

	sessionID := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)

	metaKey := sessMetaKey(tenantID, sessionID)
	agentSetKey := agentSessSetKey(tenantID, agentID)
	ttl := time.Duration(ttlSeconds) * time.Second

	pipe := rdb.Pipeline()
	pipe.HSet(ctx, metaKey, "agent_id", agentID, "created_at", now, "last_seen", now, "ttl_s", ttlSeconds)
	if bind != nil {
		pipe.HSet(ctx, metaKey, "key_id", fmt.Sprintf("%d", bind.KeyID))
		if bind.Silo != "" {
			pipe.HSet(ctx, metaKey, "silo", bind.Silo)
		}
		if bind.EngagementID != "" {
			pipe.HSet(ctx, metaKey, "engagement_id", bind.EngagementID)
			// Reverse index so pausing/closing the engagement can kill its
			// live sessions (broker-enforced lifecycle).
			engSet := engagementSessSetKey(tenantID, bind.EngagementID)
			pipe.SAdd(ctx, engSet, sessionID)
			pipe.Expire(ctx, engSet, 24*time.Hour)
		}
	}
	pipe.SAdd(ctx, agentSetKey, sessionID)
	// Enforce session TTL so abandoned sessions expire instead of leaking Redis memory.
	// The agent set is given a generous floor so it outlives its member sessions.
	pipe.Expire(ctx, metaKey, ttl)
	if agentSetTTL := ttl; agentSetTTL < 24*time.Hour {
		agentSetTTL = 24 * time.Hour
		pipe.Expire(ctx, agentSetKey, agentSetTTL)
	} else {
		pipe.Expire(ctx, agentSetKey, agentSetTTL)
	}
	_, err := pipe.Exec(ctx)
	if err != nil {
		return "", err
	}
	return sessionID, nil
}

func endSession(ctx context.Context, rdb *redis.Client, tenantID, sessionID string) error {
	metaKey := sessMetaKey(tenantID, sessionID)

	agentID, err := rdb.HGet(ctx, metaKey, "agent_id").Result()
	if err == redis.Nil {
		return nil
	}
	if err != nil {
		return err
	}

	agentSetKey := agentSessSetKey(tenantID, agentID)
	ctxKey := sessCtxKey(tenantID, sessionID)

	pipe := rdb.Pipeline()
	pipe.SRem(ctx, agentSetKey, sessionID)
	pipe.Del(ctx, metaKey, ctxKey, sessMemoryCtxKey(tenantID, sessionID))
	_, err = pipe.Exec(ctx)
	return err
}

// ---------------------------------------------------------------------------
// Context list
// ---------------------------------------------------------------------------

// appendMemoryContext links a stored memory to an existing session. Context
// reads resolve canonical records so corrections and forgetting apply on resume.
func appendMemoryContext(ctx context.Context, rdb *redis.Client, tenantID, sessionID, id string) error {
	// References retain no extra body copy: correction and forget must also
	// apply when a caller resumes a session. The existence check and sliding
	// expiry are atomic so an expired session cannot be recreated by a store.
	const script = `
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
local ttl = tonumber(redis.call('HGET', KEYS[1], 'ttl_s') or '3600')
if not ttl or ttl <= 0 then ttl = 3600 end
redis.call('LREM', KEYS[2], 0, ARGV[1])
redis.call('LPUSH', KEYS[2], ARGV[1])
redis.call('LTRIM', KEYS[2], 0, 49)
redis.call('HSET', KEYS[1], 'last_seen', ARGV[2])
redis.call('EXPIRE', KEYS[1], ttl)
redis.call('EXPIRE', KEYS[2], ttl)
return 1`
	return redis.NewScript(script).Run(ctx, rdb,
		[]string{sessMetaKey(tenantID, sessionID), sessMemoryCtxKey(tenantID, sessionID)},
		id, time.Now().UTC().Format(time.RFC3339)).Err()
}

func getContext(ctx context.Context, rdb *redis.Client, tenantID, sessionID string, n int) ([]string, error) {
	if n <= 0 {
		return []string{}, nil
	}
	const script = `
if redis.call('EXISTS', KEYS[1]) == 0 then return {} end
return redis.call('LRANGE', KEYS[2], 0, 49)`
	ids, err := redis.NewScript(script).Run(ctx, rdb,
		[]string{sessMetaKey(tenantID, sessionID), sessMemoryCtxKey(tenantID, sessionID)}).StringSlice()
	if err != nil {
		return nil, err
	}
	items := make([]string, 0, n)
	for _, id := range ids {
		fields, status, err := readMemoryFields(ctx, rdb, tenantID, id)
		if errors.Is(err, errMemoryForgotten) || errors.Is(err, errMemoryNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if text := memoryRecordFromFields(id, status, fields).Text; text != "" {
			items = append(items, text)
		}
		if len(items) == n {
			break
		}
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// Agent sessions
// ---------------------------------------------------------------------------

func agentSessions(ctx context.Context, rdb *redis.Client, tenantID, agentID string) ([]string, error) {
	agentSetKey := agentSessSetKey(tenantID, agentID)
	return rdb.SMembers(ctx, agentSetKey).Result()
}

type agentMemoryItem struct {
	ID        string                 `json:"id"`
	Text      string                 `json:"text"`
	Tags      []string               `json:"tags,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt string                 `json:"created_at"`
}

func recentAgentMemories(ctx context.Context, rdb *redis.Client, tenantID, agentID string, limit int) ([]agentMemoryItem, error) {
	ids, err := rdb.SMembers(ctx, vecSetKey(tenantID)).Result()
	if err != nil {
		return nil, fmt.Errorf("list vectors: %w", err)
	}
	if len(ids) == 0 {
		return []agentMemoryItem{}, nil
	}

	pipe := rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	forgotten := make([]*redis.IntCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, vecKey(tenantID, id))
		forgotten[i] = pipe.Exists(ctx, memoryTombstoneKey(tenantID, id))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, fmt.Errorf("fetch vectors: %w", err)
	}

	items := make([]agentMemoryItem, 0, limit)
	for i, id := range ids {
		if tombstoneHidden(forgotten[i]) {
			continue // lifecycle tombstone
		}
		data, err := cmds[i].Result()
		if err != nil || len(data) == 0 || data["agent_id"] != agentID {
			continue
		}
		item := agentMemoryItem{
			ID:        id,
			Text:      data["text"],
			Tags:      parseTags(data["tags"]),
			CreatedAt: data["created_at"],
		}
		if raw := data["metadata"]; raw != "" {
			_ = json.Unmarshal([]byte(raw), &item.Metadata)
		}
		items = append(items, item)
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt > items[j].CreatedAt
	})
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func parseAgentPath(path string) (agentID string, recall bool, ok bool) {
	trimmed := strings.TrimPrefix(path, "/agent/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 1 && validID.MatchString(parts[0]) {
		return parts[0], false, true
	}
	if len(parts) == 2 && validID.MatchString(parts[0]) && parts[1] == "recall" {
		return parts[0], true, true
	}
	return "", false, false
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
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
	keyID, err := validateAPIKey(ctx, s.rdb, s.db, tenantID, apiKey)
	if err != nil {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		AgentID      string `json:"agent_id"`
		TTLSeconds   int    `json:"ttl_seconds"`
		EngagementID string `json:"engagement_id"` // bind session to an engagement (billable path)
		Silo         string `json:"silo"`          // or bind to a raw silo (non-billable)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid json body", http.StatusBadRequest)
		return
	}

	bind := &sessionBinding{KeyID: keyID}
	if req.EngagementID != "" || req.Silo != "" {
		if s.crypto == nil {
			jsonError(w, "silo features not configured on this deployment", http.StatusServiceUnavailable)
			return
		}
		siloName := req.Silo
		if req.EngagementID != "" {
			// Resolve engagement -> silo, then enforce all four gates at bind
			// time so a consultant without a grant/membership is rejected at
			// session start (and again on every /store).
			var engSiloID, engSiloName string
			err := s.db.QueryRowContext(ctx,
				`SELECT e.silo_id, s.name FROM engagements e JOIN silos s ON s.id = e.silo_id
				  WHERE e.id = ? AND e.tenant_id = ?`, req.EngagementID, tenantID).
				Scan(&engSiloID, &engSiloName)
			if err != nil {
				jsonError(w, "engagement not found", http.StatusNotFound)
				return
			}
			siloName = engSiloName
		}
		if siloName == "" || !validID.MatchString(siloName) {
			jsonError(w, "invalid silo", http.StatusBadRequest)
			return
		}
		if _, err := s.enforceGates(ctx, tenantID, keyID, siloName, req.EngagementID); err != nil {
			if ee, ok := asEnforceError(err); ok {
				jsonError(w, "forbidden: "+ee.Code, http.StatusForbidden)
				return
			}
			log.Printf("session bind error tenant=%s: %v", tenantID, err)
			jsonError(w, "session start failed", http.StatusInternalServerError)
			return
		}
		bind.Silo = siloName
		bind.EngagementID = req.EngagementID
	}

	sessionID, err := startSession(ctx, s.rdb, tenantID, req.AgentID, req.TTLSeconds, bind)
	if err != nil {
		log.Printf("session start error tenant=%s: %v", tenantID, err)
		jsonError(w, "session start failed", http.StatusInternalServerError)
		return
	}

	resp := map[string]interface{}{"session_id": sessionID}
	if bind.Silo != "" {
		resp["silo"] = bind.Silo
	}
	if bind.EngagementID != "" {
		resp["engagement_id"] = bind.EngagementID
	}
	respondJSON(w, resp)
}

func (s *Server) handleSessionEnd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
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

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid json body", http.StatusBadRequest)
		return
	}

	if err := endSession(ctx, s.rdb, tenantID, req.SessionID); err != nil {
		log.Printf("session end error tenant=%s: %v", tenantID, err)
		jsonError(w, "session end failed", http.StatusInternalServerError)
		return
	}

	respondJSON(w, map[string]interface{}{
		"ok": true,
	})
}

func (s *Server) handleSessionCtx(w http.ResponseWriter, r *http.Request) {
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

	path := strings.TrimPrefix(r.URL.Path, "/session/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[1] != "context" {
		jsonError(w, "invalid path", http.StatusBadRequest)
		return
	}
	sessionID := parts[0]

	n := 20
	if nStr := r.URL.Query().Get("n"); nStr != "" {
		if parsed, err := strconv.Atoi(nStr); err == nil {
			n = parsed
		}
	}
	if n > 50 {
		n = 50
	}
	if n <= 0 {
		n = 20
	}

	items, err := getContext(ctx, s.rdb, tenantID, sessionID, n)
	if err != nil {
		log.Printf("session context error tenant=%s: %v", tenantID, err)
		jsonError(w, "get context failed", http.StatusInternalServerError)
		return
	}

	respondJSON(w, map[string]interface{}{
		"items": items,
	})
}

func (s *Server) handleAgentRecall(w http.ResponseWriter, r *http.Request) {
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

	agentName, recall, ok := parseAgentPath(r.URL.Path)
	if !ok {
		jsonError(w, "invalid path", http.StatusBadRequest)
		return
	}

	if !recall {
		limit := 10
		if raw := r.URL.Query().Get("limit"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
				limit = parsed
			}
		}
		if limit > 50 {
			limit = 50
		}
		items, err := recentAgentMemories(ctx, s.rdb, tenantID, agentName, limit)
		if err != nil {
			log.Printf("agent memories error tenant=%s agent=%s: %v", tenantID, agentName, err)
			jsonError(w, "agent memories failed", http.StatusInternalServerError)
			return
		}
		respondJSON(w, map[string]interface{}{
			"agent_id": agentName,
			"count":    len(items),
			"items":    items,
		})
		return
	}

	sessionIDs, err := agentSessions(ctx, s.rdb, tenantID, agentName)
	if err != nil {
		log.Printf("agent recall error tenant=%s: %v", tenantID, err)
		jsonError(w, "agent recall failed", http.StatusInternalServerError)
		return
	}

	respondJSON(w, map[string]interface{}{
		"session_ids": sessionIDs,
	})
}
