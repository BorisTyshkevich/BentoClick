# bentoclick — agent contributor guide

You are working on `bentoclick`, a small ClickHouse-backed dashboard
system. Spec is stored as typed columns in `bentoclick.dashboards_raw`
(Null engine), sanitized through a `SQL SECURITY DEFINER` materialized
view into `bentoclick.dashboards` (read target). The MV runs as the
`bentoclick_definer` user — the only principal with INSERT on the
read target. The SPA reads rows and renders panels at view time,
executing user-supplied SQL as the viewer's ClickHouse identity.

## Hard rules

1. **No antalya (or any shared cluster) writes until `make test` is
   green.** The test gate exists precisely to keep half-finished
   schema or runtime changes off shared infrastructure. If a test is
   flaky, fix the flake — do not skip the gate.

2. **Coverage threshold is 90%** across statements, branches,
   functions, and lines. New panel types or runtime branches ship
   with their tests in the same PR. Coverage is enforced by
   `vitest.config.ts` and CI; do not lower the threshold to make a
   PR pass.

3. **All writes go through `dashboards_raw`, never directly to
   `dashboards`.** The writer role only has INSERT on the raw
   table; an explicit REVOKE on `bentoclick.dashboards` blocks
   direct mutation; the SECURITY DEFINER MV (running as
   `bentoclick_definer`) is the only sanctioned path that puts
   rows in `bentoclick.dashboards`.

4. **Don't commit secrets.** No bearer tokens, no client secrets, no
   `config.json` with real origins baked in. `config/*.json` are
   `.gitignore`d; only `*.json.tmpl` files are checked in.

5. **Spec contract is versioned by `spec_version`.** Adding a panel
   type or changing a renderer's contract is a breaking change —
   either keep v1 semantics intact and bump to v2, or add the
   feature additively (new optional field) within v1.

## Repo map

| Path | What's there |
|---|---|
| `schema/` | `01-database.sql` (tables, MV), `02-roles.sql` (grants) |
| `handlers/` | ClickHouse HTTP handler XML, spliced by `install.sh` |
| `runtime/v1/` | SPA shell + `dash.js` for `spec_version = 1` |
| `config/*.json.tmpl` | `envsubst` templates rendered by `install.sh` |
| `samples/` | Spec JSON installed by `install.sh` as starter dashboards |
| `skills/bentoclick/` | Generic guide-driven skill: discover → explore → author → save (incl. tokenized masked sandboxes) |
| `anon/` | Go module (`anond`): the tokenized-masked-sandbox pipeline + the bentoclick integration SQL (`anon/integrations/bentoclick/sql/`). Unit-tested (`make go-test`, 80% floor); live no-leak suite under `internal/integration/` |
| `tests/schema/` | pytest + clickhouse-connect against CH 26.3 |
| `tests/runtime/` | vitest + happy-dom unit tests, 90% coverage gate |
| `tests/e2e/` | Full spec → DOM integration tests |
| `docs/` | `SPEC_VERSIONING`, `SANITIZATION`, `TESTING`, `AUTHORING` |

## How to test

```bash
make test          # full suite: go-test + schema (pytest) + runtime (vitest)
make go-test       # anon/ Go: vet + build + unit tests, GO_COVER_MIN=80% floor
make test-schema   # pytest only
make test-runtime  # vitest only
make coverage      # vitest with HTML report
make clean         # tear down test container
```

`make test` runs `go-test` first (fast, no Docker): the security-critical
anon/ pipeline (masking, leak guard, de-tok) ships with its unit suite and a
coverage floor, same gate as the schema/runtime suites. The live no-leak /
trusted-split / detok-bijection invariants run against a real ClickHouse with
NO Docker — `anon/internal/integration` boots ClickHouse as a host subprocess
via `franchb/embedded-clickhouse` (TestMain), reusing a `clickhouse` binary on
PATH. Locally: `go test ./internal/integration/...` from `anon/` (needs
`clickhouse` installed); in CI via the `go-integration` job. The cross-cluster
cases still take `ANON_TEST_SOURCE_CMD`/`ANON_TEST_DEST_CONNECTION`.

Schema tests spin a ClickHouse 26.3 container via
`tests/docker-compose.test.yml` on ports 18123 (HTTP) and 19000
(native). Each pytest gets a fresh `test_<uuid>` database via the
fixture in `tests/conftest.py`.

Runtime tests run in `happy-dom`. Each panel renderer is exported as
a pure function (`render(panel, rows) -> HTMLElement`) and tested in
isolation. The fetch path is mocked via the `DASH.fetch` seam.

## How to add a panel type

1. Add the renderer to `runtime/v1/dash.js` as an exported pure
   function in the panel dispatcher.
2. Add `tests/runtime/unit/panels.<name>.test.js` covering the happy
   path, empty input, formatter coverage, and error/crash isolation.
3. If the panel introduces a new column behavior (e.g. accepts HTML),
   extend `sanitize_panel` in `schema/01-database.sql` *and* add a
   case to `tests/schema/test_sanitize_panel.py`.
4. Document it in `skills/bentoclick/panels/<name>.md`.
5. Add a fixture and assertion to `tests/e2e/spec-render.test.js`.

## What stays out

- No client-side state persistence (`localStorage`, cookies, `parent.*`)
  — the iframe sandbox blocks it and the SPA assumes statelessness.
- No third-party CDNs in dashboard content without an explicit CSP
  allowlist update — coordinate with whoever owns the install's
  config.
- No "temporary" feature flags or backwards-compat shims for the
  legacy spec-blob shape. v1 is the only shape; old dash code from
  the mcp repo does not migrate.
