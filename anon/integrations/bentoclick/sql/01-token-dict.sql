-- token_to_real — reverse-anonymization dictionary for the bentoclick
-- de-tokenizing view.
--
-- Source of truth is anond's de-anon secret `${SECRET_DB}.identifier_map`
-- (kind, original, token), which a cross-cluster anond run lands on the SOURCE
-- cluster — i.e. this cluster (otel), the one that holds real data and
-- hosts bentoclick. Both the dictionary and its source live in ${SECRET_DB}
-- (isolated from the LLM-facing ${META_DB}/registry). The dictionary exposes
-- only the reverse lookup token -> original.
--
-- Dedup by token: HMAC determinism guarantees one `original` per `token`
-- forever (8-hex collisions are widened to 16-hex at map-build time, so
-- the token domain is injective). GROUP BY token therefore never collapses
-- two distinct originals.
--
-- LIFETIME auto-reload is monotonic: a later anond run only ADDS tokens
-- (a known token never remaps), so a live dictionary never invalidates an
-- already-rendered dashboard.
--
-- SECURITY: this dictionary's DATA is the de-anonymization secret. Grant
-- dictGet on it ONLY to ${DB}_definer (the de-tok view's definer). The
-- LLM's authoring role and ordinary viewers must NOT hold it — they never
-- resolve tokens directly; the DEFINER view does it on their behalf.
--
-- The CLICKHOUSE source authenticates as the dedicated `anon_dict_reader`
-- user via the `anon_dict_src` NAMED COLLECTION (created in 00-anon-rbac.sql)
-- — NOT the server's internal credentials. The collection holds the
-- credentials server-side, so SHOW CREATE DICTIONARY leaks neither the
-- password nor any real name (verified on CH 26.3). See docs/RBAC.md.

CREATE DICTIONARY IF NOT EXISTS ${SECRET_DB}.token_to_real
  ON CLUSTER '{cluster}'
(
    token    String,
    original String
)
PRIMARY KEY token
SOURCE(CLICKHOUSE(NAME anon_dict_src QUERY
  'SELECT token, any(original) AS original FROM ${SECRET_DB}.identifier_map GROUP BY token'))
LAYOUT(COMPLEX_KEY_HASHED())
LIFETIME(MIN 300 MAX 600);
