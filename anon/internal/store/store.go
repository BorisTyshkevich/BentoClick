// Package store owns the `altinity` meta database: schema, writers, and the
// run protocol. The tables split into two groups with different trust levels:
// TrustedTables (identifier_map, masking_plan) hold real names and live ONLY
// on the source cluster's meta DB; ProfileTables hold tokens by construction
// and live on the dest (sandbox) cluster's meta DB, where the LLM-facing read
// path lives. A grants misconfiguration on the LLM-exposed cluster can
// therefore de-anonymize nothing. The manifest row is written LAST: its
// presence marks a complete run, and readers only consume the latest complete
// run.
package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Altinity/anon-discovery/internal/chclient"
)

type Store struct {
	Ex     chclient.Executor
	MetaDB string
}

func New(ex chclient.Executor, metaDB string) *Store {
	return &Store{Ex: ex, MetaDB: metaDB}
}

// ddl: one CREATE TABLE IF NOT EXISTS per meta table, keyed by table name.
// ReplacingMergeTree keyed so concurrent/idempotent re-runs dedup instead of
// conflicting.
var ddl = map[string]string{
	"manifest": `CREATE TABLE IF NOT EXISTS %[1]s.manifest (
		run_id String, started DateTime, finished DateTime,
		status String, connection String,
		scope_databases Array(String), window_days UInt32, sample_rows UInt64,
		stats String, notes Array(String)
	) ENGINE = ReplacingMergeTree ORDER BY run_id`,
	"identifier_map": `CREATE TABLE IF NOT EXISTS %[1]s.identifier_map (
		run_id String, kind String, original String, token String
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, kind, original)`,
	"masking_plan": `CREATE TABLE IF NOT EXISTS %[1]s.masking_plan (
		run_id String, database String, table String, column String,
		class String, transform String, included UInt8
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, database, table, column)`,
	"generated_objects": `CREATE TABLE IF NOT EXISTS %[1]s.generated_objects (
		run_id String, object_kind String, name String, created_at DateTime
	) ENGINE = ReplacingMergeTree ORDER BY (object_kind, name)`,
	"profile_shape": `CREATE TABLE IF NOT EXISTS %[1]s.profile_shape (
		run_id String, key String, value String
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, key)`,
	"profile_catalog": `CREATE TABLE IF NOT EXISTS %[1]s.profile_catalog (
		run_id String, db_token String, table_token String,
		engine String, engine_family String,
		sorting_key_tok String, partition_key_tok String,
		total_rows UInt64, total_bytes UInt64,
		role String, role_confidence String,
		demoted UInt8, demote_reasons Array(String), review_flags Array(String),
		sandboxed UInt8, sandbox_rows UInt64
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, db_token, table_token)`,
	"profile_columns": `CREATE TABLE IF NOT EXISTS %[1]s.profile_columns (
		run_id String, db_token String, table_token String, col_token String,
		position UInt32, type_tok String, class String,
		in_pk UInt8, in_sk UInt8, in_part UInt8, included UInt8
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, db_token, table_token, col_token)`,
	"profile_attr_keys": `CREATE TABLE IF NOT EXISTS %[1]s.profile_attr_keys (
		run_id String, db_token String, table_token String, col_token String,
		attr_key String, role String, cardinality UInt64, kept UInt8
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, db_token, table_token, col_token, attr_key)`,
	"profile_relations": `CREATE TABLE IF NOT EXISTS %[1]s.profile_relations (
		run_id String, rel_kind String,
		src_db_tok String, src_tbl_tok String,
		dst_db_tok String, dst_tbl_tok String, detail String
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, rel_kind, src_db_tok, src_tbl_tok, dst_db_tok, dst_tbl_tok)`,
	"profile_workload": `CREATE TABLE IF NOT EXISTS %[1]s.profile_workload (
		run_id String, db_token String, table_token String,
		execs UInt64, total_ms UInt64, sels UInt64, ins UInt64,
		users_tok Array(String)
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, db_token, table_token)`,
	"profile_hot_columns": `CREATE TABLE IF NOT EXISTS %[1]s.profile_hot_columns (
		run_id String, db_token String, table_token String, col_token String, touches UInt64
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, db_token, table_token, col_token)`,
	"profile_queries": `CREATE TABLE IF NOT EXISTS %[1]s.profile_queries (
		run_id String, db_token String, table_token String,
		query_hash String, query_tok String, execs UInt64
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, db_token, table_token, query_hash)`,
	"profile_conventions": `CREATE TABLE IF NOT EXISTS %[1]s.profile_conventions (
		run_id String, db_token String, table_token String,
		metric String, numerator UInt64, denominator UInt64, convention String
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, db_token, table_token, metric)`,
	"profile_verification": `CREATE TABLE IF NOT EXISTS %[1]s.profile_verification (
		run_id String, claim_type String, subject String, status String, detail String
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, claim_type, subject)`,
}

