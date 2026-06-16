# RBAC — anond + bentoclick

The least-privilege access model across both clusters. The whole anonymization
trust model reduces to grants: if any login-capable principal can both resolve
a token and read real data, the boundary is gone. This doc is the single
source of truth for who-can-touch-what; the SQL that implements it is
`integrations/bentoclick/sql/00-anon-rbac.sql` and `rbac/anond-service-users.sql`.

## Threat model

Two things must never meet in one principal:

1. **Token resolution** — reading `identifier_map` or `dictGet`-ing
   `token_to_real` (turns `tbl_5f5c0ed2` back into `otel_logs`).
2. **Real data / real identifiers** — `SELECT` on the real data DB, or on the
   de-tokenized `dashboards` view.

The hard case — the **same** `user@domain.com` reaching ClickHouse both
directly (SPA → real data) and indirectly (LLM → MCP, no real data) — can't be
solved by RBAC on one identity alone (a granted role is reachable via
`SET ROLE` from either path). Two escapes: make the two paths **two identities**
(Option C, implemented below — the LLM authors as `anon_author`), or keep one
identity and scope authority by the **channel's token** (Option A, enforced by
Antalya OIDC role mapping + Auth0 tuning — future, see
[`OAUTH-ROLE-SEPARATION.md`](OAUTH-ROLE-SEPARATION.md)).

The **LLM** is the untrusted actor: it must hold neither. The **human viewer**
is trusted with real data (their own grants) but never needs token resolution.
**anond** and the **bentoclick definer** are trusted machinery, each scoped to
exactly its job. The de-anon secret (`identifier_map` + `token_to_real` data)
is reachable by exactly two principals: `anon_dict_reader` (to load the
dictionary) and `${DB}_definer` (to resolve tokens inside the DEFINER view).

## Topology

| | OTEL (source) | DEMO (sandbox) |
|---|---|---|
| Real data | `claude_otel.*` | — (none) |
| Masked data | — | `claude_otel.*` (token tables) |
| Trusted meta | `altinity.{identifier_map, masking_plan}` | — |
| Tokens-only meta | — | `altinity.{profile_*, generated_objects, manifest}` |
| bentoclick | `bentoclick.*` (dashboards_raw → dashboards_tok → dashboards view) + `token_to_real` dict | — |
| LLM touches | writes token specs; reads `dashboards_tok` (tokens) | reads sandbox + `profile_*` (tokens) |
| Human touches | reads `bentoclick.dashboards` (real), runs panel SQL on `claude_otel.*` | — |

## Principals

### OTEL (source)

| Principal | Type | Grants | Explicitly denied |
|---|---|---|---|
| `anond` | service user | `SELECT system.*`; `SELECT ${DATA_DB}.*`; on `${META_DB}.*`: `SELECT, INSERT, CREATE TABLE, CREATE DICTIONARY, ALTER, DROP TABLE` | write on `${DATA_DB}`; CREATE/DROP DATABASE; access management |
| `anon_dict_reader` | service user | `SELECT ${META_DB}.identifier_map` — and nothing else (`DEFAULT ROLE NONE`) | everything else |
| `${DB}_definer` | definer user | `SELECT ${DB}.dashboards_raw`; `INSERT/SELECT ${DB}.dashboards_tok`; `dictGet ${META_DB}.token_to_real` | real `${DATA_DB}`; `identifier_map` direct |
| `anon_author_role` | LLM role | `INSERT(cols) ${DB}.dashboards_raw`; `SELECT ${DB}.dashboards_tok` (tokens) | `${DB}.dashboards` (view), `token_to_real`, `${META_DB}.*`, `${DATA_DB}.*` |
| `${DB}_anon_viewer_role` | human role | `SELECT ${DB}.dashboards` (de-tok view) + `${DB}.dashboards_prefix`; **plus the viewer's own `${DATA_DB}` grants, assigned per-user** | `dashboards_tok`, `dashboards_raw`, `token_to_real`, `${META_DB}.*` |

### DEMO (sandbox)

| Principal | Type | Grants | Notes |
|---|---|---|---|
| `anond` | service user | `CREATE DATABASE`; `SELECT, INSERT, CREATE TABLE, DROP TABLE, DROP DATABASE` (sandbox DBs + `${META_DB}` profile); `SELECT system.*` | `CREATE DATABASE` is the one unavoidable broad grant (sandbox DB names are dynamic); foreign-object safety is enforced in anond's code, not RBAC |
| `anon_mcp_reader` | LLM explore role | `SELECT` on sandbox `claude_otel.*` (masked) + `${META_DB}.profile_*` | `identifier_map`/`masking_plan` don't exist here (trusted split) |

