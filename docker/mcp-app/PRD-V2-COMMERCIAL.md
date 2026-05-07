# HeurChain PRD — V2 Commercial

**Status:** Planning  
**Branch target:** `commercial` (forked from v1.3.0)  
**Author:** Peter Medina  
**Created:** 2026-05-07

---

## 1. Context

HeurChain v1.3.0 ships a working single-tenant MCP memory server: Redis + Obsidian vault + BM25 search + session buffer + procedural memory + frictionless REST layer. It is deployed as a Docker stack on a homelab node and used by OpenClaw agents.

The original architecture (README.old.md) described a significantly more capable system — ACT-R cognitive decay, LLM cue compression, PostgreSQL/Qdrant Tier 2, RRF fusion, Langfuse observability — none of which are implemented in v1.3.0.

This PRD defines the V2 roadmap that closes that gap **and** extends the architecture to support commercial deployment: multi-tenant, multi-agent, and SMB-ready.

---

## 2. Problem Statement

### What v1.3.0 solves
- Agents start each session with zero memory of prior context
- Manual `store_memory()` call requirements cause silent memory loss
- BM25 search was broken (Redis keys never indexed — fixed in v1.3.0)
- SSE tool definition overhead (~1,450 tokens) is wasteful for HTTP-only use

### What v1.3.0 does NOT solve
- **Index rot** — Redis accumulates everything indefinitely; no aging, no pruning. Quality degrades over time.
- **Semantic gap** — BM25 finds exact keywords, not concepts. "How do I handle condensate?" won't find a note titled "TD600 NPT steam trap 600 PSIG" without keyword overlap.
- **Compressor cost** — Every memory write stores full content. No compression step means Tier 1 grows unbounded.
- **No observability** — No way to debug why a retrieval succeeded or failed.
- **Single tenant** — One Redis instance, one vault, one bearer token. No isolation between agents or organizations.
- **No revenue surface** — Nothing to charge for; no billing primitives.

---

## 3. Goals

### V2 Technical Goals
1. Self-organizing memory via ACT-R decay — index quality improves over time without human intervention
2. LLM cue compression — 200-800 token notes compressed to 5-15 token retrieval keys before long-term storage
3. Hybrid search — Qdrant semantic + BM25 keyword + RRF fusion
4. Retrieval observability — every search traced with tier, latency, fusion scores
5. Consolidation merging — semantically similar entries merged at write time, not duplicated

### Commercial Goals
1. Multi-tenant isolation — per-org namespacing, auth, billing metering
2. Multi-agent coordination — multiple agents within a tenant share memory, with write attribution
3. Self-hosted deployment — Docker Compose target for SMB self-hosting (zero cloud dependency)
4. Managed SaaS — hosted offering for teams who don't want to run infra
5. Revenue primitives — usage metering baked in from the start, not bolted on later

---

## 4. Target Market: SMB

### Who This Is For

**Primary:** Small software teams (2-15 engineers) using AI coding agents daily. Pain: agents have no institutional memory. Every sprint restarts cold. Preferences, architectural decisions, and debugging sessions are re-explained weekly.

**Secondary:** SMB professional services firms (legal, accounting, consulting) using AI assistants for client work. Pain: AI assistants don't remember client context, prior work, or team conventions across sessions.

**Out of scope for V2:** Enterprise (compliance/audit requirements add scope), consumer (wrong economics).

### SMB Buying Behavior
- Self-serve preferred — they will not sit through a sales call for a dev tool
- $50-300/month per team is the SMB sweet spot for dev tooling
- Docker Compose self-hosted option required — many SMBs won't send data to a third-party SaaS
- Needs to "just work" — no MLOps expertise, no model hosting, no vector DB administration

---

## 5. Architecture: V2 Target State

