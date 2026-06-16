# BentoClick Architecture

Status: draft
Target version: v0.2 backend
Last updated: 2026-05-21

## 1. Summary

We need to evolve or fork [`Altinity/altinity-mcp`](https://github.com/Altinity/altinity-mcp) to make it a full-featured **AI Gateway in front of a fleet of ClickHouse clusters with business data** — a single control plane that LLM agents, dashboard authors, and end-users hit, which then brokers identity, scope, role, and least-privilege access into many downstream ClickHouse deployments holding real business data.

BentoClick is the product name for that evolved gateway. It is **not** a from-scratch backend.

The "AI Gateway" framing matters because it sets the security posture for the whole design:

- The gateway sits in front of **many** customer ClickHouse clusters, not one. Connection, identity, and role management is multi-cluster from day one.
- The downstream data is **business data** — payments, customer records, operational telemetry. The gateway is the only thing between an LLM agent and that data. Defaults must be conservative; opt-in must be explicit; least-privilege must be enforced at the gateway *and* at ClickHouse.
- LLM agents (claude.ai, ChatGPT, internal agents) are first-class clients alongside browsers. The MCP surface is not a side feature.

altinity-mcp already provides most of the gateway machinery. BentoClick adds the dashboard product surface on top.

### 1.1 What altinity-mcp already provides toward the gateway



- OAuth resource-server posture (PR #110 — "redefine gating mode as pure OAuth resource server") with external AS (Auth0 / Authentik / Keycloak) over RFC 9728 resource metadata, RFC 8707 audience byte-equality, per-tool scope gating.
- Cluster-secret + `Auth.Username` impersonation for ClickHouse, with `WithRoles` per-query (Altinity clickhouse-go fork `feature/interserver-extra-roles` @ `3e22df3`, bumps `DBMS_TCP_PROTOCOL_VERSION` to 54472).
- `altinity.mcp_role_metadata` whitelist resolving the cluster-secret super-admin risk (#107).
- Refresh-token reuse-detection design in CH MergeTree (#106), deferred for v1 but ready when MCP re-enters AS territory.
- Dynamic MCP tools from CH schema (`pkg/server/server_dynamic_tools.go`), MCP resources, `execute_query`, schema discovery — the full `altinity-mcp` tool surface.
- Fail-closed discipline: startup refusal on misconfiguration, ERR + 500 on state failures, no graceful degradation.

### 1.2 What BentoClick adds on top to complete the gateway

- Dashboard product surface (drafts, versions, publish, share).
- SPA runtime serving with versioned artifacts.
- Dashboard MCP tool family.
- Control-plane ClickHouse for gateway state (dashboards, shares, audit, artifact registry).
- Multi-cluster awareness on the dashboard side (a dashboard targets one of many connected CH clusters, identified by `connection_id`).

Everything OAuth, everything CH-proxy, everything role-binding is inherited from altinity-mcp unchanged.

```text
Browser / LLM Agent / API Client
          |
          v
+----------------------------------+
| BentoClick (evolved altinity-mcp)|
|----------------------------------|
| [inherited from altinity-mcp]    |
|   OAuth resource server          |
|   JWT validation (external AS)   |
|   MCP server (HTTP)              |
|   CH dynamic tools, execute_query|
|   cluster_secret + impersonation |
|   WithRoles per-query            |
|   role-metadata whitelist        |
|                                  |
| [added by BentoClick]            |
|   Dashboard CRUD                 |
|   Sanitize / validate            |
|   Preview / publish              |
|   Sharing (KeeperMap-backed)     |
|   Runtime artifact registry      |
|   SPA serving                    |
|   Dashboard MCP tools            |
+----------------------------------+
   |                |
   |                +---> Control-plane ClickHouse
   |                      (+ Keeper, exposed via KeeperMap engine)
   |                      - dashboards (EmbeddedRocksDB drafts,
   |                        MergeTree versions, KeeperMap published pointer)
   |                      - shares (KeeperMap)
   |                      - artifact registry
   |                      - audit (MergeTree)
   |                      - oauth refresh-state (when re-enabled, #106)
   |
   +-------------------> Data-plane ClickHouse (customer clusters)
                          - analytical query execution
                          - viewer identity via cluster_secret + impersonation
                          - role-bound via WithRoles
```

External authorization server (Auth0 / Authentik / Keycloak) sits outside BentoClick and is reached directly by clients during the OAuth flow.

## 2. Goals

- Extend altinity-mcp with a dashboard product surface without rewriting any of the OAuth, MCP, or ClickHouse-access machinery already in place.
- Move dashboard definitions out of the customer ClickHouse and into a dedicated control-plane ClickHouse.
- Keep the resource-server posture: BentoClick never issues OAuth tokens. The external AS owns DCR, issuance, rotation, reuse detection, revocation.
- Serve versioned SPA artifacts from a registry that the router can reason about during rolling deploys.
- Keep the data path thin: byte-for-byte ClickHouse streaming, no payload parsing.
- Keep all state in ClickHouse (with KeeperMap and EmbeddedRocksDB engines where the access pattern needs them).
- Be honest about residual risk (on-box compromise) and document the mitigations actually in place.

## 3. Non-goals for this version

- Backward compatibility with existing ClickHouse-stored BentoClick v0.1 dashboards.
- Automatic deletion or GC of old runtime artifacts.
- Service-account-based ClickHouse query execution.
- Backend inspection, parsing, or transformation of ClickHouse result payloads on the data path.
- MCP-based admin operations.
- BentoClick acting as an OAuth Authorization Server. (Re-entered only if/when #107's role-picker UX, DCR-less IdPs, or MCP-side grant lifecycle become product requirements — explicitly v2/v3 territory.)
- Multi-tenant org/team model. Dashboards have a single owner.
- A second runtime dependency beyond ClickHouse+Keeper. (Keeper is already there as ClickHouse's coordination backend; KeeperMap surfaces it via SQL.)

## 4. Main architectural decisions

| Area | Decision |
|---|---|
| Backend language | Go (evolved from altinity-mcp) |
| OAuth posture | Pure resource server (inherited from altinity-mcp #110) |
| Token issuance | External AS only (Auth0 / Authentik / Keycloak) |
| ClickHouse access on data plane | cluster_secret + impersonation + WithRoles (inherited) |
| Role enforcement | `altinity.mcp_role_metadata` whitelist + CH RBAC (inherited from #107) |
| Driver | Altinity clickhouse-go fork `feature/interserver-extra-roles` (inherited) |
| Control-plane storage | Dedicated ClickHouse with Keeper |
| Storage engine — versions, audit, artifact registry | `MergeTree` (replicated) |
| Storage engine — drafts, shares, published pointer, distributed locks | `KeeperMap` |
| Why not EmbeddedRocksDB for drafts | Server-local, doesn't replicate across CH replicas; breaks HA control-plane CH (same concern #106 raised for refresh-state) |
| Why KeeperMap for shares | Needs transactional CAS for revoke and atomic insert; ReplacingMergeTree's eventual consistency was tried and rejected during #106 work |
| Why MergeTree for refresh-state | High-volume, append-only; KeeperMap couples correctness to Keeper liveness (per #106 rejection) |
| Customer ClickHouse role | Analytical query engine only |
| Compatibility | No v0.1 dashboard compatibility |
| MCP additions | `dashboard_*` tool family, additive to existing surface |
| ClickHouse username derivation | From verified email (inherited from altinity-mcp H-1 / #105) |
| Forward mode | Supported by altinity-mcp; BentoClick dashboards are gating-mode-only because role binding requires it (#107) |
| Static SPA delivery | Per-node versioned artifact directory + registry with `available_on_nodes` |
| Publish visibility | Owner-only |
| Share link semantics | Pinned to version at share-creation time by default |
| Data path logging | Compact metadata only |
| Control path logging | Detailed, into control-plane CH `audit_events` |
| Admin operations | HTTP only |
| On-box compromise | Accepted residual risk; mitigations layered (see §13) |

## 5. Components

### 5.1 Go backend (evolved altinity-mcp)

Layout extends the existing altinity-mcp tree:

```text
pkg/server/                     # existing: HTTP, MCP, OAuth resource-server
  server_auth_oauth.go            # inherited
  server_auth_jwe.go              # inherited (deferred custom-AS bits)
  server_dynamic_tools.go         # inherited
  server_resources.go             # inherited
  server_query.go                 # inherited
  server.go                       # inherited

pkg/clickhouse/                 # existing: data-plane CH client
  client.go                       # inherited (cluster_secret, impersonation, WithRoles)
  cluster_secret_test.go          # inherited

pkg/dashboards/                 # NEW
  sanitize.go                   # the v0.1-MV chokepoint, reimplemented in Go
  validate.go                   # JSON schema, panel constraints, optional EXPLAIN
  store.go                      # control-plane CH repo (drafts/versions/published)
  preview.go
  publish.go

pkg/shares/                     # NEW
  store.go                      # control-plane CH KeeperMap repo
  tokens.go

pkg/artifacts/                  # NEW
  registry.go                   # control-plane CH repo + available_on_nodes tracking
  server.go                     # static asset serving with version preference

pkg/controlplane/               # NEW
  ch.go                         # control-plane CH client wrapper, migrations
  migrations/

pkg/audit/                      # NEW (or extend existing logging)
  events.go                     # control-path audit append, data-path compact log

pkg/mcp_dashboard/              # NEW
  tools.go                      # dashboard_list, dashboard_read, ...

cmd/bentoclick/                 # NEW binary (or extend cmd/altinity-mcp)
```

Resource-server bits, JWT validation, CH proxy, role binding, dynamic MCP tools are reused unchanged. Dashboard concerns live in new packages.

### 5.2 Control-plane ClickHouse + Keeper

A dedicated, network-isolated ClickHouse deployment, with its Keeper ensemble. Customer data-plane CH clusters remain separate.

Engines used:

- `MergeTree` (replicated) for `dashboard_versions`, `audit_events`, `runtime_artifacts`. Append-heavy and queryable; CH replication handles HA.
- `KeeperMap` for `dashboards_drafts`, `shares`, `published_dashboards`, `leader_locks` — atomic insert/update/delete via Keeper, immediate visibility across all CH replicas, transactional revocation. The path #106's branch history validated empirically when ReplacingMergeTree's race semantics broke share-token revoke.

EmbeddedRocksDB was considered for drafts but rejected: it is server-local and does not replicate across CH replicas, so an HA control-plane CH would have drafts visible on one node and absent on another. KeeperMap fits drafts cleanly (point lookup, mutable, low cardinality) without the replication gap.

Why one CH instance covers it all (no second store, no separate Keeper client):

- All access through a single library (`clickhouse-go`).
- Engine choice tracks consistency needs without changing the deployment surface.
- Operationally, the team already runs CH+Keeper — no new technology.

### 5.3 Data-plane ClickHouse

Customer analytical clusters. Configured per BentoClick deployment via `clickhouse_connections`. Reached through altinity-mcp's existing cluster_secret + impersonation + `WithRoles` path. Nothing new here.

### 5.4 Artifact directory

Per-node directory:

```text
/var/lib/bentoclick/artifacts/
  runtime/
    v1/{manifest.json, index.html, assets/...}
    v2/{manifest.json, index.html, assets/...}
```

`runtime_artifacts` in control-plane CH tracks `available_on_nodes`. On startup and on artifact-install, each node advertises possession. The router prefers nodes that advertise the required version. Publishing to a new runtime version is gated on registry showing quorum availability.

No auto-deletion in v0.2. Admin endpoints expose manual disable/delete.

### 5.5 External authorization server

Auth0, Authentik, Keycloak, or any AS that supports DCR + JWT issuance + RFC 8707 audience. BentoClick advertises it via RFC 9728 resource metadata, exactly as altinity-mcp does today.

## 6. Identity and access

Defers entirely to altinity-mcp's posture (PR #110). Summary:

- External AS owns: authorize, token, refresh rotation, reuse detection, revocation.
- BentoClick (resource server) validates each request's JWT: signature against AS JWKS, `aud` byte-equal to deployment audience, `exp`, scope-to-tool gating.
- ClickHouse username derived from verified email (H-1 / #105). Startup refuses gating + cluster_secret without `require_email_verified=true`.
- Per-query CH access goes through cluster_secret with `Auth.Username` = derived CH user, `WithRoles` = consent-time-selected role from `altinity.mcp_role_metadata` (#107).
- `DEFAULT ROLE NONE` per OAuth-bound CH user. No direct grants on data tables — all data privileges flow through roles. Startup test verifies that WithRoles actually narrows effective access; failure refuses to enable role binding.

The dashboard surface adds **no new identity primitives**. Dashboard ownership uses the same `sub`/`email` claim that already identifies the MCP caller. Dashboard MCP tools authorize using the same scope grammar; suggested verbs:

- `bentoclick:dashboard:read` — `dashboard_list`, `dashboard_read`.
- `bentoclick:dashboard:write` — `dashboard_create`, `dashboard_update`, `dashboard_delete`, `dashboard_validate`, `dashboard_preview`, `dashboard_publish`.

Both verbs are advertised in `scopes_supported` only when explicitly enabled per deployment, matching the `oauth.allow_write_scope` opt-in pattern from #107.

## 7. ClickHouse access on the data path

altinity-mcp's existing path is the data path. There is no BentoClick-specific proxy mode.

- Gating mode + cluster_secret + impersonation + `WithRoles` is the production configuration for BentoClick dashboards. Role binding requires gating mode (per #107's applicability matrix), so BentoClick refuses to render a dashboard against a forward-mode connection.
- Forward mode remains supported in altinity-mcp for MCP-only deployments that don't need role binding. BentoClick dashboards are not advertised against forward-mode connections.
- Data path streams byte-for-byte; no payload parsing or error normalization. SPA handles ClickHouse response semantics.

This collapses what the previous draft of this doc spread across three proxy modes into one inherited mechanism.

## 8. Spec sanitization and validation

The v0.1 SECURITY DEFINER MV chokepoint is reimplemented in Go as `pkg/dashboards/sanitize.go`.

### 8.1 Sanitization at write time

All draft and version writes go through `sanitize`:

- Validates against the versioned schema (`spec_version`).
- Strips or escapes HTML/markup in panel content fields rendered as HTML.
- Rejects unsupported fields or panel types.
- Enforces panel-type-specific constraints (URL allowlists for image panels, etc.).
- Returns a sanitized spec or a structured error.

`pkg/dashboards/store.go` rejects any spec that did not pass `sanitize`. No admin override. No MCP back door. There is exactly one write path. This is the v0.1 SECURITY DEFINER invariant restated in Go, and it must have hard test coverage (the project's existing 90% gate covers it once tests are added).

### 8.2 Validation without execution

`validate` (used by the validate API and preview) runs:

- JSON schema check.
- Required-field check.
- Runtime compatibility check (spec matches declared `runtime_version`).
- Panel-type constraint check.
- Optional `EXPLAIN` against panel SQL using viewer identity (same cluster_secret + impersonation + WithRoles path; no result materialization). Catches broken SQL pre-publish without leaking schema access beyond what the user already has.

### 8.3 SPA renderer trust

The SPA renderer treats spec content as already-sanitized. Renderers do not re-escape. Sanitization stays at one boundary; renderer bugs are not the security backstop.

## 9. Dashboard lifecycle

### 9.1 Draft

Mutable per dashboard, owner-scoped. Stored in `dashboards_drafts` (`EmbeddedRocksDB`). LLM agents can create and update drafts through MCP if authorized as the owner.

### 9.2 Validate

Runs §8.2 checks. Does not promote.

### 9.3 Preview

Temporary renderable view of a draft. Owner-only by default. No public share link. Optional short-lived preview token if needed later. Uses publish's runtime artifact resolution.

### 9.4 Publish

Appends to `dashboard_versions` (MergeTree, append-only) and atomically updates `published_dashboards` (KeeperMap, transactional pointer). Freezes dashboard version ID, runtime artifact version, publication timestamp, owner identity.

A published dashboard remains private to the owner until shared.

### 9.5 Share

Explicit, post-publish. Owner opens the WebUI share action; backend creates a share record in `shares` (KeeperMap).

**Pinned to dashboard_version_id at share-creation time by default.** Owner can choose "share latest" at share-creation time for the rolling-version behavior. The pinned default avoids the scenario where a later publish silently exposes data the original recipient was never meant to see.

KeeperMap gives:

- Atomic insert of a new share row by `share_id`.
- Atomic update of `revoked_at` with immediate visibility — when a user revokes a share, the next validation must see it. (ReplacingMergeTree's merge-lag was the gap that bit us behind #106's branch experiments.)
- Atomic delete for hard removal.

Share record:

```text
shares (KeeperMap, primary key share_id)
  share_id
  dashboard_id
  pinned_version_id     nullable  -- null means "follow latest"
  owner_user_id
  token_hash
  created_at
  revoked_at            nullable
  expires_at            nullable
  last_used_at          nullable
  label                 nullable
```

Share links are revocable. Recipient authentication is a deployment-level choice (open issue, §17).

## 10. MCP additions

altinity-mcp's existing tool surface (`execute_query`, `list_databases`, `list_tables`, `describe_table`, schema discovery, dynamic tools from views/tables) is preserved unchanged.

### 10.1 Dashboard MCP tools (new)

```text
dashboard_list
dashboard_read
dashboard_create
dashboard_update
dashboard_delete
dashboard_validate
dashboard_preview
dashboard_publish
```

Later, conditional on demand:

```text
dashboard_share_create
dashboard_share_revoke
dashboard_runtime_list
dashboard_clone
```

### 10.2 Authorization

Same scope-gating pattern as #107. Dashboard MCP tools require `bentoclick:dashboard:read` or `bentoclick:dashboard:write` in the JWT scopes. Tools that execute SQL (`dashboard_preview`, `dashboard_publish` if it runs EXPLAIN) go through the same cluster_secret + impersonation + WithRoles chain — same identity, same role enforcement, same audit.

`execute_query` from MCP continues to route through `pkg/server/server_query.go` exactly as today; no BentoClick-specific change.

### 10.3 Transport

Streamable HTTP only in v0.2. STDIO and SSE not first-class. (Inherits altinity-mcp's posture.)

## 11. HTTP API

### 11.1 User API (new — dashboards and sharing)

```text
GET    /api/me
GET    /api/dashboards
POST   /api/dashboards
GET    /api/dashboards/{dashboard_id}
PUT    /api/dashboards/{dashboard_id}
DELETE /api/dashboards/{dashboard_id}
POST   /api/dashboards/{dashboard_id}/validate
POST   /api/dashboards/{dashboard_id}/preview
POST   /api/dashboards/{dashboard_id}/publish
POST   /api/dashboards/{dashboard_id}/shares
GET    /api/dashboards/{dashboard_id}/shares
DELETE /api/dashboards/{dashboard_id}/shares/{share_id}
```

### 11.2 Runtime serving (new)

```text
GET /d/{dashboard_id}
GET /s/{share_token}
GET /runtime/{runtime_version}/index.html
GET /runtime/{runtime_version}/assets/{asset}
```

### 11.3 MCP endpoint

Inherited from altinity-mcp. No change.

### 11.4 OAuth resource metadata + data-plane CH proxy

Inherited from altinity-mcp. No change.

### 11.5 Admin API (new)

```text
GET    /admin/api/health
GET    /admin/api/nodes
GET    /admin/api/control-clickhouse/status
GET    /admin/api/artifacts
POST   /admin/api/artifacts
DELETE /admin/api/artifacts/{version}
GET    /admin/api/clickhouse-connections
POST   /admin/api/clickhouse-connections
PUT    /admin/api/clickhouse-connections/{connection_id}
DELETE /admin/api/clickhouse-connections/{connection_id}
GET    /admin/api/audit
```

Admin authorization is stronger than dashboard ownership. Bootstrap admin user comes from deployment config, not from the database.

## 12. Storage model

All control-plane state in one ClickHouse, engine chosen per access pattern.

```sql
-- Drafts: mutable, point lookup, replicated via Keeper for HA.
CREATE TABLE dashboards_drafts (
  dashboard_id  String,
  owner_user_id String,
  name          String,
  description   String,
  spec_json     String,
  updated_at    DateTime64(3),
  deleted_at    Nullable(DateTime64(3))
) ENGINE = KeeperMap('/bentoclick/drafts')
PRIMARY KEY dashboard_id;

-- Versions: append-only history, queryable by dashboard.
CREATE TABLE dashboard_versions (
  version_id       String,
  dashboard_id     String,
  version_number   UInt64,
  spec_json        String,
  runtime_version  String,
  created_by       String,
  created_at       DateTime64(3)
) ENGINE = MergeTree
ORDER BY (dashboard_id, version_number);

-- Published pointer: needs transactional update on republish.
CREATE TABLE published_dashboards (
  dashboard_id        String,
  current_version_id  String,
  owner_user_id       String,
  updated_at          DateTime64(3)
) ENGINE = KeeperMap('/bentoclick/published')
PRIMARY KEY dashboard_id;

-- Shares: needs immediate revocation visibility.
CREATE TABLE shares (
  share_id          String,
  dashboard_id      String,
  pinned_version_id Nullable(String),
  owner_user_id     String,
  token_hash        String,
  created_at        DateTime64(3),
  revoked_at        Nullable(DateTime64(3)),
  expires_at        Nullable(DateTime64(3)),
  last_used_at      Nullable(DateTime64(3)),
  label             Nullable(String)
) ENGINE = KeeperMap('/bentoclick/shares')
PRIMARY KEY share_id;

-- Runtime artifact registry.
CREATE TABLE runtime_artifacts (
  version            String,
  path               String,
  manifest_json      String,
  available_on_nodes Array(String),
  created_at         DateTime64(3),
  disabled_at        Nullable(DateTime64(3))
) ENGINE = ReplacingMergeTree(created_at)
ORDER BY version;

-- ClickHouse connections.
CREATE TABLE clickhouse_connections (
  connection_id     String,
  name              String,
  base_url          String,
  mode              Enum8('gating'=1, 'forward'=2),
  oauth_audience    String,
  username_mapping  String,
  network_allowlist Array(String),
  created_at        DateTime64(3),
  updated_at        DateTime64(3)
) ENGINE = ReplacingMergeTree(updated_at)
ORDER BY connection_id;

-- Users: minimal, populated on first login.
CREATE TABLE users (
  user_id      String,
  email        String,
  display_name String,
  created_at   DateTime64(3),
  last_seen_at DateTime64(3)
) ENGINE = ReplacingMergeTree(last_seen_at)
ORDER BY user_id;

-- Audit: high-volume append-only.
CREATE TABLE audit_events (
  event_id      String,
  actor_user_id String,
  event_type    LowCardinality(String),
  target_type   LowCardinality(String),
  target_id     String,
  metadata_json String,
  request_id    Nullable(String),
  created_at    DateTime64(3)
) ENGINE = MergeTree
PARTITION BY toYYYYMMDD(created_at)
ORDER BY (created_at, event_type);

-- Distributed leader locks (artifact GC, audit compaction).
CREATE TABLE leader_locks (
  lock_name    String,
  holder_node  String,
  acquired_at  DateTime64(3),
  expires_at   DateTime64(3)
) ENGINE = KeeperMap('/bentoclick/locks')
PRIMARY KEY lock_name;
```

Refresh-token reuse-detection tables from #106 (`altinity.oauth_refresh_consumed_jtis`, `altinity.oauth_refresh_revoked_families`) are re-enabled only if BentoClick re-enters AS territory (v2/v3). v1 leaves token rotation to the external AS.

## 13. Audit, logging, threat model

### 13.1 Control path

Detailed structured events into `audit_events`:

- Login/logout, JWT validation failures.
- Dashboard create/update/delete/validate/preview/publish.
- Share create/revoke/use.
- Runtime artifact registration / disable.
- Admin configuration changes.
- MCP dashboard tool calls.
- MCP `execute_query` calls (control-path entry; the data-path proxy logs the underlying CH request separately, see §13.2).

Control-path entries may include full spec JSON and full SQL text for MCP calls (it's not on the streaming response path).

### 13.2 Data path

Compact metadata per request, no payload capture:

```text
request_id, user_id, clickhouse_username, connection_id,
mode (gating|forward),
http_method, clickhouse_path,
status_code, duration_ms, bytes_in, bytes_out,
error_class (nullable),
active_role (when WithRoles applied)
```

SQL text on the data path is disabled by default. Fingerprint (hash) only if enabled, never the raw query.

### 13.3 Threat model

In scope:

- Anonymous access — rejected (resource server gates everything).
- AS-issued JWT tampering — rejected (signature + audience check).
- XSS in the SPA — short JWT TTLs from the AS, strict CSP, sanitization at one boundary. SPA never sees CH-side credentials (no bypass mode in v0.2).
- Confused-deputy via DCR'd client — addressed by altinity-mcp's consent screen (#107).
- Cluster-secret super-admin risk — addressed by `altinity.mcp_role_metadata` whitelist + `DEFAULT ROLE NONE` + no-direct-grants discipline.
- Refresh-token theft — owned by the external AS; rotation + reuse detection there.
- Share link leakage — revocable (KeeperMap immediate-revoke), optional expiry, optional recipient authentication.
- Stolen cluster_secret from outside BentoClick CIDR — blocked by CH-side `<networks>` per user.
- Compromise of one BentoClick node's process — see §13.4.

Out of scope:

- DoS against BentoClick, AS, or CH.
- External AS compromise.
- Side-channel attacks on the BentoClick host.
- Compromise of the customer's CH RBAC configuration.

### 13.4 Residual risk: on-box compromise

altinity-mcp's resource-server posture moves much of the trust off-box: the AS holds the signing keys, BentoClick only validates. An attacker on a BentoClick node can:

- Replay JWTs they observe in transit (TLS limits this to active in-flight traffic).
- Issue CH requests on behalf of users with currently-valid JWTs.
- Read drafts/specs in process memory.

What they cannot do (that they could with the previous BentoClick-as-AS design):

- Mint tokens for arbitrary users (no signing key on the box).
- Forge identities for users who are not actively authenticated.

Mitigations layered in v0.2:

1. **CH-side `<networks>`** on the impersonating principal — only BentoClick CIDR accepted.
2. **Role-metadata whitelist + DEFAULT ROLE NONE** — even a compromised BentoClick cannot escalate beyond the operator-curated role set for the active user.
3. **Out-of-band audit witness** for cluster_secret impersonations — append-only sink (object-store with object-lock, or a separate Keeper namespace with different ACLs) that BentoClick cannot mutate after writing. Compromise becomes detectable, dwell time bounded.
4. **Short AS-issued JWT TTLs** — the AS configuration controls this; recommended ≤ 1h with rotation.

Aspirational, not v0.2: hardware-attested boot (HSM/CH honors connections only from a measured BentoClick binary). The only structural answer to on-box compromise, deferred.

## 13.5 HA validation chain

BentoClick replicas are stateless per-request. Sticky sessions are not required; any replica can serve any request.

| State | Where | How HA works |
|---|---|---|
| AS public keys | Per-replica memory cache, TTL refresh against AS JWKS | All replicas converge on the AS's published keys |
| User identity | Self-contained in JWT claims | Each request is self-describing |
| Scope/role allowed list | `altinity.mcp_role_metadata` (control-plane CH) | All replicas read same control-plane CH |
| Dashboard versions, audit, artifacts | Replicated MergeTree | CH replication |
| Drafts, shares, published pointer, locks | KeeperMap | Keeper replication; immediate cross-replica visibility |
| Audit witness | External append-only sink | Sink owns durability |

JWE going back to a "random replica" is a non-question in v1: BentoClick does not issue JWEs. The external AS issues signed JWTs, and any replica validates them by public-key crypto against the AS's JWKS. No shared secret on the BentoClick side, no cross-replica coordination, no decryption key to distribute.

The dormant JWE machinery in `pkg/jwe_auth/` would become relevant only if BentoClick re-enters AS territory (v2/v3 per #106/#107). At that point HA requires either:

- Shared JWE key material loaded from deployment config on every replica, rotated by config rollout (canary as decryptor first, then promote to encryptor), or
- JWE keys stored in KeeperMap with `kid`-keyed lookup on decrypt; replicas watch for key updates.

This is out of scope for v0.2 and noted only so the v2/v3 follow-up does not miss it.

## 14. Deployment shape

```text
3x BentoClick nodes (Go binary, evolved altinity-mcp)
1x external authorization server (Auth0 / Authentik / Keycloak)
1x control-plane ClickHouse cluster
1x Keeper ensemble (already part of CH deployment; KeeperMap surfaces it)
0..N customer ClickHouse clusters (data plane)
1x audit-witness sink (object store with object-lock, or separate Keeper)
shared or per-node artifact directory
```

Per-node artifact directory in v0.2; release process installs identical artifacts on every node. `runtime_artifacts.available_on_nodes` lets the router prefer in-sync nodes during rolling deploys. Object-store-backed artifact sync is a later improvement.

Single-node dev deploys collapse to one BentoClick + one CH + one Keeper.

## 15. Migration from altinity-mcp

Existing altinity-mcp deployments upgrade by:

1. Bumping to the BentoClick-branded binary (same Go module tree, new packages enabled).
2. Adding control-plane CH configuration (separate from customer CH connections).
3. Running the BentoClick schema migrations against the control-plane CH (creates the engines above).
4. Optionally enabling dashboard surface via `bentoclick.dashboards.enabled=true` (off by default for pure-MCP deployments).
5. Optionally adding `bentoclick:dashboard:read/write` to advertised scopes.

The existing MCP tool surface, OAuth resource-server config, cluster_secret flow, and role-metadata table are unchanged. A pure-MCP deployment that never enables the dashboard surface should be functionally identical to today's altinity-mcp.

## 16. Implementation milestones

### Milestone 1: Control-plane CH foundation

- `pkg/controlplane`: client, migrations runner.
- Initial migration: all tables in §12.
- Local single-node dev with embedded-ClickHouse helper from the existing test harness.

### Milestone 2: Dashboard storage and sanitize

- `pkg/dashboards`: `sanitize`, `store`, `validate`.
- Drafts CRUD against `EmbeddedRocksDB`.
- Versions append against `MergeTree`.
- Published pointer atomic update against `KeeperMap`.

### Milestone 3: Dashboard HTTP API

- `/api/dashboards/*` endpoints.
- Owner-only visibility enforced via existing JWT identity.

### Milestone 4: Preview and EXPLAIN-based validation

- Optional EXPLAIN-through-WithRoles preflight in `validate`.
- Preview rendering pipeline.

### Milestone 5: Artifact-aware rendering

- `pkg/artifacts`: registry with `available_on_nodes`.
- `/d/{dashboard_id}` route, runtime preference.

### Milestone 6: Sharing

- `pkg/shares`: KeeperMap-backed.
- WebUI share action, revoke, `/s/{share_token}`.
- Pinned-by-default share semantics.

### Milestone 7: Dashboard MCP tools

- `pkg/mcp_dashboard`: list/read/create/update/delete/validate/preview/publish.
- Scope gating: `bentoclick:dashboard:read|write`.

### Milestone 8: Admin API

- Artifact admin, CH connection admin, control-plane CH status, audit browsing.
- Manual artifact GC.

### Milestone 9: Audit witness wiring

- Out-of-band sink for cluster_secret impersonation events.
- Witness write path independent of BentoClick's primary CH credentials.

## 17. Open discussion items

1. **Recipient authentication on share links** — required by default, or allow bearer share links as a connection-level option?
2. **Distributed lock TTL handling** — KeeperMap doesn't auto-expire rows; the holder must heartbeat or a watchdog must reap. Pick one pattern, document.
3. **EXPLAIN-as-validation policy** — always run on publish, only on validate, or per-deployment toggle? EXPLAIN against the user's role surface tells the truth but adds round-trips.
4. **Audit witness sink choice** — object-store with object-lock vs. separate Keeper ensemble. Different recovery and tamper-evidence properties.
5. **`bentoclick:dashboard:*` scope advertising** — off by default per the #107 pattern, or on for backend deployments that exist solely for dashboards?
6. **Artifact distribution** — package install on every node vs. shared volume vs. object-store sync (later milestone).
7. **Dashboard ownership transfer on user removal** — admin-mediated reassignment vs. orphan-with-flag. v0.2 punts; needs a decision before any user-departure scenario.

## 18. References

- altinity-mcp repository: https://github.com/Altinity/altinity-mcp
- PR #110 (merged): "redefine gating mode as pure OAuth resource server" — the v1 OAuth posture BentoClick inherits.
- Issue/PR #106 (closed, deferred to v2/v3): refresh-token reuse detection in CH MergeTree; rejection of KeeperMap for that use case.
- Issue/PR #107 (closed, deferred to v2/v3): DCR consent + role binding via `WithRoles` and `altinity.mcp_role_metadata`.
- PR #105 (merged): H-1 — require_email_verified gate for gating + cluster_secret.
- PR #104 (merged): OAuth spec-compliance hardening + HKDF + C-1.
- Altinity clickhouse-go fork: `feature/interserver-extra-roles` @ `3e22df3` — bumps `DBMS_TCP_PROTOCOL_VERSION` to 54472 for `WithRoles`.
- BentoClick repository: https://github.com/BorisTyshkevich/bentoclick
- ClickHouse external HTTP authentication (not used in v0.2, kept for reference): https://clickhouse.com/docs/operations/external-authenticators/http
- ClickHouse `KeeperMap` engine: in-tree at `src/Storages/StorageKeeperMap.cpp`.
