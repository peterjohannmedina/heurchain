package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestBrokerArtifactSHA256(t *testing.T) {
	digest := brokerArtifactSHA256()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
		t.Fatalf("brokerArtifactSHA256() = %q, want lowercase SHA-256", digest)
	}
}

func TestBrokerInstanceIDPrefersEnvironment(t *testing.T) {
	t.Setenv("HEURCHAIN_INSTANCE_ID", "canary-p003")
	if got := brokerInstanceID(); got != "canary-p003" {
		t.Fatalf("brokerInstanceID() = %q, want canary-p003", got)
	}
}

func TestBrokerInstanceIDFallsBackWhenFileMissing(t *testing.T) {
	t.Setenv("HEURCHAIN_INSTANCE_ID", "")
	if _, err := os.Stat(brokerInstanceIDPath); err == nil {
		t.Skipf("host has %s; missing-file fallback is not testable here", brokerInstanceIDPath)
	}
	if got := brokerInstanceID(); got != "unconfigured" {
		t.Fatalf("brokerInstanceID() = %q, want unconfigured", got)
	}
}

func TestExecutableUsedForArtifactDigestExists(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Clean(executable)); err != nil {
		t.Fatal(err)
	}
}
