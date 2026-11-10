package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/redis/go-redis/v9"
	_ "modernc.org/sqlite"
)

func main() {
	configPath := flag.String("config", "/etc/heurchain/broker.yaml", "Path to broker config file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	setLegacyEmbedModelAliases(cfg.Embed.LegacyModelAliases)
	defaultQdrantClient, err = qdrantClientFromEnvironment()
	if err != nil {
		log.Fatalf("Failed to configure Qdrant: %v", err)
	}

	// --- SQLite --------------------------------------------------------------
	// Pragmas are passed in the DSN so EVERY connection gets them:
	// secure_delete and foreign_keys are per-connection, and database/sql
	// silently replaces a connection after driver errors — an Exec-only
	// pragma would be lost on the replacement. The explicit Exec loop below still runs for first-time
	// file setup and as a fail-fast check.
	dsn := cfg.SQLite.Path
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}
	dsn += "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Fatalf("Failed to open SQLite: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1) // modernc.org/sqlite does not support concurrent writes
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Set WAL mode and sync pragmas explicitly (modernc.org/sqlite ignores DSN pragma keys)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
		// Overwrite deleted content with zeros so deleted rows (revoked keys,
		// removed tenants) do not linger in freed B-tree pages.
		"PRAGMA secure_delete=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			log.Fatalf("Failed to set pragma %q: %v", pragma, err)
		}
	}

	if err := db.Ping(); err != nil {
		log.Fatalf("Failed to ping SQLite: %v", err)
	}
	log.Println("SQLite connected:", cfg.SQLite.Path)

	// Apply pending schema migrations (v3+) exactly once, transactionally.
	// An empty database is seeded from the embedded baseline first; the broker
	// refuses to serve on a half-migrated schema.
	if err := runMigrations(db); err != nil {
		log.Fatalf("Schema migration failed: %v", err)
	}

	// --- Redis ---------------------------------------------------------------
	// DisableIndentity suppresses the CLIENT SETINFO handshake go-redis sends on
	// every new connection. That handshake is the trigger for GO-2025-3540: if
	// it times out during connection establishment, an unread reply is left on
	// a connection that then returns to the pool, and a later command can read
	// the previous command's response. On a pool shared by every tenant, in a
	// system whose isolation is key-namespacing over one Redis, that is a
	// cross-tenant response-mixing risk. The labels it provides are cosmetic,
	// so removing the call eliminates the vulnerable path outright — a version
	// bump to >= v9.7.3 is still worth doing, but this does not depend on it.
	// (Field name is go-redis's own spelling, typo included.)

	// Request-path pool — sized for concurrent handlers.
	rdb := redis.NewClient(&redis.Options{
		Addr:             cfg.Redis.Addr,
		Password:         cfg.Redis.Password,
		DB:               cfg.Redis.DB,
		PoolSize:         50,
		MinIdleConns:     5,
		PoolTimeout:      2 * time.Second, // fail fast instead of hanging 30s
		DialTimeout:      2 * time.Second,
		ReadTimeout:      3 * time.Second,
		WriteTimeout:     3 * time.Second,
		DisableIndentity: true,
	})
	// Background worker pool — separate so workers can't starve HTTP handlers.
	bgRdb := redis.NewClient(&redis.Options{
		Addr:             cfg.Redis.Addr,
		Password:         cfg.Redis.Password,
		DB:               cfg.Redis.DB,
		PoolSize:         5,
		MinIdleConns:     1,
		PoolTimeout:      5 * time.Second,
		DialTimeout:      2 * time.Second,
		ReadTimeout:      5 * time.Second,
		WriteTimeout:     5 * time.Second,
		DisableIndentity: true,
	})
	log.Println("Redis configured:", cfg.Redis.Addr)

	// --- HTTP Server ---------------------------------------------------------
	srv := &Server{config: cfg, rdb: rdb, db: db}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	semanticIndexDone := RunSemanticIndexWorker(ctx, bgRdb, defaultQdrantClient)
	// Workers use bgRdb so they cannot starve the request-path pool.
	// Initial run is delayed 2 min so the broker reaches steady state first.
	consolidationInterval := time.Duration(cfg.Consolidation.IntervalSeconds) * time.Second
	SetConsolidationConfig(&ConsolidationConfig{
		Interval:           consolidationInterval,
		PromotionThreshold: cfg.Consolidation.PromotionThreshold,
		DecayRates:         cfg.Consolidation.DecayRates,
		PromoteLive:        *cfg.Consolidation.PromoteLive,
		DecayDefault:       cfg.Consolidation.DecayDefault,
		DemoteFraction:     cfg.Consolidation.DemoteFraction,
		DemoteFloor:        cfg.Consolidation.DemoteFloor,
	})
	if crossAgentEnabled(cfg) {
		log.Println("cross_agent_visibility ON (cooperative): per-agent corpus isolation enforced on " +
			"/query. NOT a security boundary - agent identity is self-declared under the shared tenant " +
			"key. /search (Qdrant path), the manifest, sessions, vault export and cache are NOT " +
			"filtered. Organization control only.")
	}
	SetVecScanCap(cfg.Query.ScanCap)
	if cfg.Query.ScanCap > 0 {
		log.Printf("Query: brute-force cosine scan capped at %d vectors/tenant", cfg.Query.ScanCap)
	}
	mode := "DRY-RUN (vaults nothing)"
	if *cfg.Consolidation.PromoteLive {
		mode = "LIVE (vaults below threshold)"
	}
	selector := fmt.Sprintf("threshold=%.2f (absolute)", cfg.Consolidation.PromotionThreshold)
	if cfg.Consolidation.DemoteFraction > 0 {
		selector = fmt.Sprintf("rank-cut=bottom %.0f%% (max %d/sweep)",
			cfg.Consolidation.DemoteFraction*100, cfg.Consolidation.DemoteFloor)
	}
	log.Printf("Consolidation: %s interval=%s %s decay_default=%.2f decay_rates=%v",
		mode, consolidationInterval, selector,
		cfg.Consolidation.DecayDefault, cfg.Consolidation.DecayRates)
	RunConsolidationWorker(ctx, bgRdb, srv.db, consolidationInterval)

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.logRequest(srv.handleDiscovery))
	mux.HandleFunc("/health", srv.logRequest(srv.handleHealth))
	mux.HandleFunc("/ready", srv.logRequest(srv.handleReady))
	mux.HandleFunc("/openapi.json", srv.logRequest(srv.handleOpenAPI))
	mux.HandleFunc("/store", srv.logRequest(srv.tenantStatusGuard(srv.rpmGuard(srv.handleStore))))
	mux.HandleFunc("/query", srv.logRequest(srv.tenantStatusGuard(srv.rpmGuard(srv.handleQuery))))
	mux.HandleFunc("/search", srv.logRequest(srv.tenantStatusGuard(srv.rpmGuard(srv.handleSearch))))
	mux.HandleFunc("/embed", srv.logRequest(srv.tenantStatusGuard(srv.rpmGuard(srv.handleEmbed))))
	mux.HandleFunc("/promote", srv.logRequest(srv.tenantStatusGuard(srv.rpmGuard(srv.handlePromote))))
	mux.HandleFunc("/session/start", srv.logRequest(srv.tenantStatusGuard(srv.rpmGuard(srv.handleSessionStart))))
	mux.HandleFunc("/session/end", srv.logRequest(srv.tenantStatusGuard(srv.rpmGuard(srv.handleSessionEnd))))
	mux.HandleFunc("/session/", srv.logRequest(srv.tenantStatusGuard(srv.rpmGuard(srv.handleSessionCtx))))
	mux.HandleFunc("/agent/", srv.logRequest(srv.tenantStatusGuard(srv.rpmGuard(srv.handleAgentRecall))))
	mux.HandleFunc("/mcp/sse", srv.logRequest(srv.handleMCPSSE))
	mux.HandleFunc("/mcp/message", srv.logRequest(srv.handleMCPMessage))
	mux.HandleFunc("/admin/tenants", srv.logRequest(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			srv.handleAdminCreateTenant(w, r)
		case http.MethodGet:
			srv.handleAdminListTenants(w, r)
		default:
			jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	mux.HandleFunc("/admin/tenants/", srv.logRequest(srv.handleAdminRevokeKey))
	// NOTE: /vault/* deliberately carries no guard middleware here. These
	// handlers take tenant_id from the QUERY STRING and authenticate in-handler
	// (API key, admin basic auth), whereas tenantStatusGuard/rpmGuard read the
	// tenant HEADER. Wrapping these routes in that middleware 400s every
	// legitimate call. See vault.go.
	mux.HandleFunc("/vault/flush-working-memory", srv.logRequest(srv.handleVaultFlush))
	mux.HandleFunc("/vault/export", srv.logRequest(srv.handleVaultExport))
	mux.HandleFunc("/vault/stats", srv.logRequest(srv.handleVaultStats))

	// Import lives under /admin/ because it is restore tooling and a bulk write
	// path. The handler keeps its own admin-auth check, so this is defence in
	// depth rather than a replacement. Registered on the exact pattern, which
	// Go's ServeMux prefers over the "/admin/" subtree.
	mux.HandleFunc("/admin/vault/import", srv.logRequest(srv.handleVaultImport))
	mux.HandleFunc("/admin/vault/rehydrate", srv.logRequest(srv.handleVaultRehydrate))
	mux.HandleFunc("/memory/manifest", srv.logRequest(srv.tenantStatusGuard(srv.handleMemoryManifest)))
	for path, handler := range map[string]http.HandlerFunc{
		"/memory/get": srv.handleMemoryGet, "/memory/list": srv.handleMemoryList,
		"/memory/update": srv.handleMemoryUpdate, "/memory/forget": srv.handleMemoryForget,
		"/memory/checkpoint": srv.handleMemoryCheckpoint, "/memory/resume": srv.handleMemoryResume,
	} {
		mux.HandleFunc(path, srv.logRequest(srv.tenantStatusGuard(srv.rpmGuard(handler))))
	}
	mux.HandleFunc("/memory/provenance", srv.logRequest(srv.tenantStatusGuard(srv.rpmGuard(srv.handleMemoryProvenance))))
	mux.HandleFunc("/vectors/", srv.logRequest(srv.tenantStatusGuard(srv.rpmGuard(srv.handleVectorHistory))))
	mux.HandleFunc("/agents", srv.logRequest(srv.handleAgents))
	mux.HandleFunc("/agent-settings", srv.logRequest(srv.handleAgentSettings))
	// Agent provenance: source IPs / user-agents / keys / activity, so a user can
	// tell apart same-named agent instances across machines.
	//
	// tenantStatusGuard, not rpmGuard: rpmGuard always validates an API key and
	// 401s before reaching the handler. tenantStatusGuard only reads the tenant
	// header for the status check; resolveVaultCaller inside the handler still
	// performs full authentication.
	mux.HandleFunc("/agent-provenance", srv.logRequest(srv.tenantStatusGuard(srv.handleAgentProvenance)))
	mux.HandleFunc("/admin/", srv.logRequest(srv.handleAdmin))
	mux.HandleFunc("/admin/db-version", srv.logRequest(srv.handleDBVersion))

	bindAddr := fmt.Sprintf("%s:%s", cfg.Server.Bind, cfg.Server.Port)
	httpServer := &http.Server{
		Addr:         bindAddr,
		Handler:      mux,
		ReadTimeout:  time.Duration(cfg.Server.Timeout) * time.Second,
		WriteTimeout: time.Duration(cfg.Server.Timeout) * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Graceful shutdown
	idleConnsClosed := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM) // SIGHUP excluded — use systemctl reload
		<-sigCh
		log.Println("Shutting down server...")
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP shutdown error: %v", err)
		}
		select {
		case <-semanticIndexDone:
		case <-shutdownCtx.Done():
			log.Printf("Semantic index shutdown deadline exceeded; unacknowledged work remains in Redis")
		}
		_ = bgRdb.Close()
		close(idleConnsClosed)
	}()

	// Bind before announcing readiness. Under Type=notify, systemd treats the
	// READY notification as "the service is usable", and `systemctl restart`
	// returns to the deploy helper at that point. Announcing before the listener
	// exists means the health check that immediately follows a restart can race
	// a socket that is not accepting yet — and the same race sits inside the
	// rollback path, where losing it makes a successful rollback report failure.
	listener, err := net.Listen("tcp", bindAddr)
	if err != nil {
		log.Fatalf("HTTP listen error: %v", err)
	}

	if _, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		log.Printf("systemd notify error: %v", err)
	} else {
		log.Println("Sent systemd READY notification")
	}

	log.Printf("HeurChain broker listening on %s", bindAddr)
	if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP server error: %v", err)
	}

	<-idleConnsClosed
	log.Println("Server stopped gracefully")
}
