# Anonymized Schema Discovery & Data Exploration — design discussion (v0)

Status: **early design discussion**. No code. This branch is a standalone
experiment, intentionally not connected to altinity-mcp's history — it may
become a fork.

## 1. Problem statement & trust model

An LLM (claude.ai, ChatGPT, any agent) is a powerful query- and
dashboard-author, but it is **not trusted** with customer schema or data.
altinity-mcp, deployed inside Altinity Cloud in front of ClickHouse, **is
trusted**.

Goal: let an untrusted LLM explore a cluster's schema and data shape, and
compose queries / dashboards — **without ever seeing real identifiers or
real data values**. The composed artifacts (queries, dashboards) are then
executed on the trusted side with real names and real data, by:

- Grafana, or
- LLM-oriented dashboard frontends (e.g. `bentoclick`) talking to
  altinity-mcp's OpenAPI handlers directly — no LLM in the data path.

This is **mode (b) — full anonymized mode**: the LLM-facing MCP surface is
exclusively tokenized. A deployment that offers anonymized discovery while
also exposing plain `execute_query` to the same LLM would be theater (the
model would just read `system.tables`). Anonymized mode and raw mode are
mutually exclusive per deployment (config-level switch, like `broker:`).

### The round trip

```
            (untrusted)                          (trusted)
   LLM  ──tools──▶  altinity-mcp  ──SQL──▶  ClickHouse
    ▲                    │
    │   anonymized       │  de-tokenize tbl_a3f29c → real name,
    │   schema profile,  │  execute, route results to the
    │   masked samples   │  dashboard/human — NOT back to the LLM
    ▼                    ▼
  composes SQL      OpenAPI handlers ◀── Grafana / bentoclick
  over tokens       (real names, real data, no LLM)
```

The LLM writes `SELECT col_9f12e3 FROM db_0a1b2c.tbl_a3f29c WHERE …`.
Because tokens are **lexically reserved words** (see §4.3), de-tokenization
on the trusted side is a dumb word-level substitution — no SQL parser
required.

## 2. Architecture overview

Three pillars, all decided in discussion:

1. **Job/worker inside altinity-mcp** (not a tool call). A discovery job
   inspects the cluster, builds the schema profile and the rename map.
2. **ClickHouse as the only storage.** altinity-mcp stays stateless and
   multi-replica. All intermediate metadata, the profile, and the rename
   map live in a dedicated database: **`altinity`**. This extends the
   existing "dynamic tools = SQL views" philosophy: as much of discovery
   as possible is plain ClickHouse SQL (`INSERT INTO altinity.x SELECT …
   FROM system.…`), auditable and hot-fixable; Go only orchestrates,
   parses, and classifies.
3. **HMAC-derived tokens** (not sequential). Computed, never allocated →
   no cross-replica coordination, idempotent re-runs, stable under schema
   drift. The map table in ClickHouse is a *materialization* for trusted
   consumers to JOIN against, not an allocation ledger.

### 2.1 Service identity

Discovery runs as a configured **service user**, not a per-request bearer:

- read on `system.*` (and, for data masking/sampling, read on profiled
  user tables),
- write on the `altinity` database **only** (scoped GRANT, documented in
  helm values).

This is the first write privilege the MCP server holds for itself — today
all writes happen on behalf of users. Deliberate, scoped, visible.

Per-user schema visibility (row policies, grants) does NOT apply to the
map: the map must be global and consistent. The LLM-facing tools may still
filter the *profile* per-bearer later; v0 serves one global profile.

### 2.2 Job lifecycle

