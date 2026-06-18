# system-anon — RETIRED (folded into `anond --model=schema-preserving`)

The standalone Python recipe (`gen.py`/`build.sh`/`grants.sql`/`verify.sh`) is
gone — its schema-preserving masking is now a first-class mode of `anond`, so
one tool produces both the tokenizing and the schema-preserving sandboxes and
both write the shared `bentoclick.schema_guide` registry.

## Build the system sandbox

```bash
cd ../anon
go build -o bin/anond ./cmd/anond
./bin/anond run --model=schema-preserving \
  --source "cl <cluster>" --source-db system --dest "cl <cluster>" --dest-db system_anon \
  --hmac-key-file ~/.sysanon-seed --window-days 30 --sample-rows 1000000
```

This keeps **real** table/column names and masks only **values**; reversible
identifier tokens are minted into `bentosecrets.identifier_map` (detok'd for the
human report); the registry rows land in `bentoclick.schema_guide`
(`model='schema-preserving'`, `naming='real'`).

## Masking contract (schema-preserving)

`anond` classifies each column (generic, type+name rules — see
`anon/internal/classify/preserve.go`); ClickHouse-system-specific keep/redact
policy is **operator input** (`Config.ColumnOverrides`), not baked into anond —
that domain knowledge belongs with the altinity-skills, not bentoclick.

| class | columns | transform | reversible? |
|---|---|---|---|
| keep → `real` | numerics, timestamps, enums, UUID, Map (ProfileEvents/Settings), CH-internal vocab | as-is | n/a |
| tok:`<kind>` → `identifier` | db/table/column/user/host names | `concat('<kind>_', hex(sipHash64(seed, v)))` → minted into `bentosecrets.identifier_map` | **yes** (detok) |
| hash:`<kind>` → `identifier` | query ids, partition values, id-shaped keys | same hash, non-detok prefix (`qid_`/`pt_`) | no (deterministic, joinable) |
| redact → `redacted` | free text / SQL / client IPs (sentinel) | `'[redacted]'` / `toIPv6('::')` | no |

Fail-closed: any unclassified string-bearing column redacts; unhandled complex
types (Tuple/Nested) and nested dotted subcolumns are dropped.

The four sample system-health dashboards remain in `../samples/system/` as
concrete examples (their ClickHouse-specific authoring guidance lives with the
altinity-skills, not here).

The old `dashboards-mv.sql` snapshot (a hand-captured, write-time-detok MV with
hardcoded per-database rewrites and a stale dictionary reference) has been
**deleted** — it is superseded by the generic read-time `detok` UDF
(`<x>_anon → <x>` for any `<x>`, sourced from the `token_to_real` dict). The
dashboards MV is generated entirely from repo source:
`anon/integrations/bentoclick/sql/03-dashboards-anon.sql`, with spec validation
(issue #16) applied to existing installs via
`schema/migrations/0001-mv-spec-validation.sql` (`ALTER TABLE … MODIFY QUERY`,
which preserves the `dashboards_tok` TO target).
