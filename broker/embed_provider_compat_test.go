package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// openAIEmbedConfig exercises the 0.6.1 fix: Ollama turned off with an empty
// URL, embeddings from a local OpenAI-compatible endpoint, and no explicit
// provider.
func openAIEmbedConfig(openAIURL string) string {
	return "embed:\n" +
		"  ollama_url: \"\"\n" +
		"  openai_url: \"" + openAIURL + "\"\n" +
		"  openai_key: \"tei-no-auth\"\n" +
		"  openai_model: \"BAAI/bge-base-en-v1.5\"\n"
}

func withLegacyEmbedAliases(t *testing.T, aliases []string) {
	t.Helper()
	previous := legacyEmbedModelAliases
	setLegacyEmbedModelAliases(aliases)
	t.Cleanup(func() { legacyEmbedModelAliases = previous })
}

func TestExplicitEmptyOllamaURLSelectsOpenAICompatibleProvider(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, openAIEmbedConfig("http://localhost:8090/v1/embeddings")))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Embed.Provider != "openai" {
		t.Fatalf("provider = %q, want openai", cfg.Embed.Provider)
	}
	if !slices.Equal(cfg.Embed.LegacyModelAliases, []string{"nomic-embed-text"}) {
		t.Fatalf("legacy aliases = %v, want [nomic-embed-text] (the tag 0.4.x wrote)", cfg.Embed.LegacyModelAliases)
	}
	if got := (&Server{config: cfg}).activeEmbedModel(); got != "openai:BAAI/bge-base-en-v1.5" {
		t.Fatalf("active model = %q", got)
	}
}

func TestOmittedOllamaURLKeepsOllamaDefault(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, "embed:\n  openai_url: \"http://localhost:8090/v1/embeddings\"\n"))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Embed.Provider != "" || len(cfg.Embed.LegacyModelAliases) != 0 {
		t.Fatalf("omitted ollama_url changed selection: provider=%q aliases=%v", cfg.Embed.Provider, cfg.Embed.LegacyModelAliases)
	}
	if got := embedProvider((&Server{config: cfg}).getEmbedConfig()); got != "ollama" {
		t.Fatalf("provider = %q, want ollama (unchanged default)", got)
	}
}

func TestEmptyOllamaURLWithExplicitProviders(t *testing.T) {
	if _, err := loadConfig(writeCfg(t, "embed:\n  provider: ollama\n  ollama_url: \"\"\n")); err == nil {
		t.Fatal("provider ollama with an empty ollama_url loaded; want a startup error")
	}
	cfg, err := loadConfig(writeCfg(t, "embed:\n  provider: openai\n  ollama_url: \"\"\n"))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Embed.Provider != "openai" || len(cfg.Embed.LegacyModelAliases) != 0 {
		t.Fatalf("explicit provider: provider=%q aliases=%v; aliases are only inferred when the provider was", cfg.Embed.Provider, cfg.Embed.LegacyModelAliases)
	}
}

func TestInferredAliasFollowsConfiguredOllamaModel(t *testing.T) {
	cfg, err := loadConfig(writeCfg(t, "embed:\n  ollama_url: \"\"\n  ollama_model: \"mxbai-embed-large\"\n"))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !slices.Equal(cfg.Embed.LegacyModelAliases, []string{"mxbai-embed-large"}) {
		t.Fatalf("aliases = %v", cfg.Embed.LegacyModelAliases)
	}
	cfg, err = loadConfig(writeCfg(t, "embed:\n  ollama_url: \"\"\n  legacy_model_aliases: [\"custom-tag\"]\n"))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !slices.Equal(cfg.Embed.LegacyModelAliases, []string{"custom-tag"}) {
		t.Fatalf("explicit aliases were replaced: %v", cfg.Embed.LegacyModelAliases)
	}
}

// The 0.6.0 regression: the absent default Ollama was chosen and every store,
// query and search failed. With an explicit empty ollama_url, embeddings must
// reach the configured OpenAI-compatible endpoint.
func TestEmptyOllamaURLConfigEmbedsThroughOpenAICompatibleEndpoint(t *testing.T) {
	tei := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("TEI path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.5,0.25]}]}`))
	}))
	defer tei.Close()

	cfg, err := loadConfig(writeCfg(t, openAIEmbedConfig(tei.URL+"/v1/embeddings")))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	embedding, model, err := (&Server{config: cfg}).embedTextWithModel(context.Background(), "hello")
	if err != nil {
		t.Fatalf("embedTextWithModel: %v (0.6.0 failed here against the absent Ollama)", err)
	}
	if model != "openai:BAAI/bge-base-en-v1.5" || len(embedding) != 2 || embedding[0] != 0.5 {
		t.Fatalf("model=%q embedding=%v", model, embedding)
	}
}

func TestLegacyAliasKeepsOldVectorsSearchable(t *testing.T) {
	query := "openai:BAAI/bge-base-en-v1.5"
	legacy := map[string]string{"embedding_model": "nomic-embed-text"}

	withLegacyEmbedAliases(t, nil)
	if semanticHitMatches(legacy, map[string]interface{}{}, query) {
		t.Fatal("without an alias a different model tag must not match")
	}

	withLegacyEmbedAliases(t, []string{" nomic-embed-text ", "", "nomic-embed-text"})
	if !slices.Equal(legacyEmbedModelAliases, []string{"nomic-embed-text"}) {
		t.Fatalf("aliases not cleaned: %v", legacyEmbedModelAliases)
	}
	for tag, want := range map[string]bool{
		"nomic-embed-text":             true,
		"BAAI/bge-base-en-v1.5":        true,
		"openai:BAAI/bge-base-en-v1.5": true,
		"":                             true,
		"text-embedding-3-small":       false,
		"ollama:nomic-embed-text":      false,
	} {
		if got := semanticHitMatches(map[string]string{"embedding_model": tag}, map[string]interface{}{}, query); got != want {
			t.Errorf("tag %q matches = %v, want %v", tag, got, want)
		}
	}

	filter := recallFilterWithModel("", nil, query, RecallOptions{})
	must, _ := filter["must"].([]interface{})
	var compatible []string
	for _, clause := range must {
		should, _ := clause.(map[string]interface{})["should"].([]interface{})
		for _, option := range should {
			if match, ok := option.(map[string]interface{})["match"].(map[string]interface{}); ok {
				compatible, _ = match["any"].([]string)
			}
		}
	}
	for _, tag := range []string{query, "BAAI/bge-base-en-v1.5", "nomic-embed-text", ""} {
		if !slices.Contains(compatible, tag) {
			t.Errorf("Qdrant filter compatible list %v lacks %q", compatible, tag)
		}
	}
}