- **Triggers**: startup staleness check (schema fingerprint, like the
  profiler skill's regeneration mode) + admin OpenAPI endpoint
  (`POST /admin/discovery/run`). No internal cron in v0.
- **Run protocol** (borrowed from the collector's crash-safe staging):
  every phase writes rows under a `run_id`; a **manifest row is written
  last** — manifest presence = complete run; consumers read only the
  latest complete run. Multi-replica concurrent runs are harmless:
  HMAC tokens are conflict-free, duplicate profile rows dedup by
  `(run_id, …)` keys (ReplacingMergeTree).
- **Multicluster**: discovery is per-cluster (like the catalog cache);
  one `altinity` database per cluster.

### 2.3 LLM-facing vs trusted-facing surfaces

- **LLM-facing: tools only.** MCP resources are unevenly supported
  (ChatGPT connectors are effectively tools-only; claude.ai web treats
  resources as user-attachable context at best; only Claude Desktop and
  some IDEs handle them properly). Resources may be mirrored as a free
  bonus if the SDK makes it a one-liner — never the primary path.
  Profile served by a sectioned/paginated tool family (see §6).
- **Trusted-facing: OpenAPI handlers** (`server_openapi.go` exists today)
  serving the profile, the rename map, de-tokenization of LLM-composed
  SQL, and execution with real results.

## 3. Discovery pipeline (all phases, from the profiler skill)

The `altinity-profiler-clickhouse` skill is the source design. Its
pipeline is ~90% mechanical SQL + arithmetic — the skill itself delegates
arithmetic to Python helpers (`pareto_cut.py`, `synthesize_conventions.py`)
because LLM arithmetic drifts. Everything below is deterministic Go/SQL;
the only LLM-shaped phase (prose synthesis) is *deleted* — in MCP the
client LLM is the synthesizer, and here it synthesizes over tokens.

| Phase | What | Where it runs |
|---|---|---|
| 0 — shape | CH version bucket, query_log raw-vs-preagg, Distributed naming | SQL + Go string parsing |
| 1 — discovery | topology, DB roster, engine mix, qlog span | pure SQL → `altinity.*` |
| 1.5 — archetype | A–E decision rules (first-match-wins table from the skill) | Go (direct port of the rule table) |
| 2 — questionnaire | replaced by job config: db scope, window, service users | config defaults: all non-system DBs, last 7d |
| 3 — catalog | tables/columns/keys/engines | pure SQL → `altinity.*` |
| 4 — relations | dependency graph, dictionaries, `TO`/`FROM`/`JOIN` extraction from DDL | SQL (`extract()`) + Go regex where SQL gets unreadable |
| 5 — mining | hot tables, co-occurrence, hot columns, form ratios (PREWHERE/FINAL/argMax idioms) | pure SQL; `normalizeQuery()` + comment-strip server-side |
| 6 — demotion | Pareto cut, insert-dominated / service-user / staging rules | Go (port of `pareto_cut.py`); review flags emitted as structured output, resolved by consumer |
| 7 — classification | Fact/Dim/Mart/Staging by engine + shape | Go switch (the skill's case statement verbatim) |
| 7.5 — verification | existence (set difference), EXPLAIN parse, probe + comment-token query_log lookup, join-cardinality probes | SQL + Go; tenant auto-pick = top tenant by volume, else claims marked `inferred` |
| 8 — synthesis | structured profile rows + verification log in `altinity` | Go templating; **no prose** — the client LLM narrates if it wants |

Two honest judgment points, both demoted to data:

- **Verification tenant**: heuristic (top tenant by recent volume) with a
  config override; absent → behavior/relationship claims marked
  `inferred`, exactly as the skill specifies.
- **Demotion review flags** (`misleading-staging-name`,
  `shadow-traffic-vs-base`, `per-tenant-hash-pattern`): detected
  mechanically, emitted as flags, never auto-resolved.

The skill remains useful as the *narrative* generator; long-term it could
consume this layer instead of 50 ad-hoc `execute_query` round-trips,
making its identifier hallucination structurally impossible.

## 4. Identifier anonymization

Port of the proven `system-audit/collector` design (pure-stdlib Python →
Go), with three deliberate changes driven by the new trust model.

### 4.1 What carries over unchanged

- **Two-phase observe→map** discipline: the global map is complete before
  any value is rewritten, so the same name maps to the same token in
  structured columns and inside free-text SQL.
- **The lexer-not-parser rewriter** (`sqlrewrite.py`): regex scanner,
  keyword set, keep-registry of the cluster's own function/setting names,
  neighbor-aware `db.tbl.col` kind probing, **fail-closed** on unbalanced
  quotes (whole value → `[redacted]`).
- **Fail-closed type fallback**: unknown string-bearing /
  schemaless-container columns (`String`, `IPv4/6`, `JSON`, `Object`,
  `Dynamic`, `Variant`) drop; value types keep. Survives version drift.
- **Manifest-style residual notes**: every profile self-documents its
  leak surface (numeric literals in non-strict SQL, etc.).

### 4.2 What changes

| Collector | This design | Why |
|---|---|---|
| sequential `tbl_0007` tokens | **HMAC-derived** `tbl_a3f29c` | computed not allocated: multi-replica safe, idempotent, drift-stable |
| `rename_artifact.json` local file | map table in `altinity` DB | trusted instruments JOIN it; never returned to the LLM |
| one-shot offline run | resident job, incremental re-runs | fingerprint staleness; append-only map |
| reversibility via artifact | reversibility via re-computation or map table | both server-side only |

Token derivation: `<kind>_<hex(truncate(HMAC(key, kind || original)))>`,
key derived from `signing_secret` (or a dedicated config key). Truncation
length chosen for collision safety at realistic identifier counts
(~6–8 hex chars; collision check at map-write time, lengthen on hit —
record the decision in the manifest).

### 4.3 Token alphabet = the reverse path

Tokens must be **lexically reserved**: a fixed prefix set
(`db_`, `tbl_`, `col_`, `user_`, `dict_`, `sql_`, …) + hex suffix, a shape
that (a) is a valid unquoted CH identifier, (b) can never collide with a
real identifier (enforced: discovery *fails* the run if a real identifier
matches the token pattern — then quote/extend the pattern), (c) never
collides with SQL keywords or CH function names. Consequence:
de-tokenizing inbound LLM SQL is word-boundary substitution. No parser.

### 4.4 No SQL parser — decided

Three tiers were considered:

1. **Server-side `normalizeQuery()` + Go lexer rewrite** (collector
   approach) — proven on real corpora, fail-closed, version-agnostic.
2. Go AST parser (e.g. AfterShip/clickhouse-sql-parser) — better
   classification, but: perpetual dialect lag, parse failure still needs
   the fail-closed fallback (so both get maintained), and a misparse
   *leaks* while a lexer over-tokenizes (safe failure direction). A
   security-critical third-party dep for negative net value.
3. ClickHouse itself as parser (`EXPLAIN AST` round-trips) — always
   on-version, but the output is a pretty-printed tree, not a stable API.

Decision: **tier 1 permanently**. ClickHouse is still used as the
*validator* for inbound LLM SQL: de-tokenize, then `EXPLAIN` under the
read-only profile — the real parser, on-version, answering the only
questions that matter (parses? read-only? touches only allowed tables?).
`EXPLAIN AST` may serve as a test oracle for lexer coverage.

## 5. Data anonymization (values, not identifiers)

New scope from this discussion. Purpose: the LLM needs to *see data shape*
to build good queries and dashboards — realistic distributions,
cardinalities, example values — without seeing real values. Crucially,
**no reverse map is needed**: nobody ever de-anonymizes a data value; the
dashboard fetches real data on the trusted side.

### 5.1 Requirements

For LLM dashboard-authoring to work, masked data must preserve:

- **types** (a masked `DateTime` is a `DateTime`),
- **joinability** — deterministic per-value transform: the same
  `user_id` masks to the same value everywhere, so the LLM's test JOINs
  and GROUP BYs behave like the real ones,
- **cardinality & distribution shape** — top-N breakdowns, percentiles,
  sparsity must look right,
- **ordering/continuity where it matters** (time columns especially).

Reversibility: explicitly NOT required.

### 5.2 ClickHouse-native building blocks (research)

| Mechanism | What it does | Fit |
|---|---|---|
| **`CREATE MASKING POLICY`** (25.12+; documented under ClickHouse *Cloud* guides — **verify availability in Altinity builds before depending on it**) | native, role-based, query-time column masking; no separate views, no stored copies | the cleanest possible mechanism: bind policies to the anonymized-mode role; LLM-facing queries get masked values transparently |
| **`clickhouse-obfuscator`** (ships with CH) | offline transform preserving cardinalities, conditional cardinalities, int/float distributions, time continuity, string lengths; deterministic from a secret seed | gold standard for a *synthetic sandbox dataset*; file-based (read table → write table), so it fits a job that materializes an obfuscated sample DB; seed must be secret (some transforms are reversible with the seed) |
| **Masked views** | per-column transform expressions (`sipHash64`, `SHA256`, partial-reveal string functions) in a view layer | works on any CH version; our discovery job already classifies every column, so view generation is mechanical |
| **Materialized masked columns / tables** | store masked copies | only needed if query-time masking is too slow for sampling |
| **`query_masking_rules`** (server config, OSS) | regex masking of query text in logs | orthogonal: protects `system.query_log` from leaking literals into our *own* mining phase — worth recommending in deployment docs regardless |
| **Row policies + column grants** | restrict what the service/LLM role can read at all | the outer fence around all of the above |

### 5.3 Proposed layered design

The column classification produced by discovery (§3 phase 3 + the
collector's disposition taxonomy) drives a **per-column masking plan**:

| Column class (from discovery) | Transform |
|---|---|
| numeric measures | keep, or obfuscator-style noise (decide per deployment; keep is often fine) |
| dates / times | keep (continuity is analytically load-bearing) |
| join keys / IDs (string or numeric) | deterministic keyed hash (`sipHash64(key, v)` formatted back to the column's shape) — preserves joins & cardinality |
| low-cardinality labels (enums, statuses, countries) | keep verbatim if vocabulary-like, else deterministic token per distinct value |
| free text (names, emails, URLs, payloads) | redact or length-preserving scramble; never keep |
| schemaless (`JSON`/`Dynamic`/…) | drop (fail closed, as in the collector) |

Delivery to the LLM, two complementary channels:

1. **Masked sample tool**: `sample_table(tbl_token, n)` returns N masked
   rows + per-column stats (distinct counts, min/max, top-K masked
   values). Implemented as generated SELECT with masking expressions —
   or via `MASKING POLICY` if available on the target version.
2. **Obfuscated sandbox (optional, phase 2)**: job materializes
   `altinity_sandbox.<tbl_token>` via obfuscator-style transforms for the
   Pareto-hot tables (bounded rows). The LLM can then *run real SQL*
   against the sandbox — full exploration freedom, zero exposure. This is
   the strongest version of "explore data without trusting the LLM," and
   the natural target for the staged-query loop: compose against sandbox,
   validate, promote to trusted execution.

Open question: whether `CREATE MASKING POLICY` is in OSS/Altinity builds
or Cloud-only. If Cloud-only, the masked-view/generated-SELECT path is the
portable default and policies become an optimization on supported targets.

### 5.4 Honest residuals

- Aggregates can leak: `SELECT max(salary)` over masked-IDs-but-kept
  -measures returns a real number. The per-deployment masking plan must
  decide which numeric columns are "measures, keep" vs "sensitive, noise".
- Cardinality itself is metadata some customers consider sensitive
  (e.g. customer counts). Document; make top-K depth configurable.
- The mining phase reads `system.query_log` which contains raw literals;
  that data stays inside the trusted job and only normalized+tokenized
  shapes reach the profile. `query_masking_rules` recommended in
  deployment docs as defense-in-depth.

## 6. LLM-facing tool surface (sketch)

Replaces `execute_query`/`list_tables` in anonymized mode:

- `get_profile(section, cursor)` — shape, archetype, hot tables, join
  graph, conventions; all identifiers tokenized; paginated.
- `describe_table(tbl_token)` — columns (tokenized), types, keys, masked
  per-column stats.
- `sample_table(tbl_token, n)` — masked rows (§5.3).
- `validate_query(sql)` — de-tokenize server-side, `EXPLAIN` under
  read-only profile, return ok/errors + (maybe) estimated read shape.
  Results of *execution* never return to the LLM.
- `stage_query(sql, title, …)` / `stage_dashboard(spec)` — persist the
  composed artifact (tokenized form) into `altinity` for trusted
  consumers (Grafana / bentoclick / OpenAPI) to fetch, de-tokenize, run.
- (phase 2) `query_sandbox(sql)` — real SQL against the obfuscated
  sandbox DB only.

## 7. Storage sketch (`altinity` database, conceptual)

All ReplacingMergeTree-style, keyed to be idempotent; `run_id` everywhere;
`manifest` written last.

- `identifier_map(kind, original, token, first_seen, run_id)` — trusted
  consumers only; never exposed through any LLM-facing tool.
- `profile_shape`, `profile_catalog`, `profile_columns`,
  `profile_relations`, `profile_workload`, `profile_conventions`,
  `profile_verification` — tokenized; the LLM-facing tools read these.
- `masking_plan(db, table, column, class, transform, run_id)` — drives
  §5; trusted-side.
- `staged_artifacts(id, kind, body_tokenized, created_by, run_id)` —
  LLM-composed queries/dashboards awaiting trusted execution.
- `manifest(run_id, started, finished, stats, residual_notes, …)`.

Exact DDL is deliberately out of scope for v0 discussion.

## 8. Decisions log

1. Trust boundary: MCP trusted, LLM untrusted → **full anonymized mode**;
   raw and anonymized modes mutually exclusive per deployment.
2. Discovery is a **job inside altinity-mcp**; ClickHouse (`altinity` DB)
   is the only storage; SQL-first discovery, Go orchestrates.
3. **HMAC tokens**, kind-prefixed, lexically reserved; map table is
   materialization; reverse path = word substitution, no parser.
4. **No SQL parser**; fail-closed lexer + ClickHouse-as-validator.
5. LLM surface = **tools only**; OpenAPI for trusted instruments;
   resources optional mirror.
6. Data anonymization in scope: classification-driven masking plan,
   masked sampling first, obfuscated sandbox as phase 2; no reverse map
   for values.
7. This branch is a standalone experiment (orphan branch), candidate fork.

## 9. Open questions

1. `CREATE MASKING POLICY` availability on Altinity Stable builds /
   target customer versions — determines whether policies or generated
   masked SELECTs are the default mechanism.
2. Numeric-measure policy: keep vs noise, per deployment? Who decides —
   config, or per-column heuristic with override?
3. Staged-artifact handoff contract with Grafana / bentoclick: pull via
   OpenAPI? push? format of a "dashboard spec"?
4. Per-bearer profile filtering (does user A's LLM see tables user A
   can't read?) — v0 serves a global profile under the service user;
   revisit before any multi-tenant deployment.
5. Sandbox sizing/refresh policy (rows per table, staleness) if/when the
   obfuscated sandbox lands.
6. Does anonymized mode live in altinity-mcp behind config, or does this
   become a separate binary/fork sharing packages? (This branch exists to
   find out.)

## References

- `altinity-skills/altinity-profiler-clickhouse` — the LLM profiler
  pipeline this design mechanizes (SKILL.md, pipeline.md, tools/).
- `system-audit/collector` — the anonymization pipeline this design
  ports (anonymize.py, idmap.py, sqlrewrite.py;
  `.wiki/anonymization-risks.md` for the leak-surface analysis).
- [Data masking in ClickHouse (Cloud guide)](https://clickhouse.com/docs/cloud/guides/data-masking)
- [CREATE MASKING POLICY](https://clickhouse.com/docs/sql-reference/statements/create/masking-policy)
- [Five Methods For Database Obfuscation (ClickHouse blog)](https://clickhouse.com/blog/five-methods-of-database-obfuscation)
- [clickhouse-obfuscator source](https://github.com/ClickHouse/ClickHouse/blob/master/programs/obfuscator/Obfuscator.cpp)
