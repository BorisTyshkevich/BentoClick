# otel — anonymized-LLM dashboards over ClickHouse

> **Status: LIVE & e2e-verified.** Single cluster (otel), native Antalya
> `token_processors` (26.3, no sidecar). The full rendered design — diagrams,
> rationale, deployment config, verification — is in
> **[`otel-anonymization.html`](../otel-anonymization.html)** (the canonical doc).
> This page is the quick operational reference.

Companion: [bentoclick-spa.md](bentoclick-spa.md) (the **antalya** bentoclick, which
uses **broker** mode — a different topology), [cimd.md](cimd.md),
[bootstrap-new-mcp.md](bootstrap-new-mcp.md).

## The model in one paragraph

An untrusted LLM browses a **masked sandbox** (`claude_otel_anon`, tokenized
names + masked values, built by **anond**) and authors dashboard SQL; a human
opens the same dashboard and the **bentoclick** SPA runs it against real
`claude_otel`. Both channels share **one audience**
(`https://otel-mcp.demo.altinity.cloud/`), **one CH token directory**, and **one
identical JWT claim** (real email + the full role set). The split is per-request:
the **MCP activates only the `^anon_` roles** for the LLM (`oauth.role_filter`,
fail-closed); **bentoclick talks to CH directly** and uses the full default grant.

## Channels

| Channel | OAuth client | CH identity | Active roles | Data |
|---|---|---|---|---|
| **Human** — bentoclick SPA (browser → CH direct) | static first-party | real `<email>` | full grant (default) | **real** `claude_otel` |
| **LLM** — claude.ai / Code / ChatGPT (→ MCP → CH) | CIMD `tpc_*` third-party | real `<email>` | only `anon_*` (MCP `role=`) | **tokenized** `claude_otel_anon` |

## Key facts

- **Auth0 Action `otel-bentoclick-claims`** (`fd911023…`) mints the SAME claim on
  every channel: real email + `https://clickhouse/roles =
  [oauth_otel_role, bentoclick_viewer_role, anon_author_role, anon_mcp_reader]`.
  No client-type branching, no username rewrite. (Identical claim ⇒ the CH
  token-directory entity never flips ⇒ no role-thrash race.)
- **MCP** (`deploy/otel/mcp-values.yaml`, `broker:false`): `role_claim:
  https://clickhouse/roles`, `role_filter: "^anon_"`, default db
  `claude_otel_anon`, `protocol: http`, and `clickhouse.extra_settings.async_insert:
  "0"` (required for owner-stamping — see below).
- **MCP tools** (no generic `write_query`): `execute_query` (read), `describe_schema`
  (view `altinity.schema_guide` — tokenized schema map + class contract),
  `describe_attributes` (view `altinity.attr_guide` — per Map-key role + usage),
  `get_dashboards_prefix` (view `bentoclick.dashboards_prefix` — correct
  `…/b/v/<email>/` share URL), `save_dashboard` (insert → `dashboards_raw`). Tool
  names match the `bentoclick` skill; backing views must be SELECT-able by
  `anon_*` (granted `dashboards_prefix` to `anon_author_role`; `attr_guide` +
  `profile_attr_keys` to `anon_mcp_reader`).
- **De-anon read path:** `dashboards_mv` (DEFINER `bentoclick_definer`) wraps panels
  with `detok()` → stored `bentoclick.dashboards` SQL is real-name; the `/b/` SPA
  queries real `claude_otel`. Needs `GRANT dictGet ON altinity.token_to_real TO
  bentoclick_definer`. **detok rewrites NAMES not VALUES**: GROUP BY any attrmap key
  de-anonymizes for free; only FILTER values must be real in the sandbox.
- **attrmap per-key roles (auto):** anond classifies each Map key — PII denylist first
  (`organization`, `user.*`, `*.id`, …) → identity (masked); numeric → measure (kept);
  low-card → vocabulary (kept real, filterable); else → sensitive (masked). Emitted to
  `altinity.profile_attr_keys` → `describe_attributes`. Flags: `--attr-card-threshold`
  (64), `--pii-key-pattern`, `--keep-attr-keys` (manual override). "By user" must GROUP
  BY `user.email` (`user.id` is a source SHA256, not reversible).
- **CH** token directory `roles_filter
  ^(oauth_otel_role|bentoclick_viewer_role|anon_author_role|anon_mcp_reader)$`,
  `username_claim https://mcp.altinity.cloud/email`, users provisioned with
  `storage=token`, `default_roles_all=1`.
- **Owner-stamping is free**: `currentUser()` = real email on every channel, so
  the DEFINER MV `dashboards_mv … currentUser() AS owner` stamps the human's email
  on LLM-authored dashboards — **but only with `async_insert=0`**. Under
  `async_insert=1` (otel's default) the background flush has an empty `ClientInfo`
  and `currentUser()` evaluates blank. Upstream:
  [ClickHouse#107530](https://github.com/ClickHouse/ClickHouse/issues/107530).

## Operate

- **Add a human:** Auth0 → Organizations → `demo` → add member → assign
  `oauth_otel_role` + `bentoclick_viewer_role`. Token directory provisions on first login.
- **Add an MCP client:** connect it to
  `https://otel-mcp.demo.altinity.cloud/mcp/otel`; CIMD registers a `tpc_*` client,
  anon automatically.
- **After any CH auth change:** `kubectl rollout restart
  deploy/otel-mcp-altinity-mcp -n demo` (clears the per-pod auth cache).

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| claude.ai `mcp_registration_failed` on Connect | audience changed from what it's CIMD-registered for | set audience back to the MCP-host value |
| `497 ACCESS_DENIED` for the LLM on `claude_otel` | **correct** — LLM active roles are anon only | (expected) |
| LLM-saved dashboard has empty `owner` | `async_insert=1` on the save path | set `clickhouse.extra_settings.async_insert: "0"`, redeploy |
| human gets no data | not an org member, or no roles assigned | add member + assign roles in Auth0 UI |
| CH config didn't change | ACM `/push` not run / pod not reconciled | push cluster 9572, wait for pod ready |
