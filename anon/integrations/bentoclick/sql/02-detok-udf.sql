-- detok(s) — translate an LLM-authored (sandbox) text blob into one that runs
-- against real data: (1) expand every anond token to its real identifier via
-- the token_to_real dictionary, then (2) map the sandbox database qualifier
-- `<real>_anon.` back to `<real>.`. Tables/columns are tokens (step 1), but the
-- sandbox DB name is disclosed verbatim (`mydb_anon`, not a token), so
-- the LLM authors `FROM mydb_anon.<tbl_token>` and step 2 resolves the DB.
--
-- This is word-substitution, not SQL parsing: anond tokens occupy a
-- reserved lexical namespace (`<kind>_<hex>`), and a run aborts if any real
-- identifier ever collides with that shape — so a textual replace is exact
-- and a SQL parser is unnecessary.
--
-- Mechanics:
--   extractAll   — pull the distinct token occurrences out of the blob.
--   arrayDistinct — replace each unique token once.
--   arrayFold    — fold replaceRegexpAll over the token list, expanding
--                  each via dictGetOrDefault.
--   \b boundaries — stop an 8-hex token from matching inside a 16-hex one
--                  (collision-widened tokens). Verified on CH 26.3:
--                  `tbl_5f5c0ed2` expands while `tbl_5f5c0ed2aabbccdd`
--                  (not in the map) is left intact.
--   dictGetOrDefault(..., tok) — an UNKNOWN token passes through unchanged,
--                  so a stale/hallucinated token surfaces as a loud
--                  "unknown table" error at query time rather than silent
--                  corruption. (In correct operation there are none: the
--                  LLM explores the sandbox built by the SAME anond run
--                  that populated identifier_map, so every token it knows
--                  is in the map; the anonymized MCP's validate step is the
--                  backstop.)
--
-- `{{param}}` placeholders do not match the token shape, so bentoclick's
-- runtime params survive de-tok and interpolate client-side afterward.
--
-- Non-capturing group in the regex is REQUIRED: extractAll returns the
-- first capturing group when one exists, so `(?:...)` is what makes it
-- return the whole token.
--
-- No database qualifier and no ON CLUSTER: ClickHouse replicates
-- user-defined functions automatically (same as bentoclick's
-- sanitize_json_text).

CREATE FUNCTION detok AS (s) ->
  -- Step 2: map the sandbox DB qualifier `<real>_anon.` back to `<real>.`.
  -- Scoped to a database qualifier — the `_anon` suffix must sit immediately
  -- before `.` or `` `. `` — so a real value that merely ends in `_anon` is
  -- never rewritten. (`\b...\b` token expansion has already fixed the table.)
  replaceRegexpAll(
    -- Step 1: expand every anond token to its real identifier (see header).
    arrayFold(
      (acc, tok) -> replaceRegexpAll(
                      acc,
                      concat('\\b', tok, '\\b'),
                      dictGetOrDefault('${SECRET_DB}.token_to_real', 'original', tuple(tok), tok)),
      arrayDistinct(extractAll(
        s,
        '(?:db|tbl|col|user|role|dict|cluster|disk|host|sql|field|enum)_[0-9a-f]{8,16}')),
      s),
    '([A-Za-z0-9_]+)_anon(`?\\.)',
    '\\1\\2'
  );
