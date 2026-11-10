package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds all broker configuration loaded from YAML.
type Config struct {
	Server struct {
		Bind    string `yaml:"bind"`
		Port    string `yaml:"port"`
		Timeout int    `yaml:"timeout_seconds"`
	} `yaml:"server"`

	Redis struct {
		Addr     string `yaml:"addr"`
		Password string `yaml:"password"`
		DB       int    `yaml:"db"`
	} `yaml:"redis"`

	SQLite struct {
		Path string `yaml:"path"`
	} `yaml:"sqlite"`

	Vault struct {
		Path string `yaml:"path"`
	} `yaml:"vault"`

	Auth struct {
		APIKeyHeader string `yaml:"api_key_header"`
		TenantHeader string `yaml:"tenant_header"`
	} `yaml:"auth"`

	// RateLimit bounds per-tenant request throughput. Every tenant gets the
	// same default; there are no plan tiers.
	RateLimit struct {
		DefaultRPM int `yaml:"default_rpm"`
	} `yaml:"rate_limit"`

	Embed struct {
		Provider             string `yaml:"provider"`
		StrictInput          bool   `yaml:"strict_input"`
		ModelRevision        string `yaml:"model_revision"`
		QueryCacheEntries    int    `yaml:"query_cache_entries"`
		QueryCacheTTLSeconds int    `yaml:"query_cache_ttl_seconds"`
		OllamaURL            string `yaml:"ollama_url"`
		OllamaModel          string `yaml:"ollama_model"`
		OpenAIKey            string `yaml:"openai_key"`
		OpenAIModel          string `yaml:"openai_model"`
		OpenAIURL            string `yaml:"openai_url"` // override OpenAI base URL (e.g., http://localhost:8080/v1/embeddings for local TEI)
		// LegacyModelAliases are unqualified embedding_model tags on stored
		// vectors that share the active model's vector space. Retrieval treats
		// them as the active model. See loadConfig for the inferred default.
		LegacyModelAliases []string `yaml:"legacy_model_aliases"`
	} `yaml:"embed"`

	// Consolidation governs the ACT-R style memory consolidation worker:
	// how often it sweeps, when to forget (vault) a document, and how
	// fast each namespace decays. Defaults match the original hardcoded
	// constants (1h interval, A_i < -2.0 threshold, ops=3 notes=1 self=0.1).
	Consolidation struct {
		IntervalSeconds    int                `yaml:"interval_seconds"`
		PromotionThreshold float64            `yaml:"promotion_threshold"`
		DecayRates         map[string]float64 `yaml:"decay_rates"`

		// PromoteLive arms the destructive half of consolidation. When false
		// the worker logs what it *would* vault and changes nothing — the
		// dry-run mode the deployed broker.yaml comments have long described
		// but which no code implemented until now. Defaults to false: an
		// operator must opt in to a worker that deletes from the active set.
		PromoteLive *bool `yaml:"promote_live"`

		// DecayDefault is the decay rate applied to documents whose namespace
		// has no entry in DecayRates (including the empty namespace, which is
		// every document written before namespace persistence landed).
		// Defaults to 1.0, the historical hardcoded fallback.
		DecayDefault float64 `yaml:"decay_default"`

		// DemoteFraction switches consolidation from an absolute activation
		// threshold to a relative one: demote the lowest-activation N of the
		// tenant's corpus, expressed as a fraction (0.20 = bottom 20%).
		//
		// Why this exists: A_i = log(access_count) - d*log(age+1) is unbounded
		// downward, so an absolute threshold has no stable operating point. On
		// the production corpus, 64% of documents have never been re-read, and
		// for those log(access_count) = 0, making A_i strictly negative and
		// falling forever. Every (rate, threshold) pair therefore converges on
		// vaulting that whole 64%, just at different speeds. A relative cut is
		// scale- and age-invariant by construction.
		// See docs/architecture/archive-vs-working-set.md 4.4 and 5a.
		//
		// 0 (the default) keeps the legacy absolute-threshold behaviour.
		DemoteFraction float64 `yaml:"demote_fraction"`

		// DemoteFloor caps how many documents one sweep may demote, as a guard
		// against a mis-set DemoteFraction emptying a corpus. Applies only when
		// DemoteFraction > 0. Default 1000.
		DemoteFloor int `yaml:"demote_max_per_sweep"`
	} `yaml:"consolidation"`

	Query struct {
		// ScanCap bounds the number of documents a single /query cosine scan
		// will examine. 0 means unbounded (the historical behaviour).
		ScanCap int `yaml:"scan_cap"`
	} `yaml:"query"`

	// Ontology and Graph (below) are accepted and ignored so a config written
	// for the mainline broker still loads instead of failing the strict key check.
	Ontology map[string]interface{} `yaml:"ontology"`

	// Features is a forward-compatibility bag for rollout flags. The keys are
	// deliberately not enumerated as struct fields: the deployed config
	// carries flags for stages that do not exist in this source tree, and
	// strict decoding would refuse to start rather than ignore them. Reading
	// them into a map keeps them visible (they are logged at boot) without
	// pretending they do anything.
	Features map[string]bool `yaml:"features"`

	// Graph is accepted and ignored; see Ontology above.
	Graph map[string]interface{} `yaml:"graph"`
}

