package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestStoreDerivesCollectionDimensionFromEmbedding(t *testing.T) {
	f := newRecallFixture(t)
	created := false
	q := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			http.NotFound(w, r)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points"):
			var body struct {
				Points []QdrantPoint `json:"points"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if !created || len(body.Points) != 1 || len(body.Points[0].Vector) != 1536 {
				t.Error("point was not projected into a matching collection")
			}
			respondJSON(w, map[string]interface{}{})
		case r.Method == http.MethodPut:
			var body struct {
				Vectors struct {
					Size int `json:"size"`
				} `json:"vectors"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Vectors.Size != 1536 {
				t.Errorf("collection dimension = %d", body.Vectors.Size)
			}
			created = true
			respondJSON(w, map[string]interface{}{})
		default:
			t.Errorf("unexpected request %s", r.Method)
		}
	}))
	defer q.Close()
	old := defaultQdrantClient
	defaultQdrantClient = NewQdrantClient(q.URL, 768)
	defer func() { defaultQdrantClient = old }()
	vector := make([]float32, 1536)
	vector[0] = 1
	w := f.call(t, "/store", map[string]interface{}{"id": "dimension-test", "text": "original source", "embedding": vector})
	if w.Code != 200 {
		t.Fatalf("store = %d: %s", w.Code, w.Body.String())
	}
	if !created || defaultQdrantClient.vectorSize != 768 {
		t.Fatal("dimension not applied per collection")
	}
	if err := defaultQdrantClient.EnsureCollection(context.Background(), "invalid", 0); err == nil {
		t.Fatal("accepted an empty vector dimension")
	}
}

func TestBM25BatchesPreserveFullCorpusScores(t *testing.T) {
	s := newExportTestServer(t)
	ctx := context.Background()
	ids := make([]string, 300)
	for i := range ids {
		ids[i] = fmt.Sprintf("doc-%03d", i)
		if err := updateBM25Index(ctx, s.rdb, "t1", ids[i], strings.Repeat("needle ", i%5+1)+"context"); err != nil {
			t.Fatal(err)
		}
	}
	want, err := scoreBM25Batch(ctx, s.rdb, "t1", []string{"needle"}, ids)
	if err != nil {
		t.Fatal(err)
	}
	got, err := scoreBM25(ctx, s.rdb, "t1", []string{"needle", "needle"}, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 300 || !reflect.DeepEqual(got, want) {
		t.Fatal("batching changed scores or omitted later documents")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := scoreBM25(cancelled, s.rdb, "t1", []string{"needle"}, ids); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestSearchReportsCorrelatedRetrievalDiagnostics(t *testing.T) {
	f := newRecallFixture(t)
	f.store(t, "source-doc", "cobalt docking credential", "reader", []string{"book"}, 1)
	w := f.call(t, "/search", map[string]interface{}{"text": "cobalt", "limit": 1})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var body struct {
		Retrieval recallDiagnostics `json:"retrieval"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	d := body.Retrieval
	if d.QueryID == "" || w.Header().Get("X-HeurChain-Query-ID") != d.QueryID {
		t.Fatal("missing correlated query identity")
	}
	if d.BM25Candidates != 1 || d.FusedCandidates != 1 || d.ReturnedDocuments != 1 {
		t.Fatalf("bad counts: %+v", d)
	}
	if d.EmbeddingMilliseconds < 0 || d.RetrievalMilliseconds < 0 || d.TotalMilliseconds < d.EmbeddingMilliseconds+d.RetrievalMilliseconds {
		t.Fatalf("invalid stage timing: %+v", d)
	}
}