```
┌─────────────────────────────────────────────────────┐
│                  TENANT ISOLATION                    │
│  Each org: own Redis namespace + own Qdrant collection│
│  + own vault directory + own Langfuse project        │
└──────────────────────┬──────────────────────────────┘
                       │
         ┌─────────────▼─────────────┐
         │     HeurChain Gateway     │
         │  Auth · Rate Limit · Meter │
         │  Tenant routing · Billing  │
         └──┬──────────┬─────────────┘
            │          │
   ┌─────────▼──┐  ┌───▼──────────┐
   │  MCP SSE   │  │  REST Layer  │
   │  (slim/full│  │  /api/*      │
   │  profiles) │  │  zero tool   │
   │            │  │  overhead    │
   └─────────┬──┘  └───┬──────────┘
             │          │
   ┌──────────▼──────────▼──────────────────────┐
   │              Memory Engine                  │
   │                                             │
   │  ┌──────────────────────────────────────┐  │
   │  │ Tier 1 — Working Memory              │  │
   │  │ Redis + vault                        │  │
   │  │ ACT-R vitality scores                │  │
   │  │ ops/ 3× decay · notes/ 1× · self/ 0.1│  │
   │  │ Consolidation worker watches vitality │  │
   │  └────────────────┬─────────────────────┘  │
   │                   │ below threshold         │
   │  ┌────────────────▼─────────────────────┐  │
   │  │ Consolidation Worker                  │  │
   │  │ 1. Similarity check vs Tier 2         │  │
   │  │ 2. Merge OR insert                    │  │
   │  │ 3. LLM compressor → 5-15 token cue    │  │
   │  │ 4. Cue → Tier 1 notes/               │  │
   │  │ 5. Full content → Tier 2             │  │
   │  └────────────────┬─────────────────────┘  │
   │                   │                         │
   │  ┌────────────────▼─────────────────────┐  │
   │  │ Tier 2 — Long-Term Store             │  │
   │  │ Redis BM25 (current, keep)            │  │
   │  │ Qdrant (new) — dense vector embeddings│  │
   │  │ RRF fusion at retrieval time          │  │
   │  │ Langfuse trace on every search        │  │
   │  └──────────────────────────────────────┘  │
   └────────────────────────────────────────────┘
```

---

## 6. V2 Technical Phases

### Phase 1 — Self-Organizing Memory (ACT-R + Cue Compression)

**What:** Implement the cognitive decay engine and LLM compressor described in README.old.md. This is the core differentiator vs. every other memory system.

**Components:**
- Vitality score on every stored key: `vitality = base_activation + recency_boost + access_frequency_boost`
- ACT-R base-level learning equation: `A_i = ln(Σ t_j^-d)` where `d` is decay rate
- Per-directory decay rates: `ops/` 3×, `notes/` 1×, `self/` 0.1×
- Consolidation worker: runs every 15min, finds keys with `vitality < threshold`
- LLM compressor: `temperature=0.1`, standardized prompt, outputs 5-15 token cue
- Cue written back to Tier 1 `notes/` with wiki-link `[[tier2:chunk_id]]`
- Original key removed from `ops/` after consolidation

**Dependencies:** Ollama API or Anthropic API for compressor LLM. Default: use same model as calling agent (configured per tenant).

**Success criteria:**
- Index size stays bounded over 30-day test — `ops/` keys decay, Tier 2 grows, cue count grows
- Cue discriminativeness: given a cue, Tier 2 search returns the correct note in top-3 results 90%+ of the time

**Scope risk:** Cue quality varies by content type. Technical content (product codes, IDs) cues well; conceptual content cues poorly. Need evaluation harness before declaring success.

---

### Phase 2 — Hybrid Semantic Search (Qdrant + RRF)

**What:** Add Qdrant as a second search tier alongside BM25. Fuse results with RRF at query time.

