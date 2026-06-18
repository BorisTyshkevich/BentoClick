# BentoClick

Compartmentalized ClickHouse dashboards. Save a small JSON spec, get a
shareable URL — the SPA fetches the row at view time and renders panels
against ClickHouse as the viewer.

Designed to be authored by agents through MCP: the dashboard table is the
API surface, and the MCP write tool reflects its columns into typed tool
parameters.

## Status

**v0.1.0 released** — see [Releases](https://github.com/BorisTyshkevich/BentoClick/releases).
Early adopters welcome; production users still nil. Schema, runtime, and
authoring skill are stable for `spec_version = 1`. Breaking changes
will bump to `spec_version = 2` and live alongside v1, per
`docs/SPEC_VERSIONING.md`.

## Quick install

Once `make test` is green, run:

```bash
./scripts/install.sh \
  --ch-host=https://<host>:8443 \
  --ch-user=<admin> \
  --ch-password=<pw> \
  --mcp-url=https://<mcp-host> \
  --spa-origin=https://<host> \
  [--db=bentoclick]                  # default; the CH database name
  [--cluster='{cluster}']            # default; CH expands the macro
  [--migrate-from=<old-db>]          # if renaming from a prior install
  [--brand-name=bentoclick]          # browser-tab title
  [--accent='#00d4aa']
```

To ship only an SPA asset change (a JS/CSS/HTML edit) to a running
cluster without re-applying schema, handlers, or samples, use
`scripts/update.sh` instead — see [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md):

```bash
CH_PASSWORD=<pw> ./scripts/update.sh \
  --ch-host=https://<host>:8443 --ch-user=<admin> dash.js
```

The installer:

1. Applies `schema/00-definer.sql`, `schema/01-database.sql`,
   `schema/02-roles.sql` — creates the SQL SECURITY DEFINER materialized
   view, the writer/reader/definer roles, and the `bentoclick.dashboards`
   ReplicatedReplacingMergeTree (cluster-aware via `ON CLUSTER '{cluster}'`).
2. Pushes the SPA runtime (`spa.html`, `spa.js`, `spa-helpers.js`,
   `dash.js`, `charts.js`, `dash-theme.css`, `oauth-callback.html`) and
   the handler XML to every cluster replica's `user_files/` via
   `INSERT INTO FUNCTION clusterAllReplicas(...)` against a per-asset
   File-engine table.
3. Renders `config/config.json` and `config/client.json` from the
   `*.tmpl` templates with the install-time values.
4. Inserts the five sample dashboards (in `samples/`) through the
   sanitizing MV.

The handler XML (`handlers/bentoclick.xml`) ships as a `user_files/`
artifact; wiring it into ClickHouse's `config.d/` chain is operator
work — see `.wiki/operations.md` in the parent `acm/mcp` repo for the
antalya-specific recipe.

## Releases

Tagged commits trigger a GitHub Actions workflow that publishes two
artifacts per release:

| Asset | Use |
|---|---|
| `bentoclick-<version>.tar.gz` | Install distro — `scripts/` (`install.sh`, `update.sh`, `uninstall.sh`, `lib.sh`) + schema + runtime + handlers + config templates + samples. |
| `bentoclick-skill-<version>.zip` | Agent skill — `SKILL.md` + 11 panel reference docs. Upload to a claude.ai personal connector. |

See [releases/tag/v0.1.0](https://github.com/BorisTyshkevich/BentoClick/releases/tag/v0.1.0).

## Running tests

```bash
make test          # full suite — ~2-3 min with Docker pull warm
make test-schema   # pytest only — fast SQL iteration
make test-runtime  # vitest only — fast JS iteration
make coverage      # vitest with HTML report at tests/runtime/coverage/
make clean         # tear down the test container
```

Test stack: ClickHouse 26.3 in Docker with embedded Keeper (so
`ReplicatedReplacingMergeTree` and `ON CLUSTER '{cluster}'` resolve on
a single-node test cluster — see `tests/clickhouse-config.xml`),
`pytest` + `clickhouse-connect` for schema/SQL, `vitest` + `happy-dom`
for the runtime.

**Coverage gates** (per-file, in `tests/vitest.config.ts`):

| File | Statements / Branches / Functions / Lines |
|---|---|
| `runtime/v1/dash.js` | 90 / 85 / 90 / 90 |
| `runtime/v1/charts.js` | 90 / 85 / 90 / 90 |
| `runtime/v1/spa-helpers.js` | 90 / 85 / 90 / 90 |
| `runtime/v1/spa.js` | 0 / 0 / 0 / 0 (in report, ungated — OAuth/DOM bootstrap is e2e-tested) |

## Repo layout

```
bentoclick/
├── .github/workflows/  # tests.yml, release.yml
├── scripts/            # install.sh, update.sh, uninstall.sh, lib.sh (shared)
├── schema/             # 00-definer.sql, 01-database.sql, 02-roles.sql
├── handlers/           # bentoclick.xml (CH HTTP handler config)
├── runtime/v1/         # SPA shell + ES-module runtime for spec_version=1
├── config/             # config.json / client.json templates
├── samples/            # 5 sample spec JSON files
├── skills/bentoclick/     # Claude skill (SKILL.md + panels/*.md)
├── docs/               # SPEC_VERSIONING, SANITIZATION, TESTING
└── tests/              # pytest (schema) + vitest (runtime) + e2e
```

## Authoring dashboards

Install the skill — symlink the source tree (for dev) or unzip the
release asset:

```bash
# dev: track main
ln -s "$(pwd)/skills/bentoclick" \
      ~/.claude/skills/bentoclick

# release: unzip the published artifact
cd ~/.claude/skills && \
  curl -L https://github.com/BorisTyshkevich/BentoClick/releases/download/v0.1.0/bentoclick-skill-0.1.0.zip -O && \
  unzip bentoclick-skill-0.1.0.zip
```

Then ask Claude Code (or any agent attached to the MCP) to build a
dashboard. The skill teaches eleven panel types — `kpi-strip`, `table`,
`bars`, `markdown`, `hero`, `callouts`, `html`, `script`, `line`,
`combo`, `chart` — and routes writes through the reflected MCP
`save_dashboard` tool. Owner is recorded by the SECURITY DEFINER MV
(via `currentUser()` resolved against the session, not the definer)
so cross-user dashboard forgery is structurally blocked.

## License

Apache 2.0 — see `LICENSE`.
