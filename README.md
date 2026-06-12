# anond — anonymized ClickHouse schema discovery (v1 experiment)

Standalone CLI implementing v1 of [DESIGN.md](DESIGN.md): profile a ClickHouse
cluster (schema + workload), build an HMAC-derived identifier map, store a
**tokens-only profile** in a meta database, and materialize an **obfuscated
sandbox** — per-token databases of physical token-named tables holding masked
data — that an untrusted LLM can explore without ever seeing a real
identifier or value.

Not yet integrated with altinity-mcp; this branch is a separate experiment.

## Quick start

```bash
make build
export ANON_HMAC_KEY="$(openssl rand -hex 32)"   # keep as secret as the data

# full pipeline against a clickhouse-client connection
./bin/anond run --connection demo --databases git,eth

# inspect the (tokens-only) profile
./bin/anond print --connection demo --section catalog
./bin/anond print --connection demo --section workload

# acceptance checks: no-survivor + sandbox integrity
./bin/anond verify --connection demo

# drop everything the tool created (and nothing else)
./bin/anond cleanup --connection demo --include-meta
```

## What a run produces

- **Meta DB `altinity`** (override: `--meta-db`):
  - `profile_*` tables — shape, catalog, columns, relations, workload, hot
    columns, normalized query shapes, conventions, verification log. All
    identifiers tokenized (`db_a3f29c.tbl_b41c22`); safe for LLM-facing reads.
  - `identifier_map`, `masking_plan` — **real names, trusted side only**.
  - `generated_objects` — registry of every object the tool created (the only
    things `cleanup` will ever drop).
  - `manifest` — written **last**; presence = complete run.
- **Sandbox**: one `db_<tok>` database per real database, one MergeTree
  `tbl_<tok>` per eligible table (token column names, masked data, bounded
  rows via `--sample-rows`, default 1M). `SHOW CREATE` on these exposes only
  tokens + engine — the masking expressions (value seed + real names) exist
  only in the job's trusted-side `INSERT ... SELECT`.

## Masking classes (fail closed)

| class | rule | transform |
|---|---|---|
| time | Date*/DateTime* | keep |
| measure | other numerics | keep (v1) |
| enum | Enum8/16 | keep |
| joinkey | UUID/IP, id-named, or key columns | deterministic `sipHash64(seed, v)` — joins/GROUP BYs survive |
| label | LowCardinality(String) | short keyed-hash token |
| freetext | other String | `'[redacted]'` |
| schemaless | JSON/Dynamic/complex-with-strings | **excluded** |

Tokens are HMAC-SHA256 derived (`ANON_HMAC_KEY`), so re-runs and replicas
mint identical tokens with zero coordination. The token shape is a reserved
namespace — discovery aborts if a real identifier matches it.

## Safety rules

- The tool **never drops or replaces an object it didn't create**. Every
  created name is registered in `generated_objects` first; a name collision
  with a foreign object aborts the run (covered by an integration test).
- Profile writes are fail-closed: an unobserved identifier is an error, an
  unparseable SQL value is redacted whole, an unknown column type is excluded.

## Tests

```bash
make test                      # unit (no network)
make itest CONNECTION=demo     # integration: pipeline + acceptance A–F on a live cluster
make smoke CONNECTION=demo     # dry-run discovery, no writes
```

Integration tests create `altinity_anontest_<pid>` + sandbox DBs and remove
them afterwards.
