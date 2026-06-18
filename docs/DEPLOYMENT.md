# Deployment

Agent-readable runbook for installing and updating a bentoclick
cluster. The installer is intentionally generic — ACM-specific glue
(cluster lifecycle, OIDC client provisioning, secret rotation) is the
caller's responsibility.

## What `scripts/install.sh` actually does

Steps, in order, all run against the `--ch-host` HTTPS endpoint as
the `--ch-user` admin:

1. **Apply schema** — `schema/00-definer.sql`,
   `schema/01-database.sql`, `schema/02-roles.sql` via the
   per-statement applier. Every DDL is `IF NOT EXISTS` /
   `CREATE OR REPLACE`, so re-runs are no-ops.
2. **Optional `--migrate-from=<old-db>`** — copies rows from
   `<old-db>.dashboards` into `${DB}.dashboards`. **Only pass once.**
   Re-passing duplicates rows; `ReplacingMergeTree` de-dupes them
   eventually but it's cosmetically odd in `system.parts` until then.
3. **Push `runtime/v1/*`** to `user_files/dash/<file>` on every
   replica via `INSERT INTO FUNCTION clusterAllReplicas(...)`. Each
   asset has a per-cluster `_asset_<safe-name>` File-engine table
   created `ON CLUSTER … IF NOT EXISTS`; the INSERT uses
   `engine_file_truncate_on_insert = 1` so re-deploys overwrite.
   Assets: `spa.html`, `spa.js`, `spa-helpers.js`, `dash.js`,
   `charts.js`, `dash-theme.css`, `tweaks.js`, `oauth-callback.html`.

   **`dash.js` and `charts.js` are bundles.** Source lives split
   across `runtime/v1/{core,panels,charts}/` for readability, but
   the iframe boot fetches a single `/lib/v1/dash.js` and
   `/lib/v1/charts.js`. The deploy concatenates each tree in
   topological order and uploads the result. The concat order is
   fixed in `scripts/lib.sh` (`DASH_BUNDLE` / `CHARTS_BUNDLE`),
   shared by `install.sh` and `update.sh`; if you add a new file
   under `core/`, `panels/`, or `charts/`, thread it into the
   matching array. The bundle test at
   `tests/runtime/unit/module-to-classic.test.js` mirrors the same
   file list and will fail if they drift apart.
