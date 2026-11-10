package main

// brokerVersion is the single source of truth for the broker's version string.
//
// It is reported by GET /health, GET / (discovery, both JSON and Markdown), and
// GET /openapi.json. Before this constant existed the value was duplicated as
// four separate literals across three files, and they drifted: discovery.go was
// bumped to "0.3.0" on 2026-05-16 as part of an agent-onboarding doc rewrite
// while /health and the OpenAPI document stayed on "0.2.0" — so the same
// deployment advertised two different versions depending on which endpoint you
// asked, and CHANGELOG.md documented neither.
//
// When cutting a release, change this line and add the matching section to
// CHANGELOG.md. Nothing else needs editing.
const brokerVersion = "0.6.1"

// brokerRollbackPolicy controls whether the automated release pipeline may
// restore the previous binary after a failed smoke test. A release containing
// a destructive or backward-incompatible migration must set this to
// "forward-only" and use a migration-specific manual release plan.
// Older binaries do not honor lifecycle tombstones or immutable checkpoints.
// See docs/memory-lifecycle.md for the separate release/rollback prerequisite.
const brokerRollbackPolicy = "forward-only"
