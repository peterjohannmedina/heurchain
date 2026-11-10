package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// newExportTestServer builds the minimum Server /vault/export touches: Redis for
// the record hashes and a real SQLite DB, because the handler audits every
// export through s.db before streaming a byte.
func newExportTestServer(t *testing.T) *Server {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "broker.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("apply base schema: %v", err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	// The handlers read the auth header names straight off the config, which in
	// production is always populated (loadConfig defaults them). An empty config
	// file is a documented legitimate input — every field gets a default — so
	// the test server gets a real loadConfig result rather than a hand-built
	// literal or a nil config that would panic on the first header read.
	cfgPath := filepath.Join(t.TempDir(), "broker.yaml")
	if err := os.WriteFile(cfgPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load test config: %v", err)
	}

	return &Server{rdb: newTestRedis(t), db: db, config: cfg}
}

// exportVault runs the handler as an authenticated admin and returns the entries
// of the streamed archive keyed by name.
func exportVault(t *testing.T, s *Server, tenantID string) map[string]string {
	t.Helper()
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASS", "hunter2")

	req := httptest.NewRequest(http.MethodGet, "/vault/export?tenant_id="+tenantID, nil)
	req.SetBasicAuth("admin", "hunter2")
	rec := httptest.NewRecorder()
	s.handleVaultExport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("export status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	raw := rec.Body.Bytes()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("response is not a readable zip: %v", err)
	}
	out := make(map[string]string, len(zr.File))
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open zip entry %s: %v", f.Name, err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read zip entry %s: %v", f.Name, err)
		}
		out[f.Name] = string(b)
	}
	return out
}

// markdownBody returns the text between the frontmatter and the "## Metadata"
// section — the part a customer actually opens the archive for.
func markdownBody(t *testing.T, md string) string {
	t.Helper()
	_, rest, ok := strings.Cut(strings.TrimPrefix(md, "---\n"), "\n---\n")
	if !ok {
		t.Fatalf("exported file has no closing frontmatter delimiter:\n%s", md)
	}
	body, _, ok := strings.Cut(rest, "\n## Metadata\n")
	if !ok {
		t.Fatalf("exported file has no Metadata section:\n%s", md)
	}
	return strings.TrimSpace(body)
}

// The regression this file exists for. storeVec — the path behind /store, and so
// behind every natively-stored memory — writes the body to the top-level "text"
// hash field. The export read only metadata.text, so it shipped a correct
// frontmatter header above an empty body for the entire corpus: an archive that
// looked plausible, weighed almost nothing, and contained none of the memories.
func TestVaultExportIncludesTextFromCanonicalField(t *testing.T) {
	ctx := context.Background()
	s := newExportTestServer(t)
	const tenant = "tenant-export-1"
	const body = "ACT-R activation decays with age unless the doc is re-read"

	if err := storeVec(ctx, s.rdb, tenant, storedVector{
		ID:        "doc-native",
		AgentID:   "hermes",
		Embedding: []float32{1, 0},
		Metadata:  json.RawMessage(`{}`),
		Text:      body,
	}); err != nil {
		t.Fatal(err)
	}

	files := exportVault(t, s, tenant)
	md, ok := files["doc-native.md"]
	if !ok {
		t.Fatalf("doc-native.md missing from archive (entries: %v)", files)
	}
	if got := markdownBody(t, md); got != body {
		t.Errorf("exported body = %q, want %q", got, body)
	}
}

// Records written by handleVaultImport before this fix carry their body only
// under metadata.text. The export must still find it, and must not then print
// the body a second time inside the metadata block.
func TestVaultExportIncludesTextFromMetadataFallback(t *testing.T) {
	ctx := context.Background()
	s := newExportTestServer(t)
	const tenant = "tenant-export-2"
	const body = "imported records keep their body under metadata.text"

	meta, err := json.Marshal(map[string]interface{}{"text": body, "source": "vault-import"})
	if err != nil {
		t.Fatal(err)
	}
	// Written the way handleVaultImport wrote it: metadata.text, no "text" field.
	if err := s.rdb.HSet(ctx, vecKey(tenant, "doc-imported"), map[string]interface{}{
		"embedding":  "[1,0]",
		"metadata":   string(meta),
		"created_at": "2026-07-16T03:18:06Z",
		"agent_id":   "codex",
	}).Err(); err != nil {
		t.Fatal(err)
	}

	md := exportVault(t, s, tenant)["doc-imported.md"]
	if got := markdownBody(t, md); got != body {
		t.Errorf("exported body = %q, want %q", got, body)
	}
	metaBlock := md[strings.Index(md, "## Metadata"):]
	if strings.Contains(metaBlock, body) {
		t.Error("body text duplicated inside the metadata block; it should be lifted out")
	}
	if !strings.Contains(metaBlock, "vault-import") {
		t.Errorf("metadata block lost its other keys:\n%s", metaBlock)
	}
}

// A vaulted (cold) record exports exactly like a hot one — promotion moves the
// whole hash, so the body must survive the vec -> vault move.
func TestVaultExportIncludesTextForVaultedRecords(t *testing.T) {
	ctx := context.Background()
	s := newExportTestServer(t)
	const tenant = "tenant-export-3"
	const body = "cold storage must not swallow the body"

	if err := storeVec(ctx, s.rdb, tenant, storedVector{
		ID:        "doc-cold",
		AgentID:   "caclaw",
		Embedding: []float32{1, 0},
		Metadata:  json.RawMessage(`{}`),
		Text:      body,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := moveActiveToVault(ctx, s.rdb, tenant, "doc-cold"); err != nil {
		t.Fatal(err)
	}

	md := exportVault(t, s, tenant)["doc-cold.md"]
	if got := markdownBody(t, md); got != body {
		t.Errorf("exported body for vaulted record = %q, want %q", got, body)
	}
}

func TestVaultRecordText(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string]string
		meta map[string]interface{}
		want string
		// wantMetaText asserts on metaMap["text"] after the call. It is checked
		// in both directions: the body must not survive in the map when it was
		// found (it would be printed twice — once as the body, once inside the
		// metadata block), and it must survive untouched when it was not found.
		wantMetaText bool
	}{
		{
			// The canonical field is what the body is read from, so the stale
			// copy under metadata must be dropped.
			name: "canonical field wins",
			data: map[string]string{"text": "canonical"},
			meta: map[string]interface{}{"text": "stale", "k": "v"},
			want: "canonical",
		},
		{
			name: "metadata fallback",
			data: map[string]string{},
			meta: map[string]interface{}{"text": "from metadata"},
			want: "from metadata",
		},
		{
			name: "nil metadata is safe",
			data: map[string]string{"text": "canonical"},
			meta: nil,
			want: "canonical",
		},
		{
			name: "no text anywhere",
			data: map[string]string{},
			meta: map[string]interface{}{"k": "v"},
			want: "",
		},
		{
			// The value is not a string, so it was never the body; the map is
			// left exactly as the caller passed it.
			name:         "non-string metadata text is ignored",
			data:         map[string]string{},
			meta:         map[string]interface{}{"text": 42},
			want:         "",
			wantMetaText: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := vaultRecordText(tc.data, tc.meta)
			if got != tc.want {
				t.Errorf("vaultRecordText = %q, want %q", got, tc.want)
			}
			if tc.meta != nil {
				if _, still := tc.meta["text"]; still != tc.wantMetaText {
					t.Errorf(`metaMap["text"] present = %v, want %v`, still, tc.wantMetaText)
				}
			}
		})
	}
}