4. **Push `handlers/bentoclick.xml`** to `user_files/dash/bentoclick.xml`
   with `${MCP_ORIGIN}` substituted (scheme+authority of `--mcp-url`,
   e.g. `https://mcp.example.com`). See [Templating](#templating)
   below. **This is a reference copy only** — CH does not read HTTP
   handler config from `user_files`. The config.d registration is the
   caller's responsibility; see
   [Config.d registration (ACM)](#configd-registration-acm) below.
5. **Render `config/*.json.tmpl`** with the install-time args
   (`${CH_URL}`, `${MCP_URL}`, `${SPA_ORIGIN}`, `${DB}`,
   `${BRAND_NAME}`, `${ACCENT}`) and upload as
   `dash/config.json` and `dash/client.json`.
6. **Insert sample dashboards** from `samples/*.spec.json` into
   `${DB}.dashboards_raw`. The SECURITY DEFINER materialized view
   sets `owner = currentUser()`, which means the **installer admin**
   becomes the owner — users won't see these samples in `/app`
   unless they log in as the admin. Re-running refreshes
   `updated_at`; it doesn't touch user-saved dashboards because
   they have a different `(owner, slug)` key.

## Config.d registration

`scripts/install.sh` does not touch cluster config.d — that is the caller's
responsibility. `handlers/bentoclick.xml` must be placed (or
registered) as a config.d fragment before `/app` will respond.

**`name=` attribute requirement:** every `<rule>` in `bentoclick.xml`
carries a unique `name=` slug. This is required whenever multiple
config.d files each define `<http_handlers>`. Without it, CH merges
sibling `<rule>` elements positionally across files, and later files
silently overwrite earlier files' rules slot-for-slot (upstream:
[ClickHouse#70636](https://github.com/ClickHouse/ClickHouse/issues/70636),
reproduced on 26.1.x).

**`<defaults/>` ordering:** `bentoclick.xml` includes `<defaults/>`
at the end of its handler list. If the caller adds other config.d
files that also define `<http_handlers>`, `<defaults/>` must remain
in the alphabetically last file — the built-in `/play` handler added
by `<defaults/>` matches by prefix and shadows any custom
`regex:^/play$` rule that appears after it in the merged list.

To apply (substitute real MCP origin before registering):

```bash
MCP_ORIGIN="https://mcp.example.com"
sed "s|\${MCP_ORIGIN}|$MCP_ORIGIN|g" handlers/bentoclick.xml \
  > /etc/clickhouse-server/config.d/bentoclick.xml
# then reload / restart ClickHouse
```

## Templating

Three placeholders are substituted by `scripts/install.sh` at deploy time:

| Placeholder | Source | Used in |
|---|---|---|
| `${MCP_ORIGIN}` | scheme+authority of `--mcp-url` | `handlers/bentoclick.xml` CSP `connect-src` |
| `${MCP_URL}`, `${CH_URL}`, `${SPA_ORIGIN}`, `${DB}`, `${BRAND_NAME}`, `${ACCENT}` | install-time args | `config/config.json.tmpl` |
| `${SPA_ORIGIN}`, `${BRAND_NAME}` | install-time args | `config/client.json.tmpl` |

**If you see a literal `${MCP_ORIGIN}` in the deployed handler XML,
the substitution was skipped — re-run `scripts/install.sh`.** Browsers parse
the literal as a malformed CSP source and silently drop it, leaving
`connect-src 'self'`. That only works when MCP and SPA share an
origin; if they're on different origins, the SPA's RFC 9728
discovery fetch is blocked and OAuth login breaks with no visible
error in the UI (look for a CSP violation in DevTools).

## First-time install

```bash
./scripts/install.sh \
  --ch-host=https://<host>:<port>        \
  --ch-user=<admin>                      \
  --ch-password=<pw>                     \
  --mcp-url=https://<mcp-host>           \
  --spa-origin=https://<spa-host>        \
  [--db=bentoclick]                      \
  [--cluster='{cluster}']                \
  [--brand-name=bentoclick]              \
  [--accent='#00d4aa']
```

Verification (from any host that can hit the cluster's HTTPS
endpoint):

```bash
SPA=https://<spa-host>
curl -fsSI "$SPA/app"               | head -1   # expect 200
curl -fsS  "$SPA/lib/v1/dash.js"    | head -3   # expect JS, "// bentoclick runtime v1"
curl -fsS  "$SPA/lib/v1/tweaks.js"  | head -3   # expect JS, tweaks panel module
curl -fsS  "$SPA/config.json" | jq '.mcp_url, .ch_url'  # expect the install-time values
curl -fsS  "$SPA/app" -D - -o /dev/null | grep -i content-security-policy
# Confirm the CSP contains the literal MCP origin (e.g. https://mcp.example.com),
# NOT the placeholder ${MCP_ORIGIN}.
```

## Updating SPA assets only (`scripts/update.sh`)

The common case — shipping a JS/CSS/HTML change to the SPA — does **not**
need a full `install.sh` re-run (which would re-apply schema, re-substitute
the handler CSP, and re-insert samples). `scripts/update.sh` pushes only the
named SPA assets to `user_files/dash/`, reusing the same bundling +
`clusterAllReplicas` fan-out as `install.sh` (both share `scripts/lib.sh`).

```bash
# Push just the dash.js bundle (rebundled from runtime/v1/* on the fly):
CH_PASSWORD=<pw> ./scripts/update.sh \
  --ch-host=https://<host>:<port> --ch-user=<admin> dash.js

# Several assets at once:
CH_PASSWORD=<pw> ./scripts/update.sh \
  --ch-host=https://<host>:<port> --ch-user=<admin> dash.js charts.js dash-theme.css

# No asset args → push the full SPA set (both bundles + verbatim files).
CH_PASSWORD=<pw> ./scripts/update.sh \
  --ch-host=https://<host>:<port> --ch-user=<admin>
```

Asset names are the served basenames under `/lib/v1/`: `dash.js`, `charts.js`
(bundled), and `spa.html`, `spa.js`, `spa-helpers.js`, `tweaks.js`,
`dash-theme.css`, `oauth-callback.html` (verbatim). It does **not** touch
schema, roles, handlers, config templates, or samples. Pass the password via
the `CH_PASSWORD` env var rather than `--ch-password` so it never lands on the
process argv. Asset rotation is non-atomic across replicas — see the window
note below.

**kubectl-only clusters (`--exec`).** When admin access is via `kubectl exec`
(a `clickhouse-client` wrapper) rather than a reachable admin HTTPS endpoint,
pass `--exec=<wrapper>` instead of `--ch-host`/`--ch-user`. The upload SQL is
piped to `<wrapper> --multiquery`; the wrapper owns auth, so no `CH_PASSWORD`
is needed. Example (the Altinity demo `cl` wrapper, which `kubectl exec`s into
the CHI pod):

```bash
./scripts/update.sh --exec='cl otel' dash.js
```

The `clusterAllReplicas` fan-out still pushes to every replica.

## Full re-deploy (update existing cluster)

A re-run of `install.sh` upgrades runtime assets, handler XML, and config
templates in place. **It is safe to re-run with the same args** — every step
except `--migrate-from` is idempotent. Use this when schema, handlers, or
config also need to change; for an asset-only change prefer `update.sh` above.

```bash
./scripts/install.sh \
  --ch-host=https://<host>:<port>  \
  --ch-user=<admin>                \
  --ch-password=<pw>               \
  --mcp-url=https://<mcp-host>     \
  --spa-origin=https://<spa-host>  \
  --db=<same-db-as-before>
# DO NOT pass --migrate-from on a re-run.
```

### What changes on a re-run

- Runtime + handler assets get the new bytes (truncate-on-insert per
  replica).
- `config.json` / `client.json` re-rendered with current args.
- Samples re-inserted under the installer admin's identity — new
  `updated_at`, same content.

### What does *not* change

- User-saved dashboards (different `(owner, slug)` from samples).
- OAuth state in browsers (lives in `localStorage`).
- Already-issued bearers (still valid until the AS-set TTL).
- Schema objects (`CREATE IF NOT EXISTS`).

### Asset rotation window

`INSERT INTO FUNCTION clusterAllReplicas` fans bytes to each replica
sequentially, **not atomically across replicas**. For ~1-2s during
the push, a browser behind a non-sticky LB can fetch replica A's
new `dash.js` and replica B's old `dash-theme.css`. The
`Cache-Control: no-store` (spa.js, spa-helpers.js, tweaks.js,
config.json) and `no-cache` (dash.js, charts.js, dash-theme.css)
headers mean the next refresh resolves it. Prefer a low-traffic
window if avoidable; not a hard requirement.

## Verifying a deploy made the change you expect

After re-deploy, check that the assets actually rotated:

```bash
SPA=https://<spa-host>
# Pull the live runtime and diff its first line against the repo.
curl -fsS "$SPA/lib/v1/dash.js" | head -1
head -1 runtime/v1/dash.js
# Identical → asset rotated. Mismatch → check replica fan-out,
# clear browser cache, re-fetch with `?_=$(date +%s)` cache-bust.

# Check CSP for substituted MCP origin
curl -fsSI "$SPA/app" | grep -i content-security-policy | grep -E 'connect-src[^;]+'
# Should contain the actual MCP host, not the literal `${MCP_ORIGIN}`.
```

## Rollback

There is no built-in rollback. Options:

1. **`git checkout <prev-tag> && ./scripts/install.sh ...`** — re-runs the
   installer with the previous tag's runtime + handlers. User
   dashboards are untouched. (To roll back assets only, `git checkout`
   the prior runtime and `./scripts/update.sh … dash.js charts.js`.)
2. **`./scripts/uninstall.sh ...`** — drops the entire `${DB}` schema +
   user-files assets. Destroys all user-saved dashboards. **Only
   use for a clean-slate redeploy.**

## Troubleshooting

| Symptom | Likely cause | Check |
|---|---|---|
| `/app` loads, dashboards show "renderSpec error" | Stale `dash.js` cached by browser | Hard reload. If persists, check `curl $SPA/lib/v1/dash.js` matches `runtime/v1/dash.js` |
| OAuth login redirects then fails silently | CSP blocking MCP discovery | `curl -I $SPA/app` and look for literal `${MCP_ORIGIN}` in CSP. Re-run `scripts/install.sh` |
| `/v/<owner>/<slug>` 404 | Handler XML overwritten by another `config.d` fragment | `bentoclick.xml` carries `name=` on every `<rule>` (fix for [GH#70636](https://github.com/ClickHouse/ClickHouse/issues/70636)). If other config.d files in the deployment lack `name=`, their rules can overwrite bentoclick's. Verify: `grep 'name="bentoclick-' /var/lib/clickhouse/preprocessed_configs/config.xml` — all 10 bentoclick rule slugs should appear. |
| User sees old dashboards listed in `/app` | `/app` index queries `dashboards FINAL WHERE owner = currentUser()` — they're saved as a different OAuth identity | Check `currentUser()` in CH for that bearer matches the row's `owner` |
| Sample dashboards missing | Samples are owned by the installer admin, not the viewer | Log in as admin to see them, or copy spec into user's account via the `save_dashboard` MCP tool |
