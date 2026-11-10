package main

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// entryAsZipFile builds an in-memory zip holding one entry and returns that entry
// as a *zip.File, which is what parseVaultMarkdown consumes.
func entryAsZipFile(t *testing.T, name string, content []byte) *zip.File {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 1 {
		t.Fatalf("expected 1 zip entry, got %d", len(zr.File))
	}
	return zr.File[0]
}

// TestParseVaultMarkdownRoundTrip guards the ordinary path: a record written by
// buildMarkdown parses back to the same fields, so the size cap added alongside
// it did not disturb normal imports.
func TestParseVaultMarkdownRoundTrip(t *testing.T) {
	md := buildMarkdown("doc-1", "t1", "agent-1", "2026-01-02T03:04:05Z",
		[]string{"alpha", "beta"}, "hello body", []byte(`{"k":"v"}`))

	rec, err := parseVaultMarkdown(entryAsZipFile(t, "doc-1.md", []byte(md)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rec.ID != "doc-1" || rec.Tenant != "t1" || rec.AgentID != "agent-1" {
		t.Errorf("frontmatter mismatch: %+v", rec)
	}
	if !strings.Contains(rec.Text, "hello body") {
		t.Errorf("Text = %q, want it to contain the body", rec.Text)
	}
	if len(rec.Tags) != 2 {
		t.Errorf("Tags = %v, want 2 entries", rec.Tags)
	}
}

func TestLegacyUnixCreatedAtSurvivesVaultRoundTrip(t *testing.T) {
	md := buildMarkdown("legacy-doc", "t1", "codex", "1704067200", nil, "legacy memory", []byte(`{}`))
	record, err := parseVaultMarkdown(entryAsZipFile(t, "legacy-doc.md", []byte(md)))
	if err != nil {
		t.Fatal(err)
	}
	fellBack := false
	got := normalizeImportedCreatedAt(record.CreatedAt, func(_, _ string) { fellBack = true })
	if got != "2024-01-01T00:00:00Z" || fellBack {
		t.Fatalf("legacy created_at became %q, fallback=%v", got, fellBack)
	}
}

// TestParseVaultMarkdownRejectsOversizeEntry covers the declared-size path. The
// entry holds maxVaultEntryBytes+1 zero bytes, which deflates to a few tens of
// KiB — exactly the shape of a bomb: a small archive that expands far past what
// the reader should be willing to buffer. The archive-level cap does not catch
// this (the upload is tiny), so the per-entry cap must.
//
// The declared size is what rejects it here. The read-side bound is not reachable
// with an honest header and is not claimed to be covered — see the understated
// case below for why it cannot be reached with a dishonest one either.
func TestParseVaultMarkdownRejectsOversizeEntry(t *testing.T) {
	big := make([]byte, maxVaultEntryBytes+1)
	// Frontmatter prefix, so that the size cap is what rejects this and not the
	// format check — otherwise the test would pass for the wrong reason.
	copy(big, "---\nid: doc-big\n---\n")

	f := entryAsZipFile(t, "doc-big.md", big)
	if f.UncompressedSize64 <= maxVaultEntryBytes {
		t.Fatalf("entry compressed down to %d bytes uncompressed; "+
			"the cap is not actually exercised", f.UncompressedSize64)
	}
	// Sanity: the compressed archive really is small, i.e. this is a bomb and not
	// just a large legitimate upload that the 1 GiB cap would have caught anyway.
	if f.CompressedSize64 > 1<<20 {
		t.Fatalf("compressed entry is %d bytes; not a small-archive bomb",
			f.CompressedSize64)
	}

	_, err := parseVaultMarkdown(f)
	if err == nil {
		t.Fatal("expected an oversize entry to be rejected, got a nil error")
	}
	if !strings.Contains(err.Error(), "byte limit") {
		t.Fatalf("error = %v, want it to mention the byte limit", err)
	}
}

// TestParseVaultMarkdownRejectsUnderstatedHeader covers the case an honest
// declared-size check cannot: a central directory that *understates* the entry.
//
// The payload really is maxVaultEntryBytes+4096 bytes, but the header claims
// 1024, so the declared-size check passes and something else has to stop it. What
// stops it is archive/zip itself — checksumReader.Read returns ErrFormat once it
// has produced more bytes than the entry declares (archive/zip/reader.go) — so the
// read ends at the declared boundary, errors, and the parser rejects the record
// without ever buffering the payload.
//
// So this asserts the property that actually holds — an understated header cannot
// make the parser buffer an oversize entry, and the rejected entry costs a bounded
// read — rather than naming the branch that rejects it. Naming the branch would be
// wrong twice over: the read-side bound is not reachable through zip.File.Open()
// while archive/zip enforces the declared size, and a test asserting "the read-side
// cap rejects it" would fail today while passing with the cap's implementation
// removed. Should archive/zip ever stop enforcing the declared size, the read-side
// bound becomes the gate and both assertions below still hold.
func TestParseVaultMarkdownRejectsUnderstatedHeader(t *testing.T) {
	big := make([]byte, maxVaultEntryBytes+4096)
	copy(big, "---\nid: doc-understated\n---\n")

	f := entryAsZipFile(t, "doc-understated.md", big)
	if f.UncompressedSize64 <= maxVaultEntryBytes {
		t.Fatalf("fixture is only %d bytes uncompressed; it is not an oversize entry",
			f.UncompressedSize64)
	}

	// Understate the declared size, as a crafted central directory would. The
	// payload stays oversized, so the entry is a bomb whose header claims it is not.
	f.UncompressedSize64 = 1024
	if f.UncompressedSize64 > maxVaultEntryBytes {
		t.Fatal("declared size is still over the cap; the declared-size check " +
			"would be what rejects this, not the case under test")
	}

	// The decompressor must not hand out more than the bound. This is the property
	// the cap exists for, and it holds whichever guard is doing the rejecting.
	rc, err := f.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, _ := io.Copy(io.Discard, io.LimitReader(rc, maxVaultEntryBytes+1))
	rc.Close()
	if got > maxVaultEntryBytes+1 {
		t.Fatalf("read %d bytes past the %d byte bound", got, maxVaultEntryBytes)
	}

	rec, err := parseVaultMarkdown(f)
	if err == nil {
		t.Fatalf("expected an understated-header bomb to be rejected, got record %+v", rec)
	}
}

// TestIsBodyTooLarge pins the 413 classification. A body that trips
// http.MaxBytesReader must be reported as too large so the handler answers 413
// instead of the generic 400, which would give a client no signal to shrink the
// archive. A plain read failure must stay a 400.
//
// This covers one half of the chain — that a real over-limit read produces the
// error. TestHandleVaultImportAnswers413ForOversizeUpload covers the other, that
// the handler turns it into a 413 response.
func TestIsBodyTooLarge(t *testing.T) {
	const limit = 16
	r := &http.Request{
		Body: io.NopCloser(strings.NewReader(strings.Repeat("x", limit*4))),
	}
	r.Body = http.MaxBytesReader(nil, r.Body, limit)

	_, err := io.ReadAll(r.Body)
	if err == nil {
		t.Fatal(`expected MaxBytesReader to stop the read, got nil error`)
	}
	if !isBodyTooLarge(err) {
		t.Errorf("isBodyTooLarge(%v) = false, want true — maxVaultImportBytes "+
			"overruns would answer 400 instead of 413", err)
	}

	if isBodyTooLarge(io.ErrUnexpectedEOF) {
		t.Error("isBodyTooLarge(io.ErrUnexpectedEOF) = true, want false")
	}
	if isBodyTooLarge(nil) {
		t.Error("isBodyTooLarge(nil) = true, want false")
	}
}

// errBody yields a fixed error instead of content, so a handler can be driven
// down its read-failure path without materialising a body.
type errBody struct{ err error }

func (e errBody) Read([]byte) (int, error) { return 0, e.err }
func (e errBody) Close() error             { return nil }

// realMaxBytesError returns the error net/http produces for an over-limit read,
// so the handler test asserts on the value the cap actually emits rather than one
// hand-constructed here.
func realMaxBytesError(t *testing.T) error {
	t.Helper()
	rc := http.MaxBytesReader(nil, io.NopCloser(strings.NewReader(strings.Repeat("x", 64))), 16)
	_, err := io.Copy(io.Discard, rc)
	if err == nil {
		t.Fatal("expected MaxBytesReader to reject an over-limit read")
	}
	return err
}

// TestHandleVaultImportAnswers413ForOversizeUpload drives the real handler and
// asserts the HTTP status a client sees. The previous coverage called
// isBodyTooLarge directly, so reverting the handler's 413 branch to the generic
// 400 left it green: the classification was pinned, its use was not.
//
// The upload cap is maxVaultImportBytes (1 GiB), so the test does not materialise
// an over-limit archive — it supplies the read error the cap produces and drives
// the handler from there. That is the only part of the chain a test can
// substitute, and the error comes from a real MaxBytesReader (see above), so with
// TestIsBodyTooLarge both halves are covered: a real over-limit read yields this
// error, and the handler answers 413 for it. The second case pins the other
// direction — a plain read failure must stay a 400, not become a 413.
func TestHandleVaultImportAnswers413ForOversizeUpload(t *testing.T) {
	s := newExportTestServer(t)
	t.Setenv("ADMIN_USER", "admin")
	t.Setenv("ADMIN_PASS", "hunter2")

	for _, tc := range []struct {
		name string
		body io.Reader
		want int
	}{
		{"over the upload cap", errBody{realMaxBytesError(t)}, http.StatusRequestEntityTooLarge},
		{"unreadable body", errBody{io.ErrUnexpectedEOF}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/vault/import?tenant_id=t1", tc.body)
			req.SetBasicAuth("admin", "hunter2")
			rec := httptest.NewRecorder()
			s.handleVaultImport(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