## How the LLM is authored vs how the human views (the split)

Stock bentoclick has one `${DB}_reader_role` with `SELECT ON ${DB}.*` and a
`${DB}_writer_role` that includes it. **That role is too broad for anon mode**:
`${DB}.*` now spans both `dashboards_tok` (tokens) and `dashboards` (the de-tok
view, real names). anon mode therefore splits it:

- the **LLM** gets `anon_author_role` → `dashboards_tok` only (tokens), never
  the view;
- the **human** gets `${DB}_anon_viewer_role` → `dashboards` only (real),
  never the tokenized tier.

Do **not** grant the stock `reader_role`/`writer_role` to either in anon mode.

## Secret handling

- **Dictionary credentials**: the `token_to_real` `CLICKHOUSE` source
  authenticates as `anon_dict_reader` via the `anon_dict_src` **named
  collection**, not the server's internal user. The password lives in the
  collection (server-side); `SHOW CREATE DICTIONARY` shows neither the password
  nor any real name. Verified on CH 26.3 (`position()` of both = 0).
- **DEFINER bodies**: the sanitize MV and the de-tok view run
  `SQL SECURITY DEFINER`. Their `SHOW CREATE` exposes only `sanitize_json_text(…)`
  / `detok(…)` / `dictGet(…)` — never real names, which live only in dictionary
  *data*. This is the same CH-26.3 property that drove choosing a materialized
  sandbox over masking views in `DESIGN.md` §5.
- **HMAC key**: lives in anond's config (`~/.anon-hmac-key`), never in CH. It
  determines every token; rotating it re-tokens everything.
- **`DICT_READER_PW` / `ANOND_PW`**: ordinary DB credentials; rotate as such.
  They are NOT the de-anon secret (that's the map data + HMAC key).

## anond is no longer admin

The CLI runs today as `clickhouse_operator`. The redesign gives it a dedicated
`anond` user per cluster (`rbac/anond-service-users.sql`). On the **source**
(real data) it gets no `CREATE DATABASE` and no real-data writes — pre-create
`${META_DB}` once as admin. On the **sandbox** it needs `CREATE DATABASE`
(dynamic token DB names); that breadth is bounded only by anond's own
registry/`ensureOurs` logic, not by CH grants — stated honestly because CH
can't grant CREATE DATABASE by name pattern.

## Verify your grants

```sql
-- the de-anon secret is reachable by exactly two principals:
SHOW GRANTS FOR anon_dict_reader;          -- exactly: SELECT altinity.identifier_map
SHOW GRANTS FOR bentoclick_definer;        -- includes dictGet altinity.token_to_real

-- the LLM role resolves nothing and sees no real data:
SHOW GRANTS FOR anon_author_role;          -- only INSERT dashboards_raw + SELECT dashboards_tok
-- negative checks, as a user carrying ONLY anon_author_role:
--   SELECT … FROM bentoclick.dashboards         -> DENIED (the de-tok view)
--   SELECT dictGet('altinity.token_to_real',…)  -> DENIED
--   SELECT … FROM bentoclick.dashboards_tok     -> ALLOWED (tokens)

-- the dictionary leaks no secret in its definition:
SELECT position(create_table_query, '<the password>'),    -- expect 0
       position(create_table_query, '<a known real name>') -- expect 0
FROM system.tables WHERE database = 'altinity' AND name = 'token_to_real';

-- the viewer sees real specs but not the tokenized tier:
SHOW GRANTS FOR bentoclick_anon_viewer_role;  -- SELECT dashboards + dashboards_prefix only
```

## Apply order

1. `rbac/anond-service-users.sql` (admin, once per cluster) — the `anond` user.
2. anond run populates `${META_DB}.identifier_map` on the source.
3. On the source, as admin: `00-anon-rbac.sql` → `01-token-dict.sql` →
   `02-detok-udf.sql` → `03-dashboards-anon.sql`.
4. Assign roles to identities: `anon_author_role` to the LLM's MCP CH identity;
   `${DB}_anon_viewer_role` (+ per-user `${DATA_DB}` grants) to JWT viewers;
   `anon_mcp_reader` to the LLM's read identity on the sandbox.

## Out of scope

- The anonymized-mode altinity-mcp that *assumes* `anon_author_role` /
  `anon_mcp_reader` for the LLM is deferred (anond-in-MCP work). The roles
  exist; wiring the MCP to them is later.
- Per-user `${DATA_DB}` data grants for viewers are the deployment's existing
  concern (the same grants that gate any direct ClickHouse access); anon mode
  adds nothing there beyond the dashboard-view grant.