**Components:**
- `qdrant` service added to docker-compose (single node, local)
- `sentence-transformers` sidecar (Python FastAPI) or direct Ollama embeddings for vector generation
- On every Tier 2 write: embed content, upsert to Qdrant collection `{tenant_id}_longterm`
- At search time: run BM25 and Qdrant in parallel, apply RRF: `score = 1/(k+rank_bm25) + 1/(k+rank_qdrant)` where k=60
- `/api/search` returns RRF-fused results with individual tier scores

**Embedding model:** `nomic-embed-text` (768 dim, 137M params, runs locally via Ollama). Fallback: `text-embedding-3-small` via OpenAI API.

**Multi-tenant:** Each tenant gets a named Qdrant collection. Collection created on tenant provisioning, deleted on tenant offboard.

**Success criteria:**
- Semantic query "how do I handle condensate in high-pressure lines?" retrieves "TD600 NPT stainless 600 PSIG" note when BM25 alone would miss it
- P95 search latency < 200ms for index size up to 10k documents

---

### Phase 3 — Consolidation Merge + Cross-Note Synthesis

**What:** When consolidating a note, check if semantically similar content already exists in Tier 2. Merge rather than duplicate. Periodic synthesis pass extracts behavioral patterns into `self/`.

**Consolidation merge flow:**
1. Before inserting to Tier 2, query Qdrant for top-3 neighbors above cosine threshold (0.85)
2. If neighbor found: compressor LLM decides merge vs. insert with prompt: `"Does this new note add information not in the existing note? Reply YES/NO."`
3. If YES: insert as new chunk. If NO: update existing chunk timestamp + increment access count.

**Cross-note synthesis:**
- Synthesis worker runs weekly (or on-demand)
- Batches the last N `ops/` notes into a compressor prompt: `"What patterns about this user's preferences, working style, or domain expertise appear across these notes?"`
- High-confidence patterns written to `self/` with `0.1×` decay rate
- Low-confidence patterns discarded

**Success criteria:**
- After 30 days, Tier 2 document count stays < 2× note write volume (merge rate > 50% for return-visit topics)
- `self/` contains at least 3 accurate behavioral patterns after 100 session hours

---

### Phase 4 — Observability (Langfuse)

**What:** Trace every retrieval through Langfuse. Makes the memory system debuggable and provides usage data for billing.

**Components:**
- Langfuse SDK injected at search execution
- Each search emits: query, tier hit (BM25 / Qdrant / RRF), result count, fusion scores, latency, tenant_id, agent_id
- Consolidation events traced: note_id, vitality_at_consolidation, cue_generated, merge_or_insert decision
- Dashboard: per-agent retrieval quality, cache hit rates, memory growth over time

**Why this matters commercially:** Langfuse traces become the billing signal. Search count + consolidation count = billable operations. The observability layer and the billing layer are the same layer.

---

## 7. Multi-Tenant Architecture

### Isolation Model

```
Tenant: acme-corp
  ├── Redis namespace:    tenant:acme-corp:*
  ├── Qdrant collection:  acme_corp_longterm
  ├── Vault path:         /vault/acme-corp/
  ├── Auth token:         per-tenant JWT (HS256, tenant_id claim)
  └── Langfuse project:   acme-corp (or sub-org if self-hosted Langfuse)

Agent: acme-corp/agent-1
  ├── Proc namespace:     proc:agent:acme-corp:agent-1:*
  ├── Buffer:             session:{acme-corp}:{session_id}:buffer
  └── Write attribution:  source: "agent:acme-corp:agent-1" on all writes
```

### Tenant Provisioning API

```
POST /admin/tenants
{
  "tenant_id": "acme-corp",
  "plan": "team",
  "agent_limit": 10,
  "storage_limit_mb": 500
}
```

Provisions: Redis namespace ACL, Qdrant collection, vault directory, JWT signing key, Langfuse project.

### Auth Model

Current: single global `MCP_AUTH_TOKEN` bearer token.

V2: JWT per tenant. Token includes `tenant_id`, `agent_id`, `plan`, `expires_at`. Gateway validates JWT, injects `tenant_id` into every storage and search operation.

