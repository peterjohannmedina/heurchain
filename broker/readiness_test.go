package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadinessChecksAuthenticatedQdrantWithoutChangingHealth(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{
		{200, "ok"}, {401, "unauthorized"}, {403, "unauthorized"}, {503, "unavailable"},
	} {
		t.Run(tc.want+http.StatusText(tc.code), func(t *testing.T) {
			s := newExportTestServer(t)
			q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/collections" || r.Method != "GET" || r.Header.Get("api-key") != "fixture-secret" {
					t.Errorf("probe did not use authenticated read API")
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(`{"result":{"collections":[{"name":"private-tenant"}]},"secret":"fixture-secret"}`))
			}))
			defer q.Close()
			old := defaultQdrantClient
			defaultQdrantClient = NewQdrantClient(q.URL, 768)
			defaultQdrantClient.apiKey = "fixture-secret"
			defer func() { defaultQdrantClient = old }()
			w := httptest.NewRecorder()
			s.handleReady(w, httptest.NewRequest("GET", "/ready", nil))
			wantCode := 200
			if tc.code != 200 {
				wantCode = 503
			}
			var result map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if w.Code != wantCode || result["qdrant"] != tc.want || result["redis"] != "ok" || result["sqlite"] != "ok" || result["index_coverage"] != "not_checked" {
				t.Fatalf("unexpected readiness: %d %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "fixture-secret") || strings.Contains(w.Body.String(), "private-tenant") {
				t.Fatal("readiness response exposed or cached dependency data")
			}
			health := httptest.NewRecorder()
			s.handleHealth(health, httptest.NewRequest("GET", "/health", nil))
			if health.Code != 200 {
				t.Fatalf("Qdrant failure changed liveness: %s", health.Body.String())
			}
		})
	}
}

func TestReadinessDeadlineAndDependencyFailures(t *testing.T) {
	s := newExportTestServer(t)
	q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer q.Close()
	old := defaultQdrantClient
	defaultQdrantClient = NewQdrantClient(q.URL, 768)
	defer func() { defaultQdrantClient = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	start := time.Now()
	s.handleReady(w, httptest.NewRequest("GET", "/ready", nil).WithContext(ctx))
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"qdrant":"timeout"`) || time.Since(start) > time.Second {
		t.Fatalf("dependency probe ignored deadline: %d %s", w.Code, w.Body.String())
	}
	defaultQdrantClient = nil
	_ = s.rdb.Close()
	_ = s.db.Close()
	w = httptest.NewRecorder()
	s.handleReady(w, httptest.NewRequest("GET", "/ready", nil))
	var result map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 503 || result["qdrant"] != "unconfigured" || result["redis"] != "unavailable" || result["sqlite"] != "unavailable" {
		t.Fatalf("closed dependencies marked ready: %s", w.Body.String())
	}
	w = httptest.NewRecorder()
	s.handleReady(w, httptest.NewRequest("POST", "/ready", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "GET" {
		t.Fatal("unexpected method accepted")
	}
}

func TestQdrantReadinessRejectsWrongUpstream(t *testing.T) {
	for _, body := range []string{"<html>default proxy page</html>", `{}`, `{"result":{"collections":null}}`} {
		q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		client := NewQdrantClient(q.URL, 768)
		if got := client.Readiness(context.Background()); got != "invalid_response" {
			t.Errorf("wrong upstream accepted: %s", got)
		}
		q.Close()
		if got := client.Readiness(context.Background()); got != "unreachable" {
			t.Errorf("closed upstream accepted: %s", got)
		}
	}
}