// embedOllamaURLExplicitlyEmpty reports whether the config sets embed.ollama_url
// to an empty value, as opposed to leaving it out. The typed decode cannot tell
// the two apart, and only the explicit form means "Ollama is off".
func embedOllamaURLExplicitlyEmpty(data []byte) (bool, error) {
	var raw struct {
		Embed map[string]interface{} `yaml:"embed"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return false, err
	}
	value, present := raw.Embed["ollama_url"]
	if !present {
		return false, nil
	}
	if value == nil {
		return true, nil
	}
	text, isString := value.(string)
	return isString && strings.TrimSpace(text) == "", nil
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	// Decode strictly (KnownFields) so a key the broker does not understand is
	// a startup error rather than silence. A silently-ignored key is worse
	// than a missing one: `promote_live: true` sat in the deployed config for
	// weeks reading as "vaulting is armed and tuned" while no code consumed
	// it. A typo'd `promotion_threshhold` would likewise have left the
	// threshold at its default with the config appearing to set it.
	//
	// Unknown keys under `features:` are exempt — it decodes into a map.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		// An empty config file is legitimate (every field has a default).
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	ollamaDisabled, err := embedOllamaURLExplicitlyEmpty(data)
	if err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	// Apply defaults
	if cfg.Embed.QueryCacheEntries < 0 || cfg.Embed.QueryCacheEntries > 4096 || cfg.Embed.QueryCacheTTLSeconds < 0 || cfg.Embed.QueryCacheTTLSeconds > 3600 {
		return nil, fmt.Errorf("invalid embedding cache bounds")
	}
	if cfg.Embed.QueryCacheEntries > 0 && (!cfg.Embed.StrictInput || cfg.Embed.ModelRevision == "" || cfg.Embed.QueryCacheTTLSeconds == 0) {
		return nil, fmt.Errorf("embedding cache requires strict_input, model_revision and positive TTL")
	}
	if cfg.Server.Bind == "" {
		cfg.Server.Bind = "127.0.0.1"
	}
	if cfg.Server.Port == "" {
		cfg.Server.Port = "8080"
	}
	if cfg.Server.Timeout == 0 {
		cfg.Server.Timeout = 30
	}
	if cfg.Redis.Addr == "" {
		cfg.Redis.Addr = "127.0.0.1:6379"
	}
	if cfg.SQLite.Path == "" {
		cfg.SQLite.Path = "/var/lib/heurchain/broker.db"
	}
	if cfg.Vault.Path == "" {
		cfg.Vault.Path = "/opt/vault"
	}
	if cfg.Auth.APIKeyHeader == "" {
		cfg.Auth.APIKeyHeader = "X-HeurChain-API-Key"
	}
	if cfg.Auth.TenantHeader == "" {
		cfg.Auth.TenantHeader = "X-HeurChain-Tenant"
	}
	if cfg.RateLimit.DefaultRPM <= 0 {
		cfg.RateLimit.DefaultRPM = 60
	}
	switch provider := strings.ToLower(strings.TrimSpace(cfg.Embed.Provider)); provider {
	case "":
		cfg.Embed.Provider = ""
	case "ollama", "openai":
		cfg.Embed.Provider = provider
	default:
		return nil, fmt.Errorf("embed.provider must be ollama or openai")
	}
	// An explicit `ollama_url: ""` turns Ollama off; the default below would
	// otherwise turn it back on. 0.4.x tolerated that: it tried the default
	// Ollama URL, failed, and fell back to the OpenAI-compatible endpoint, while
	// tagging the stored vectors with the Ollama model name. 0.5.0 removed the
	// fallback (it can mix vector spaces), so such a deployment had every
	// embedding fail. It has embedded through the OpenAI-compatible endpoint all
	// along, so that is its provider, and its old Ollama-named tags are aliases.
	if ollamaDisabled {
		switch cfg.Embed.Provider {
		case "":
			cfg.Embed.Provider = "openai"
			if len(cfg.Embed.LegacyModelAliases) == 0 {
				alias := strings.TrimSpace(cfg.Embed.OllamaModel)
				if alias == "" {
					alias = "nomic-embed-text"
				}
				cfg.Embed.LegacyModelAliases = []string{alias}
			}
			log.Printf("config: embed.ollama_url is empty and embed.provider is unset; using provider openai, legacy model aliases %v", cfg.Embed.LegacyModelAliases)
		case "ollama":
			return nil, fmt.Errorf("embed.provider is ollama but embed.ollama_url is empty")
		}
	}
	if cfg.Embed.OllamaURL == "" {
		cfg.Embed.OllamaURL = "http://localhost:11434"
	}
	if cfg.Embed.OllamaModel == "" {
		cfg.Embed.OllamaModel = "nomic-embed-text"
	}
	if cfg.Embed.OpenAIModel == "" {
		cfg.Embed.OpenAIModel = "text-embedding-3-small"
	}
	// OpenAIKey may legitimately be empty (Ollama-only mode).
	if cfg.Embed.OpenAIKey == "" {
		cfg.Embed.OpenAIKey = os.Getenv("OPENAI_API_KEY")
	}
	if cfg.Embed.OpenAIURL == "" {
		cfg.Embed.OpenAIURL = "https://api.openai.com/v1/embeddings"
	}
	if cfg.Consolidation.IntervalSeconds <= 0 {
		cfg.Consolidation.IntervalSeconds = 3600
	}
	if cfg.Consolidation.PromotionThreshold == 0 {
		cfg.Consolidation.PromotionThreshold = -2.0
	}
	if cfg.Consolidation.DecayRates == nil {
		cfg.Consolidation.DecayRates = map[string]float64{
			"ops":   3.0,
			"notes": 1.0,
			"self":  0.1,
		}
	}
	if cfg.Consolidation.DecayDefault <= 0 {
		cfg.Consolidation.DecayDefault = 1.0
	}
	// A fraction outside (0,1) is meaningless; treat it as "disabled" rather
	// than clamping, so a typo cannot silently demote the whole corpus.
	if cfg.Consolidation.DemoteFraction < 0 || cfg.Consolidation.DemoteFraction >= 1 {
		cfg.Consolidation.DemoteFraction = 0
	}
	if cfg.Consolidation.DemoteFloor <= 0 {
		cfg.Consolidation.DemoteFloor = 1000
	}
	// Fail safe: absent promote_live, consolidation runs in dry-run and
	// deletes nothing. Arming a destructive worker must be explicit.
	if cfg.Consolidation.PromoteLive == nil {
		live := false
		cfg.Consolidation.PromoteLive = &live
	}

	// Surface flags that are present in config but consumed by nothing, so a
	// stale flag is visible in the boot log instead of quietly implying a
	// behaviour the binary does not have. Flags this build DOES read are
	// excluded — listing them as "not consumed" is the same false signal in
	// reverse, and cross_agent_visibility was reported that way for one deploy
	// after Phase 7 landed.
	if unconsumed := unconsumedFeatures(cfg.Features); len(unconsumed) > 0 {
		log.Printf("config: %d feature flag(s) present but not consumed by this build: %v",
			len(unconsumed), unconsumed)
	}

	return &cfg, nil
}

// consumedFeatures lists feature keys this build actually reads. Add a key here
// in the same change that starts consuming it, so the boot log stays honest in
// both directions.
var consumedFeatures = map[string]bool{
	"cross_agent_visibility": true,
}

// unconsumedFeatures returns the configured flags that no code reads.
func unconsumedFeatures(features map[string]bool) map[string]bool {
	out := make(map[string]bool, len(features))
	for k, v := range features {
		if !consumedFeatures[k] {
			out[k] = v
		}
	}
	return out
}
