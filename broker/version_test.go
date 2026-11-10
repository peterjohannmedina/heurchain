package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// readSourceFile reads a file relative to the package directory. Used to assert
// invariants about the source itself where the runtime value is unreachable
// (handlers that need live Redis/SQLite to build their response).
func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// The version string is reported by three independent surfaces. They used to be
// three separate literals and they drifted: discovery.go said "0.3.0" while
// /health and /openapi.json said "0.2.0", so one deployment advertised two
// versions depending on which endpoint you asked. This test fails if any
// surface stops reading brokerVersion.
func TestVersionIsConsistentAcrossSurfaces(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`).MatchString(brokerVersion) {
		t.Fatalf("brokerVersion %q is not semver", brokerVersion)
	}

	// GET /openapi.json — info.version
	rec := httptest.NewRecorder()
	(&Server{}).handleOpenAPI(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	var spec map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("decode openapi: %v", err)
	}
	if got := spec["info"].(map[string]interface{})["version"]; got != brokerVersion {
		t.Errorf("openapi info.version = %v, want %s", got, brokerVersion)
	}

	// GET / (discovery, JSON form)
	if discoveryJSON.Version != brokerVersion {
		t.Errorf("discoveryJSON.Version = %s, want %s", discoveryJSON.Version, brokerVersion)
	}

	// /health and /admin build their maps inside handlers that need live Redis
	// and SQLite, so assert on the source instead: no handler may carry a
	// hardcoded version literal.
	//
	// Checked here rather than in CI because a Go test travels with the package
	// and runs on every contributor's machine.
	for _, f := range []string{"handlers.go", "openapi.go", "discovery.go", "mcp.go"} {
		src := readSourceFile(t, f)
		// Strip the one legitimate mention: version.go's own doc comment
		// references the historical values, but that file is excluded anyway.
		if m := regexp.MustCompile(`"version":\s*"\d+\.\d+\.\d+"`).FindString(src); m != "" {
			t.Errorf("%s contains a hardcoded version literal (%s) — use brokerVersion", f, m)
		}
		if m := regexp.MustCompile(`Version:\s*"\d+\.\d+\.\d+"`).FindString(src); m != "" {
			t.Errorf("%s contains a hardcoded version literal (%s) — use brokerVersion", f, m)
		}
	}

	if brokerRollbackPolicy != "compatible" && brokerRollbackPolicy != "forward-only" {
		t.Errorf("brokerRollbackPolicy = %q, want compatible or forward-only", brokerRollbackPolicy)
	}
}

// Compatible releases may only contain additive migrations. This deliberately
// errs on the conservative side: if a future migration drops, renames, or
// rewrites data, the author must mark the release forward-only and provide a
// migration-specific deployment and recovery plan.
func TestCompatibleReleaseHasNoDestructiveMigration(t *testing.T) {
	if brokerRollbackPolicy != "compatible" {
		return
	}
	destructive := regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|COLUMN|INDEX)|TRUNCATE\s+TABLE|ALTER\s+TABLE\s+\S+\s+RENAME|DELETE\s+FROM|UPDATE\s+\S+\s+SET)\b`)
	for _, migration := range migrations {
		for _, stmt := range migration.stmts {
			if destructive.MatchString(stmt) {
				t.Errorf("migration v%d is destructive but brokerRollbackPolicy is compatible: %q", migration.version, stmt)
			}
		}
	}
}

// CHANGELOG.md must document the current version. A release that ships without
// a changelog entry is how the file went 73 commits out of date.
func TestChangelogDocumentsCurrentVersion(t *testing.T) {
	src := readSourceFile(t, "../CHANGELOG.md")
	if !strings.Contains(src, "## ["+brokerVersion+"]") {
		t.Errorf("CHANGELOG.md has no '## [%s]' section — add one before releasing", brokerVersion)
	}
}