// TrustedTables hold REAL names — they de-anonymize everything and therefore
// live ONLY on the SOURCE cluster's dedicated SECRET DB (SecretDB, e.g.
// bentosecrets), never on the meta/registry DB the LLM-facing read path uses
// and never on the cluster the LLM can reach.
var TrustedTables = []string{"identifier_map", "masking_plan"}

// ProfileTables hold tokens (plus the registry and the manifest) — these live
// on the DEST (sandbox) cluster's meta DB, where the LLM-facing read path is.
var ProfileTables = []string{
	"manifest", "generated_objects",
	"profile_shape", "profile_catalog", "profile_columns", "profile_attr_keys", "profile_relations",
	"profile_workload", "profile_hot_columns", "profile_queries",
	"profile_conventions", "profile_verification",
}

// MetaTables is the union of both groups (used by tests and cleanup).
var MetaTables = append(append([]string{}, TrustedTables...), ProfileTables...)

// ProfileContentColumns lists, per LLM-readable profile table, the columns
// that can carry tokenized content. The omitted columns hold only enumerated
// vocabulary this tool generates (masking classes, roles, engine names,
// statuses) — they cannot carry customer identifiers by construction, and a
// real column named e.g. "time" must not false-positive against the class
// vocabulary in leak scans. Used by `anond verify` and the integration tests.
var ProfileContentColumns = map[string][]string{
	"profile_shape":        {"value"},
	"profile_catalog":      {"db_token", "table_token", "sorting_key_tok", "partition_key_tok", "review_flags"},
	"profile_columns":      {"db_token", "table_token", "col_token", "type_tok"},
	"profile_relations":    {"src_db_tok", "src_tbl_tok", "dst_db_tok", "dst_tbl_tok", "detail"},
	"profile_workload":     {"db_token", "table_token", "users_tok"},
	"profile_hot_columns":  {"db_token", "table_token", "col_token"},
	"profile_queries":      {"query_tok"},
	"profile_conventions":  {"db_token", "table_token"},
	"profile_verification": {"subject", "detail"},
}

// InitTrusted creates the meta DB and the trusted (real-name) tables — call
// against the SOURCE cluster only.
func (s *Store) InitTrusted(ctx context.Context) error { return s.init(ctx, TrustedTables) }

// InitProfile creates the meta DB and the tokens-only tables — call against
// the DEST (sandbox) cluster. In single-cluster mode both groups land in the
// same meta DB.
func (s *Store) InitProfile(ctx context.Context) error { return s.init(ctx, ProfileTables) }

func (s *Store) init(ctx context.Context, tables []string) error {
	if err := s.Ex.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+quoteIdent(s.MetaDB)); err != nil {
		return fmt.Errorf("store: create meta db: %w", err)
	}
	for _, t := range tables {
		q, ok := ddl[t]
		if !ok {
			return fmt.Errorf("store: no DDL for meta table %q", t)
		}
		if err := s.Ex.Exec(ctx, fmt.Sprintf(q, quoteIdent(s.MetaDB))); err != nil {
			return fmt.Errorf("store: meta ddl: %w", err)
		}
	}
	return nil
}

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func (s *Store) table(name string) string {
	return quoteIdent(s.MetaDB) + "." + quoteIdent(name)
}

