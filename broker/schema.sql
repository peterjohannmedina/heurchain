-- ---------------------------------------------------------------------------
-- HeurChain MVP — SQLite Schema
-- ---------------------------------------------------------------------------
-- Tenant isolation via tenant_id UUID on every table.
-- API keys hashed with bcrypt (NOT SHA-256).
-- ---------------------------------------------------------------------------

PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;

-- ---------------------------------------------------------------------------
-- Tenants
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tenants (
    id              TEXT PRIMARY KEY,  -- UUID v4
    name            TEXT NOT NULL,
    email           TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'cancelled')),
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_tenants_status ON tenants(status);

-- ---------------------------------------------------------------------------
-- API Keys
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS api_keys (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id       TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    key_hash        TEXT NOT NULL,  -- bcrypt hash of the raw key
    name            TEXT NOT NULL DEFAULT 'default',
    rate_limit_qps  INTEGER NOT NULL DEFAULT 10,
    rate_limit_burst INTEGER NOT NULL DEFAULT 20,
    last_used_at    DATETIME,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    revoked_at      DATETIME
);

CREATE INDEX IF NOT EXISTS idx_api_keys_tenant ON api_keys(tenant_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(key_hash);

-- ---------------------------------------------------------------------------
-- Usage Events (per-tenant activity counters)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS usage_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id       TEXT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_type      TEXT NOT NULL CHECK (event_type IN ('store', 'query', 'embed', 'search', 'flush')),
    tokens_used     INTEGER NOT NULL DEFAULT 0,
    vectors_stored  INTEGER NOT NULL DEFAULT 0,
    metadata        TEXT,  -- JSON blob
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_usage_tenant ON usage_events(tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_date ON usage_events(created_at);

-- ---------------------------------------------------------------------------
-- Audit Log
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id       TEXT REFERENCES tenants(id) ON DELETE SET NULL,
    actor           TEXT NOT NULL,  -- api_key_id or 'system'
    action          TEXT NOT NULL,
    resource        TEXT,
    ip_address      TEXT,
    user_agent      TEXT,
    success         BOOLEAN NOT NULL DEFAULT 1,
    details         TEXT,  -- JSON blob
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_audit_tenant ON audit_log(tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_log(action, created_at);

-- ---------------------------------------------------------------------------
-- Schema Version (migration tracking)
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS schema_version (
    version         INTEGER PRIMARY KEY,
    applied_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    description     TEXT
);

INSERT OR IGNORE INTO schema_version (version, description) VALUES (1, 'Initial MVP schema');

-- ---------------------------------------------------------------------------
-- Migration v2 — agent_id tagging
-- Fresh-database bootstrap only. Never replay this file against an existing
-- broker database; the Go broker owns version-gated upgrades. The base table
-- intentionally omits these columns so a fresh schema applies the same v2
-- transition as an existing v1 database.
-- ---------------------------------------------------------------------------

-- Add agent_id to usage_events (existing rows will be NULL).
ALTER TABLE usage_events ADD COLUMN agent_id TEXT;

-- Add agent_id to audit_log (existing rows will be NULL).
ALTER TABLE audit_log ADD COLUMN agent_id TEXT;

-- Index agent_id on usage_events for the /agents listing query.
CREATE INDEX IF NOT EXISTS idx_usage_agent ON usage_events(tenant_id, agent_id, created_at);

INSERT OR IGNORE INTO schema_version (version, description) VALUES (2, 'Add agent_id to usage_events and audit_log');
