// Package store owns the `altinity` meta database: schema, writers, and the
// run protocol. Everything the pipeline produces lands here — profile tables
// hold TOKENS ONLY (safe for the LLM-facing read path); identifier_map and
// masking_plan hold real names (trusted side; never exposed via any
// LLM-facing surface). The manifest row is written LAST: its presence marks a
// complete run, and readers only consume the latest complete run.
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

// ddl: one CREATE TABLE IF NOT EXISTS per meta table. ReplacingMergeTree keyed
// so concurrent/idempotent re-runs dedup instead of conflicting.
var ddl = []string{
	`CREATE TABLE IF NOT EXISTS %[1]s.manifest (
		run_id String, started DateTime, finished DateTime,
		status String, connection String,
		scope_databases Array(String), window_days UInt32, sample_rows UInt64,
		stats String, notes Array(String)
	) ENGINE = ReplacingMergeTree ORDER BY run_id`,
	`CREATE TABLE IF NOT EXISTS %[1]s.identifier_map (
		run_id String, kind String, original String, token String
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, kind, original)`,
	`CREATE TABLE IF NOT EXISTS %[1]s.masking_plan (
		run_id String, database String, table String, column String,
		class String, transform String, included UInt8
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, database, table, column)`,
	`CREATE TABLE IF NOT EXISTS %[1]s.generated_objects (
		run_id String, object_kind String, name String, created_at DateTime
	) ENGINE = ReplacingMergeTree ORDER BY (object_kind, name)`,
	`CREATE TABLE IF NOT EXISTS %[1]s.profile_shape (
		run_id String, key String, value String
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, key)`,
	`CREATE TABLE IF NOT EXISTS %[1]s.profile_catalog (
		run_id String, db_token String, table_token String,
		engine String, engine_family String,
		sorting_key_tok String, partition_key_tok String,
		total_rows UInt64, total_bytes UInt64,
		role String, role_confidence String,
		demoted UInt8, demote_reasons Array(String), review_flags Array(String),
		sandboxed UInt8, sandbox_rows UInt64
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, db_token, table_token)`,
	`CREATE TABLE IF NOT EXISTS %[1]s.profile_columns (
		run_id String, db_token String, table_token String, col_token String,
		position UInt32, type_tok String, class String,
		in_pk UInt8, in_sk UInt8, in_part UInt8, included UInt8
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, db_token, table_token, col_token)`,
	`CREATE TABLE IF NOT EXISTS %[1]s.profile_relations (
		run_id String, rel_kind String,
		src_db_tok String, src_tbl_tok String,
		dst_db_tok String, dst_tbl_tok String, detail String
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, rel_kind, src_db_tok, src_tbl_tok, dst_db_tok, dst_tbl_tok)`,
	`CREATE TABLE IF NOT EXISTS %[1]s.profile_workload (
		run_id String, db_token String, table_token String,
		execs UInt64, total_ms UInt64, sels UInt64, ins UInt64,
		users_tok Array(String)
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, db_token, table_token)`,
	`CREATE TABLE IF NOT EXISTS %[1]s.profile_hot_columns (
		run_id String, db_token String, table_token String, col_token String, touches UInt64
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, db_token, table_token, col_token)`,
	`CREATE TABLE IF NOT EXISTS %[1]s.profile_queries (
		run_id String, db_token String, table_token String,
		query_hash String, query_tok String, execs UInt64
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, db_token, table_token, query_hash)`,
	`CREATE TABLE IF NOT EXISTS %[1]s.profile_conventions (
		run_id String, db_token String, table_token String,
		metric String, numerator UInt64, denominator UInt64, convention String
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, db_token, table_token, metric)`,
	`CREATE TABLE IF NOT EXISTS %[1]s.profile_verification (
		run_id String, claim_type String, subject String, status String, detail String
	) ENGINE = ReplacingMergeTree ORDER BY (run_id, claim_type, subject)`,
}

// MetaTables lists the tables Init creates (used by tests and cleanup).
var MetaTables = []string{
	"manifest", "identifier_map", "masking_plan", "generated_objects",
	"profile_shape", "profile_catalog", "profile_columns", "profile_relations",
	"profile_workload", "profile_hot_columns", "profile_queries",
	"profile_conventions", "profile_verification",
}

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

func (s *Store) Init(ctx context.Context) error {
	if err := s.Ex.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+quoteIdent(s.MetaDB)); err != nil {
		return fmt.Errorf("store: create meta db: %w", err)
	}
	for _, q := range ddl {
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
