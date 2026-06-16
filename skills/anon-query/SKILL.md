---
name: anon-query
description: >
  Query an ANONYMIZED ClickHouse sandbox where table/column names are tokens and
  values are masked. Trigger before writing SQL against such a sandbox: you author
  over masked data, and a human later sees REAL data through the de-tokenized
  dashboard. Read these rules, then call describe_schema for the concrete names.
---

# Querying an anonymized sandbox

You are querying an **anonymized** ClickHouse database: table and column **names are
tokens**, and **values are masked**. You author dashboard SQL here; when a human opens
the saved dashboard that SQL is **de-tokenized and run against the real database**, so
the human sees real data. **Author for the human** — you neither need nor receive real
values, and you never have to de-anonymize anything.

## 1. Start with `describe_schema`

It is the single source of every concrete name. Each row gives a table **role** (start
at the fact table, join dimensions), each column's **class**, and a **usage hint**.
Never guess token names — read them from `describe_schema`.

## 2. What is real vs masked

- **Names** (table, column) are tokens.
- **`Map` / attribute columns:** the **keys are real** (read them to pick the right
  attribute); each key has a **role** — call **`describe_attributes`** to get it:
  *vocabulary* (real → filter & group), *measure* (real number → aggregate),
  *identity* (masked → GROUP BY only; the readable one to break down by), *sensitive*
  (masked → avoid). Values are masked except numbers/booleans and vocabulary.
- **By class:** `time` & `measure` are **real** (range / aggregate them); `label` &
  `joinkey` are **stable hashes** (GROUP BY / JOIN / uniq still work — only the literal
  is meaningless); `freetext` is **redacted**.

Deep rule: **de-tokenization rewrites NAMES, not VALUES.** A masked value becomes real
only when the human re-runs the query against the real DB — which happens for GROUP BY
*output*, never for a value you hard-code.

## 3. DO

- **GROUP BY freely** — masked group keys **relabel to real** for the human. To break
  down by a person/entity, GROUP BY the **human-readable identity** key (keys are real,
  so pick the email/name-like attribute, not an opaque id).
- **Aggregate** measures; **bucket / range** on time.
- Report **ratios, shares, and shapes** — the sandbox is a **sample**
  (`sandbox_rows` ≪ `total_rows`); absolute totals aren't the whole picture.

## 4. DON'T

- **A constant in `WHERE` is usually a mistake.** Most values are masked, so
  `WHERE key = 'literal'` matches **nothing** once it runs on the real data. Only filter
  on values the usage hint marks **real** (low-cardinality vocabulary), or on **time** /
  **measures**. If a value looks like a hash, **GROUP BY it instead of filtering**.
- Don't GROUP BY or label by an **opaque id** when a readable identity key exists.
- Don't present masked values as real labels, and don't read absolute totals as truth.
- Don't try to reverse tokens or reconstruct real values — impossible and unnecessary.

## 5. Worked shape (placeholders — get real names from `describe_schema`)

```sql
SELECT <map_col>['<identity-key>'] AS who,     -- readable identity → real for the human
       toStartOfDay(<time_col>)    AS day,      -- class=time (real)
       sum(<measure_col>)          AS total     -- class=measure (real)
FROM <fact_table>
WHERE <time_col> >= now() - INTERVAL 7 DAY
  AND <map_col>['<vocab-key>'] = '<real-value>' -- ONLY because the hint marks it real
GROUP BY who, day;
```

## 6. Handoff

To build and save the dashboard, use the **bentoclick-dashboard** skill. For the share
URL, call **`get_dashboards_prefix`** and append your slug.