Admin endpoints protected by separate `ADMIN_API_KEY` env var.

---

## 8. Revenue Model

### Tier 1 — Self-Hosted (Free / OSS)

Single-tenant deployment via Docker Compose. All V2 features available. No metering. MIT license.

**Why offer free self-hosted?** HeurChain's core value proposition is trust: agents remember things the user actually did. Self-hosted means data never leaves the user's infrastructure. This is a requirement for some SMBs, not just a preference. Making it genuinely free (not crippled) builds trust and grows the install base.

**Monetization path:** Self-hosted users who scale to teams become managed SaaS customers.

---

### Tier 2 — Team Managed SaaS ($49/month per workspace)

Hosted on managed infrastructure. Includes:
- Up to 5 agents per workspace
- 100k operations/month (searches + writes + consolidations)
- Langfuse dashboard (hosted)
- Qdrant + BM25 hybrid search
- ACT-R decay + cue compression
- 30-day log retention
- Standard support (email, 48h SLA)

Overage: $0.001 per additional operation.

---

### Tier 3 — Business ($199/month per workspace)

Everything in Team plus:
- Up to 25 agents
- 1M operations/month
- Custom embedding models (bring-your-own Ollama endpoint)
- Namespace-level data export (GDPR compliance)
- SSO (SAML/OIDC)
- Priority support (Slack connect, 8h SLA)

---

### Tier 4 — Enterprise (custom pricing)

- Unlimited agents
- Dedicated infrastructure or private cloud
- Custom retention policies
- SLA with uptime guarantees
- Dedicated onboarding + customer success
- On-premise deployment with license key

---

### Add-On: HeurChain Cloud API (pay-as-you-go)

For developers who want to call HeurChain from Claude Code, Cursor, or custom agents without running their own Docker stack.

```
$0.0005 per search operation
$0.0002 per write operation
$0.002  per consolidation (LLM compressor call)
```

Free tier: 5,000 operations/month.

---

## 9. Deployment Models

### Model A: Self-Hosted Docker Compose (current + V2)

Target user: solo developer or small team with a server. Current deployment on CT 203 is exactly this.

Requirements: Docker 24+, 4GB RAM minimum (8GB with Qdrant), no cloud dependency.

Docker Compose services:
- `heurchain-mcp` — Node.js MCP + REST server
- `redis-cache` — Redis 7
- `qdrant` — Qdrant vector DB (new in V2)
- `consolidation-worker` — Python worker: decay, compression, merge (new in V2)
- `nginx-gateway` — SSL termination, rate limiting
- `langfuse` — optional self-hosted observability (new in V2)

---

### Model B: Managed SaaS (V2 commercial launch target)

HeurChain runs on managed infrastructure (DigitalOcean / Hetzner initially — lower cost than AWS for this workload). Customer gets:
- API endpoint: `https://api.heurchain.io/`
- Web dashboard: tenant management, agent list, memory explorer, Langfuse traces
- Provisioned via signup → Stripe → auto-provision

Infrastructure per region: Kubernetes (k3s) with tenant isolation via namespace. Qdrant deployed as a cluster with one collection per tenant.

---

### Model C: Private Cloud (enterprise)

Customer's cloud account, HeurChain-managed Helm chart deployment. License key validates against HeurChain license API. Telemetry opt-out available.

---

## 10. SMB Go-To-Market

### Channels

1. **npm / GitHub** — existing presence. README directs to managed SaaS for teams.
2. **MCP marketplace** (Anthropic, Cursor, Windsurf registries) — list HeurChain as a first-class MCP server. Link to managed SaaS in listing.
3. **r/LocalLLaMA, r/ClaudeAI, Hacker News** — organic developer community. HeurChain's README.old.md already contains the kind of technically grounded narrative that performs well in these communities.
4. **OpenClaw ecosystem** — HeurChain and OpenClaw have overlapping audiences. Cross-promote.

