# anond — anonymized ClickHouse schema discovery (v1.1 experiment)

Standalone CLI implementing v1.1 of [DESIGN.md](DESIGN.md): profile ONE
database on a **source** cluster (schema + workload), build an HMAC-derived
identifier map, and materialize on a **dest** (sandbox) cluster a tokens-only
profile plus an **obfuscated sandbox** — one operator-named database of
physical token-named tables holding masked data — that an untrusted LLM can
explore without ever seeing a real identifier or value. The dest cluster is
the only thing LLM users ever touch; the trusted map never leaves the source.

Not yet integrated with altinity-mcp; this branch is a separate experiment.

## Quick start

```bash
make build
export ANON_HMAC_KEY="$(openssl rand -hex 32)"   # keep as secret as the data

# cross-cluster: discover+mask on otel, sandbox+profile on demo
./bin/anond run --source "cl otel" --source-db claude_otel \
  --dest "clickhouse-client --connection demo" --dest-db claude_otel \
  --hmac-key-file ~/.anon-hmac-key

# single-cluster mode (--connection is sugar for source = dest)
./bin/anond run --connection demo --source-db git

# inspect the (tokens-only) profile — read from the DEST
./bin/anond print --source "cl otel" --dest "clickhouse-client --connection demo" --section catalog
./bin/anond print --connection demo --section workload

# acceptance checks: no-survivor + sandbox integrity + trusted split
./bin/anond verify --source "cl otel" --dest "clickhouse-client --connection demo"

# drop everything the tool created on the dest (and nothing else)
./bin/anond cleanup --source "cl otel" --dest "clickhouse-client --connection demo" --include-meta
```

`--source`/`--dest` take whitespace-split command prefixes that accept
clickhouse-client flags (a wrapper like `cl otel` works). Omitting `--dest`
means dest = source. `run` mirrors exactly one database (`--source-db`,
required); `--dest-db` names the sandbox database on the dest (default: the
source DB name).

## What a run produces

- **Meta DB `altinity`** (override: `--meta-db`; same name on both clusters,
  different tables — the trusted split):
  - on the **source**: `identifier_map`, `masking_plan` — **real names,
    trusted side only**. They never exist on the dest; `verify` hard-fails if
    they do. A grants misconfig on the LLM-exposed cluster can de-anonymize
    nothing.
  - on the **dest**: `profile_*` tables — shape, catalog, columns, relations,
    workload, hot columns, normalized query shapes, conventions, verification
    log. All identifiers tokenized (`db_a3f29c.tbl_b41c22`); safe for
    LLM-facing reads. Plus `generated_objects` (registry of every object the
    tool created on the dest — the only things `cleanup` will ever drop) and
    `manifest` — written **last**; presence = complete run.
- **Sandbox** (dest): one operator-named database (`--dest-db`), one MergeTree
  `tbl_<tok>` per eligible table (token column names, masked data, bounded
  rows via `--sample-rows`, default 1M). `SHOW CREATE` on these exposes only
  tokens + engine — the masking expressions (value seed + real names) exist
  only in SELECTs executed on the source; masked rows stream to the dest as
  TSV.

### DB-name disclosure rule

Naming the dest DB after the source DB (`--dest-db` = `--source-db`, the
default) is an operator decision to disclose that ONE name: the profile keeps
it verbatim so profile and sandbox agree. Choosing a different dest name
keeps the source DB name tokenized. Tables and columns are token-named either
way. The outcome is recorded in `profile_shape` (key `db_disclosure`) and the
manifest.

## Masking classes (fail closed)

| class | rule | transform |
|---|---|---|
| time | Date*/DateTime* | keep |
| measure | other numerics | keep (v1) |
| enum | Enum8/16 | keep |
| joinkey | UUID/IP, id-named, or key columns | deterministic `sipHash64(seed, v)` — joins/GROUP BYs survive |
| label | LowCardinality(String) | short keyed-hash token |
| freetext | other String | `'[redacted]'` |
| attrmap | `Map(String\|LowCardinality(String), String)` | keys kept verbatim (semconv-style vocabulary; custom key names pass through — residual noted); values: numerics/booleans/empties kept, else 12-hex keyed hash |
| schemaless | JSON/Dynamic/other complex-with-strings | **excluded** |

Tokens are HMAC-SHA256 derived (`ANON_HMAC_KEY`), so re-runs and replicas
mint identical tokens with zero coordination. The token shape is a reserved
namespace — discovery aborts if a real identifier matches it.

## Safety rules

- The tool **never drops or replaces an object it didn't create**. Every
  created name is registered in the dest `generated_objects` first; a name
  collision with a foreign object aborts the run (covered by an integration
  test). `cleanup` drops exactly the registry list — and refuses
  `system`/`default`/`INFORMATION_SCHEMA` under any circumstance.
- Profile writes are fail-closed: an unobserved identifier is an error, an
  unparseable SQL value is redacted whole, an unknown column type is excluded.
- Every query anond issues self-tags with `--log_comment anond`; workload
  mining excludes that tag, so the source-side masking SELECTs (which carry
  the seed and token aliases) never pollute the next run's profile.

## Tests

```bash
make test                      # unit (no network)
make itest CONNECTION=demo     # integration: pipeline + acceptance A–F on a live cluster
make smoke CONNECTION=demo     # dry-run discovery, no writes
```

Integration tests create `altinity_anontest_<pid>` + sandbox DBs and remove
them afterwards.