// Insert writes rows into a meta table.
func (s *Store) Insert(ctx context.Context, table string, names []string, rows [][]*string) error {
	return s.Ex.Insert(ctx, s.table(table), names, rows)
}

// ChArray renders a []string as a ClickHouse Array(String) TSV cell.
func ChArray(items []string) string {
	parts := make([]string, len(items))
	for i, it := range items {
		it = strings.ReplaceAll(it, `\`, `\\`)
		it = strings.ReplaceAll(it, `'`, `\'`)
		parts[i] = "'" + it + "'"
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// RegisterObject records a created object BEFORE creation, so cleanup can
// always find everything we may have touched.
func (s *Store) RegisterObject(ctx context.Context, runID, kind, name string) error {
	ts := time.Now().UTC().Format("2006-01-02 15:04:05")
	return s.Insert(ctx, "generated_objects",
		[]string{"run_id", "object_kind", "name", "created_at"},
		[][]*string{{chclient.S(runID), chclient.S(kind), chclient.S(name), chclient.S(ts)}})
}

// RegisteredObjects returns all object names ever registered, by kind.
func (s *Store) RegisteredObjects(ctx context.Context) (map[string][]string, error) {
	rows, err := s.Ex.Query(ctx, fmt.Sprintf(
		"SELECT DISTINCT object_kind, name FROM %s ORDER BY object_kind, name", s.table("generated_objects")))
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, r := range rows.Data {
		if r[0] != nil && r[1] != nil {
			out[*r[0]] = append(out[*r[0]], *r[1])
		}
	}
	return out, nil
}

// IsRegistered reports whether an object name was created by us.
func (s *Store) IsRegistered(ctx context.Context, kind, name string) (bool, error) {
	rows, err := s.Ex.Query(ctx, fmt.Sprintf(
		"SELECT count() FROM %s WHERE object_kind = '%s' AND name = '%s'",
		s.table("generated_objects"), sqlEsc(kind), sqlEsc(name)))
	if err != nil {
		return false, err
	}
	return len(rows.Data) == 1 && rows.Data[0][0] != nil && *rows.Data[0][0] != "0", nil
}

func sqlEsc(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `'`, `\'`)
}

// SQLEsc escapes a string for embedding in a single-quoted SQL literal.
func SQLEsc(s string) string { return sqlEsc(s) }

// WriteManifest writes the completion marker (call LAST).
func (s *Store) WriteManifest(ctx context.Context, runID string, cols map[string]string,
	scopeDBs []string, notes []string) error {
	names := []string{"run_id", "started", "finished", "status", "connection",
		"scope_databases", "window_days", "sample_rows", "stats", "notes"}
	row := []*string{
		chclient.S(runID), chclient.S(cols["started"]), chclient.S(cols["finished"]),
		chclient.S("complete"), chclient.S(cols["connection"]),
		chclient.S(ChArray(scopeDBs)), chclient.S(cols["window_days"]),
		chclient.S(cols["sample_rows"]), chclient.S(cols["stats"]),
		chclient.S(ChArray(notes)),
	}
	return s.Insert(ctx, "manifest", names, [][]*string{row})
}

// LatestCompleteRun returns the run_id of the newest complete run ("" if none).
func (s *Store) LatestCompleteRun(ctx context.Context) (string, error) {
	rows, err := s.Ex.Query(ctx, fmt.Sprintf(
		"SELECT run_id FROM %s WHERE status = 'complete' ORDER BY finished DESC LIMIT 1",
		s.table("manifest")))
	if err != nil {
		if strings.Contains(err.Error(), "UNKNOWN_TABLE") || strings.Contains(err.Error(), "doesn't exist") {
			return "", nil
		}
		return "", err
	}
	if len(rows.Data) == 0 || rows.Data[0][0] == nil {
		return "", nil
	}
	return *rows.Data[0][0], nil
}
