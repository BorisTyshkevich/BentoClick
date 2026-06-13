# anond × bentoclick — anonymized dashboard authoring

Design + ClickHouse objects that let an **untrusted LLM author bentoclick
dashboards over anond tokens**, while a **human views the same dashboards
rendered against real data**. The LLM never sees a real identifier or real
value; the human never sees a token.

This lives on the `experiment/anon-discovery` branch; the upstream
[bentoclick](https://github.com/BorisTyshkevich/bentoclick) clone is left
untouched. These are the objects an operator applies to the **otel** cluster
(where bentoclick is deployed and real data lives) on top of a stock
bentoclick install.

## How bentoclick works (the part that matters)

A dashboard is a JSON spec stored as a ClickHouse row. The SPA (served from
CH `user_files/`) fetches the row and runs **each panel's `query` — raw
ClickHouse SQL — directly against CH as the viewer** (OAuth bearer → JWT →
`currentUser()`); there is no app server and no LLM in the data path at view
time. Specs are authored through `altinity-mcp`'s dynamic-tool reflection:
the all-`String` `dashboards_raw` table becomes a typed `save_dashboard`
tool. A `SQL SECURITY DEFINER` MV sanitizes HTML and stamps
`owner = currentUser()` on the way to the read target.

So bentoclick **is** the "trusted dashboard instrument" in anond's
`DESIGN.md` §1 round-trip: the LLM composes SQL over tokens; the dashboard
runs real SQL for the human, no LLM in the data path.

## Trust model and the de-tok point

- The LLM works **only** with anonymized data: it explores anond's
  tokens-only profile + masked sandbox (on the demo/sandbox cluster) and
  composes panel SQL using token identifiers
  (`SELECT col_5ab2d3a7 FROM claude_otel.tbl_5f5c0ed2 …`). Tokens are global
  and HMAC-deterministic, so the same token names the same real object on
  every cluster.
- Authoring is a **loop** — read existing spec, modify, save again — so the
  stored authoring artifact must stay tokenized forever. De-tok at save time
  would leak real names into the LLM's next read. Therefore de-tokenization
  happens at **view time**, between "SPA fetches the spec" and "CH executes
  it", via a view + a dictionary.

### Storage split (one row, two lenses)

```
                          altinity-mcp (anonymized mode)
  LLM  ── save_dashboard ──►  ${DB}.dashboards_raw  (Null, String, token JSON)
   │                                  │  dashboards_mv  (DEFINER: sanitize + owner)
   │  read-back (edit loop, tokens)   ▼
   └────────────────────────  ${DB}.dashboards_tok  (RRMT, typed, TOKENS)
                                      │
                                      │  dashboards  (DEFINER VIEW: detok(), inner FINAL)
                                      ▼
  Human (browser, OAuth) ─────►  ${DB}.dashboards     (de-tokenized REAL SQL)
                                      │  SPA runs panel SQL as the viewer
                                      ▼
                                 real otel data ► rendered dashboard
```

The SPA's query is **unchanged** (`FROM ${DB}.dashboards FINAL …`):
`dashboards` flips from a table to a de-tokenizing view. The LLM's surface is
`dashboards_tok` (tokens) for read-back + `dashboards_raw` for writes. Same
underlying spec, two lenses.

This is **not** "de-tok at save" (variant A): de-tok is read-time over the
tokenized store, never written back. The tokenized artifact is permanent, so
the loop never sees real names, and the dictionary stays authoritative
(re-runs only add tokens, never remap — a rendered dashboard never goes
stale).

## The ClickHouse objects

Apply on otel, in order, on top of a stock bentoclick install:

| File | Object | Purpose |
|---|---|---|
| `sql/01-token-dict.sql` | `${META_DB}.token_to_real` dictionary | reverse map `token → original`, sourced from anond's `${META_DB}.identifier_map`; `COMPLEX_KEY_HASHED`, monotonic `LIFETIME` reload |
| `sql/02-detok-udf.sql` | `detok(s)` UDF | word-substitution expand of every token in a text blob via the dictionary |
| `sql/03-dashboards-anon.sql` | `dashboards_tok` table, re-pointed `dashboards_mv`, `dashboards` de-tok view, grants | the storage-split rewiring |

`${DB}` = dashboard database (e.g. `bentoclick`), `${META_DB}` = anond meta
DB holding the map (`altinity`).

### detok — word-substitution, not parsing

The reserved token namespace (`<kind>_<hex>`, run aborts on any real-name
collision) is what lets de-tok be a textual replace. `detok` does
`extractAll` of the token occurrences → `arrayDistinct` → `arrayFold` of
`replaceRegexpAll` with `\b` boundaries → `dictGetOrDefault(..., tok)`.

Verified on CH 26.3:
- `… col_5ab2d3a7 FROM claude_otel.tbl_5f5c0ed2 WHERE col_5ab2d3a7 > {{n}}`
  → `… ServiceName FROM claude_otel.otel_logs WHERE ServiceName > {{n}}`
  (tokens expanded, `{{n}}` runtime param preserved, the operator-disclosed
  `claude_otel` db kept verbatim).
- An unknown token passes through unchanged → CH errors loudly at query time
  rather than corrupting silently.
- `\b` stops an 8-hex token expanding inside a 16-hex (collision-widened)
  one.
- A DEFINER view with **inner** FINAL dedups correctly while the SPA's
  unchanged outer `FROM dashboards FINAL` is a harmless no-op.

## Security (grant hygiene is the whole argument)

The de-tok view is `SQL SECURITY DEFINER`, so:

- only `${DB}_definer` holds `dictGet` on `token_to_real` (and the dictionary
  data is the de-anonymization secret) — **viewers and the LLM role never
  resolve tokens directly**;
- a DEFINER view's `SHOW CREATE` exposes only `detok(...)`/`dictGet(...)`, not
  real names (those live in dictionary *data*) — consistent with the
  verified CH 26.3 DEFINER-view-body leak finding;
- **LLM authoring role**: `INSERT` on `dashboards_raw`, `SELECT` on
  `dashboards_tok` (tokens) — and explicitly NOT `dashboards` (the view),
  `token_to_real`, or `identifier_map`;
- **SPA / viewer role**: `SELECT` on `dashboards` (the de-tok view) only; the
  panel SQL then runs as the viewer's own CH identity, so real-data access is
  still gated by their grants (a de-tokenized query to a table they can't
  read fails with an auth error — correct).

The disclosed-DB rule from anond carries over: when the sandbox DB is named
after the source DB (`--dest-db = --source-db`, e.g. both `claude_otel`), the
DB name is operator-disclosed and `KeepVerbatim`'d, so it appears in both the
token spec and the de-tok output identically.

## What feeds the dictionary

`token_to_real` reads `${META_DB}.identifier_map`, which a cross-cluster
anond run lands on the source side (otel) — already in place from
`anond run --source "cl otel" --source-db claude_otel --dest "… demo" …`.
In production, whatever keeps `identifier_map` fresh (the anond CLI now; the
deferred anond-in-MCP job later) is orthogonal to this integration; the
dictionary's `LIFETIME` picks up additions automatically.

## Open items / not yet done

- **The MV body in `03-…sql` is reproduced from bentoclick v0.1.0.** Re-point
  must match the *installed* version's `dashboards_mv` SELECT (only the `TO`
  target changes). Diff before applying to a newer install.
- **Dictionary source auth**: the `CLICKHOUSE(QUERY …)` source reads
  `identifier_map` with the server's internal credentials; confirm that
  principal can `SELECT` it on the target cluster.
- **The LLM authoring transport** (anonymized-mode altinity-mcp that exposes
  only the tokenized tier + the anond sandbox/profile, and refuses otel's
  real tables) is the deferred anond-MCP work; until then specs can be
  hand-authored as token JSON into `dashboards_raw`.
- **Not yet applied to otel** — these objects are reviewed and unit-verified
  on demo (26.3) but not deployed. A live end-to-end (author a token spec →
  SPA renders real `claude_otel` data on otel) is the next step.
