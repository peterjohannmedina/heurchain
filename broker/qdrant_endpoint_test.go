package main

import (
	"strings"
	"testing"
)

func TestQdrantEndpointEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"default", "", "http://localhost:6333"},
		{"local", "http://127.0.0.1:6333", "http://127.0.0.1:6333"},
		{"remote", " https://vectors.example.test/ ", "https://vectors.example.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("QDRANT_URL", tc.input)
			t.Setenv("QDRANT_API_KEY", "test-secret")
			client, err := qdrantClientFromEnvironment()
			if err != nil {
				t.Fatal(err)
			}
			if client.baseURL != tc.want || client.apiKey != "test-secret" {
				t.Fatal("endpoint or independent API-key configuration was not applied")
			}
		})
	}
}

func TestQdrantEndpointRejectsInvalidOriginsWithoutLeakingCredentials(t *testing.T) {
	for _, input := range []string{
		"localhost:6333", "file:///tmp/qdrant", "http://", "https://user:test-secret@example.test",
		"https://example.test/collections", "https://example.test?api-key=test-secret", "https://example.test#test-secret",
	} {
		t.Setenv("QDRANT_URL", input)
		client, err := qdrantClientFromEnvironment()
		if client != nil || err == nil {
			t.Fatal("accepted an invalid Qdrant origin")
		}
		if strings.Contains(err.Error(), "test-secret") {
			t.Fatal("configuration error exposed a credential")
		}
	}
}