### Developer-Led Growth Playbook

1. Developer discovers HeurChain via npm / GitHub
2. Runs `docker compose up` on their laptop — working in < 5 minutes
3. Uses it solo for a week; sees their Claude agent remember things across sessions
4. Wants to share with their team → hits the agent limit or wants Langfuse dashboard
5. Signs up for Team tier at $49/month — self-serve, no sales call required
6. Team grows → Business tier

The self-hosted free tier is not a gimped product — it's the product. The upgrade trigger is team collaboration and observability, not feature gating.

---

## 11. Commercial Branch Strategy

### Fork point: v1.3.0

The `commercial` branch forks from the v1.3.0 tag. The `master` branch continues as the OSS single-tenant track. Features developed in `commercial` that are non-commercial (e.g., ACT-R decay, cue compression, Qdrant search) will be backported to `master` so the OSS version stays competitive.

Commercial-only features that stay in `commercial` only:
- Tenant provisioning API
- JWT multi-tenant auth
- Billing metering
- Admin dashboard
- SaaS deployment manifests (k3s Helm charts, CI/CD pipelines)

### Branch naming

```
master           — OSS single-tenant (public, MIT)
commercial       — commercial multi-tenant (private)
commercial/v2.0  — V2 release branch
```

---

## 12. V2 Milestones

| Milestone | Deliverable | Target |
|---|---|---|
| M0 | Commercial branch created from v1.3.0 tag | Week 1 |
| M1 | ACT-R decay engine + vitality scoring | Week 3 |
| M2 | LLM compressor + consolidation worker | Week 5 |
| M3 | Qdrant integration + embedding sidecar | Week 7 |
| M4 | RRF fusion at search time | Week 8 |
| M5 | Langfuse tracing (consolidation + search) | Week 9 |
| M6 | Consolidation merge (similarity check before insert) | Week 11 |
| M7 | Cross-note synthesis → `self/` patterns | Week 13 |
| M8 | Multi-tenant JWT auth + tenant provisioning API | Week 15 |
| M9 | Billing metering (operation counters → Stripe webhook) | Week 17 |
| M10 | Self-hosted Docker Compose v2.0 release | Week 18 |
| M11 | SaaS beta (invite-only, 10 teams) | Week 20 |
| M12 | Public SaaS launch | Week 24 |

---

## 13. Open Questions

1. **Compressor LLM cost at scale:** Running a local Ollama model per consolidation event is free on self-hosted. On managed SaaS, this is a GPU inference cost. Does the $49/month price cover it at typical usage volumes? Need to benchmark consolidation frequency per team.

2. **Qdrant persistence on managed SaaS:** Qdrant collections are per-tenant. At 1,000 tenants, how does Qdrant handle 1,000 collections? Need to test collection count limits.

3. **Embedding model standardization:** If tenant A uses `nomic-embed-text` and tenant B uses `text-embedding-3-small`, their vectors live in incompatible spaces. Cross-tenant search (if ever needed) requires a single embedding model. Decision: standardize on one model per deployment; allow override only on enterprise tier.

4. **GitHub license:** OSS `master` is MIT. Commercial branch is private. At what point does the commercial product need a separate EULA or BSL (Business Source License) on the `master` repo? BSL option: MIT for single-tenant self-hosted, BSL for hosted multi-tenant commercial use.

5. **ArcadeDB graph overlay (Phase 3 from README.old.md):** Not included in V2 milestones. Revisit after V2 SaaS launch if cross-agent memory traversal becomes a top user request.

---

## 14. Non-Goals for V2

- **Mobile client** — agent memory, not a consumer app
- **ArcadeDB graph overlay** — too speculative, too much scope
- **Fine-tuning / model training on memory contents** — privacy and cost complexity out of scope
- **Real-time collaboration UI** — Langfuse dashboard covers observability; a custom UI is V3+
- **HIPAA / SOC 2 compliance** — enterprise tier roadmap, not V2
