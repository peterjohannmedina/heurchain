package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// Readiness uses an authenticated read API: Qdrant's /readyz is accessible
// even with an invalid API key. Do not expose collection names or error bodies.
// This checks connectivity and read authorization, not tenant index coverage.
func (q *QdrantClient) Readiness(ctx context.Context) string {
	if q == nil {
		return "unconfigured"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.baseURL+"/collections", nil)
	if err != nil {
		return "unavailable"
	}
	q.setAuth(req)
	resp, err := q.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "timeout"
		}
		return "unreachable"
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		// A wrong upstream can return 200 HTML (for example a default proxy
		// page). Require Qdrant's response envelope and bound decoding work.
		var body struct {
			Result struct {
				Collections json.RawMessage `json:"collections"`
			} `json:"result"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil ||
			len(body.Result.Collections) == 0 || body.Result.Collections[0] != '[' {
			return "invalid_response"
		}
		return "ok"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "unauthorized"
	default:
		return "unavailable"
	}
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	type check struct{ name, status string }
	checks := make(chan check, 3)
	go func() {
		status := "ok"
		if s.rdb.Ping(ctx).Err() != nil {
			status = "unavailable"
		}
		checks <- check{"redis", status}
	}()
	go func() {
		status := "ok"
		if s.db.PingContext(ctx) != nil {
			status = "unavailable"
		}
		checks <- check{"sqlite", status}
	}()
	q := defaultQdrantClient
	go func() { checks <- check{"qdrant", q.Readiness(ctx)} }()
	result := map[string]interface{}{"status": "ready", "index_coverage": "not_checked"}
	code := http.StatusOK
	for i := 0; i < 3; i++ {
		c := <-checks
		result[c.name] = c.status
		if c.status != "ok" {
			result["status"] = "not_ready"
			code = http.StatusServiceUnavailable
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	respondJSON(w, result)
}
