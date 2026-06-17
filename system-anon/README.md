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
  --source "cl otel" --source-db system --dest "cl otel" --dest-db system_anon \
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
altinity-skills, not here). `dashboards-mv.sql` documents the live generic
`<db>_anon → <db>` de-tok MV rewrite from Phase 1.
