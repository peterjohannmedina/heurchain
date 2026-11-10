package main

import (
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	cacheKeyMaxLen   = 256
	cacheValueMaxLen = 64 * 1024 // 64 KB
	cacheTTLMax      = 30 * 24 * time.Hour
	cacheTTLDefault  = 24 * time.Hour
)

// validCacheKey allows alphanumeric plus ._:- so keys are safe as Redis key
// components and unambiguous in logs. Reject everything else upfront.
var validCacheKey = regexp.MustCompile(`^[A-Za-z0-9._:\-]{1,256}$`)

func tenantCacheKey(tenantID, key string) string {
	return "tenant:" + tenantID + ":cache:" + key
}

// ---------------------------------------------------------------------------
// POST /cache   body: {key, value, ttl_seconds}
// GET  /cache?key=...
// DELETE /cache?key=...
// ---------------------------------------------------------------------------

func (s *Server) handleCache(w http.ResponseWriter, r *http.Request) {
	if handleCORSPreflight(w, r) {
		return
	}
	setCORSHeaders(w)

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

	switch r.Method {
	case http.MethodPost:
		s.handleCacheSet(w, r, tenantID)
	case http.MethodGet:
		s.handleCacheGet(w, r, tenantID)
	case http.MethodDelete:
		s.handleCacheDelete(w, r, tenantID)
	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleCacheSet(w http.ResponseWriter, r *http.Request, tenantID string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var req struct {
		Key        string `json:"key"`
		Value      string `json:"value"`
		TTLSeconds int    `json:"ttl_seconds"`
	}
	if !decodeJSONBody(w, r, &req, 0) { return }
	if !validCacheKey.MatchString(req.Key) {
		jsonError(w, "key must be 1–256 chars: [A-Za-z0-9._:-]", http.StatusBadRequest)
		return
	}
	if len(req.Value) > cacheValueMaxLen {
		jsonError(w, "value exceeds 64 KB limit", http.StatusBadRequest)
		return
	}

	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = cacheTTLDefault
	}
	if ttl > cacheTTLMax {
		ttl = cacheTTLMax
	}

	ctx := r.Context()
	if err := s.rdb.Set(ctx, tenantCacheKey(tenantID, req.Key), req.Value, ttl).Err(); err != nil {
		log.Printf("cache set error tenant=%s key=%s: %v", tenantID, req.Key, err)
		jsonError(w, "cache write failed", http.StatusInternalServerError)
		return
	}
	respondJSON(w, map[string]interface{}{"ok": true, "ttl_seconds": int(ttl.Seconds())})
}

func (s *Server) handleCacheGet(w http.ResponseWriter, r *http.Request, tenantID string) {
	key := r.URL.Query().Get("key")
	if !validCacheKey.MatchString(key) {
		jsonError(w, "key must be 1–256 chars: [A-Za-z0-9._:-]", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	val, err := s.rdb.Get(ctx, tenantCacheKey(tenantID, key)).Result()
	if err == redis.Nil {
		respondJSON(w, map[string]interface{}{"found": false, "value": nil})
		return
	}
	if err != nil {
		log.Printf("cache get error tenant=%s key=%s: %v", tenantID, key, err)
		jsonError(w, "cache read failed", http.StatusInternalServerError)
		return
	}
	respondJSON(w, map[string]interface{}{"found": true, "value": val})
}

func (s *Server) handleCacheDelete(w http.ResponseWriter, r *http.Request, tenantID string) {
	key := r.URL.Query().Get("key")
	if !validCacheKey.MatchString(key) {
		jsonError(w, "key must be 1–256 chars: [A-Za-z0-9._:-]", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	n, err := s.rdb.Del(ctx, tenantCacheKey(tenantID, key)).Result()
	if err != nil {
		log.Printf("cache delete error tenant=%s key=%s: %v", tenantID, key, err)
		jsonError(w, "cache delete failed", http.StatusInternalServerError)
		return
	}
	respondJSON(w, map[string]interface{}{"deleted": n > 0})
}
