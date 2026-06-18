# Tokenized masked sandboxes (`<db>_anon`)

Read this **before authoring** when your data source is a `*_anon` database.
(Non-sandbox ClickHouse: ignore this file and author normally.)

A `*_anon` database is a **tokenized masked sandbox**: you query masked/
tokenized data, and the saved dashboard is de-tokenized and the sandbox DB
rewritten to the real DB at view time, so the **human sees real identifiers and
data**. You author for the human and never de-anonymize anything.

## 1. Discover via the registry

There is no bulk schema-dump tool. Query `bentoclick.schema_guide` with
**`execute_query`**, **always filtered** so you pull only a slice (the whole
registry is large — many sandboxes × many columns). Columns:
`anon_database, model, naming, table_name, table_role, column_name, type, class, usage`.

- list sandboxes: `SELECT DISTINCT anon_database, model, naming FROM bentoclick.schema_guide`
- browse a sandbox's tables: `SELECT DISTINCT table_name, table_role FROM
  bentoclick.schema_guide WHERE anon_database = '<db>'`
- describe one table: `SELECT column_name, type, class, usage FROM bentoclick.schema_guide
  WHERE anon_database = '<db>' AND table_name = '<t>' ORDER BY position`
- attrmap keys: same shape against `bentoclick.attr_guide`
  (`table_name, column_name, attr_key, role, usage`), filtered by `anon_database`/`table_name`.

Each row's contract:

- `naming` — whether table/column **names** are tokens (`tbl_<hex>`) or **real**.
- `class` — the per-column rule:
  - `real` — verbatim value: filter, group, aggregate freely.
  - `identifier` — a deterministic token: GROUP BY / JOIN / uniq only; the literal
    is meaningless but it **relabels to the real value** for the human.
  - `redacted` — masked free text: never filter, group, or show it.
  - `attrmap` — a `Map`: keys are real, values are per-key roles (see `attr_guide`):
    vocabulary / measure / identity / sensitive.

## 2. DO

GROUP BY `identifier` columns (they relabel to real), aggregate `real`
measures, range on time, and report ratios/shapes — the sandbox is a **sample**
(`sandbox_rows` ≪ `total_rows`), so absolute totals aren't the whole truth.

## 3. DON'T

Don't filter a masked literal — `WHERE col = 'x'` matches nothing on an
`identifier`/`redacted` column; only filter values whose `class=real`. Don't show
a token as a label, and don't try to reverse a token (impossible, unnecessary).

## 4. Redacted columns

They show you nothing useful. To surface a real, value-masked rendering for the
human, apply a **DB-appropriate query-time transform — your domain skill provides
the specifics**. bentoclick stays domain-agnostic.

## 5. Keep tokens out of `title`/`subtitle`

Only `panels` are de-tokenized; a token left in `title`/`subtitle` would show
verbatim to the human.
