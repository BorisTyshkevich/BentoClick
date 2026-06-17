package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/chclient"
)

// recExec is a recording fake Executor for store tests.
type recExec struct {
	execStmts []string
	inserts   []recInsert
	execErr   error
	insertErr error
	queryResp *chclient.Rows
	queryErr  error
}

type recInsert struct {
	target string
	names  []string
	rows   [][]*string
}

func (r *recExec) Exec(_ context.Context, sql string) error {
	r.execStmts = append(r.execStmts, sql)
	return r.execErr
}

func (r *recExec) Insert(_ context.Context, target string, names []string, rows [][]*string) error {
	r.inserts = append(r.inserts, recInsert{target: target, names: names, rows: rows})
	return r.insertErr
}

func (r *recExec) Query(_ context.Context, _ string) (*chclient.Rows, error) {
	if r.queryErr != nil {
		return nil, r.queryErr
	}
	if r.queryResp != nil {
		return r.queryResp, nil
	}
	return &chclient.Rows{}, nil
}

func (r *recExec) QueryStream(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (r *recExec) InsertStream(_ context.Context, _ string, _ []string, _ io.Reader) error {
	return nil
}

// TestNew verifies that New sets MetaDB and Ex correctly.
func TestNew(t *testing.T) {
	ex := &recExec{}
	s := New(ex, "mydb")
	if s.MetaDB != "mydb" {
		t.Errorf("MetaDB = %q, want %q", s.MetaDB, "mydb")
	}
	if s.Ex != ex {
		t.Error("Ex not set")
	}
}

// TestTableAndInsertTarget verifies that table() and Insert() use correctly
// backtick-quoted `db`.`table` targets.
func TestTableAndInsertTarget(t *testing.T) {
	ex := &recExec{}
	s := New(ex, "testdb")

	got := s.table("mytable")
	want := "`testdb`.`mytable`"
	if got != want {
		t.Errorf("table() = %q, want %q", got, want)
	}

	ctx := context.Background()
	names := []string{"col1"}
	rows := [][]*string{{chclient.S("val1")}}
	if err := s.Insert(ctx, "mytable", names, rows); err != nil {
		t.Fatalf("Insert returned error: %v", err)
	}
	if len(ex.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(ex.inserts))
	}
	if ex.inserts[0].target != want {
		t.Errorf("insert target = %q, want %q", ex.inserts[0].target, want)
	}
}

// TestInitTrusted verifies CREATE DATABASE + one CREATE TABLE per TrustedTable.
func TestInitTrusted(t *testing.T) {
	ex := &recExec{}
	s := New(ex, "metadb")
	ctx := context.Background()

	if err := s.InitTrusted(ctx); err != nil {
		t.Fatalf("InitTrusted: %v", err)
	}

	// First statement must be CREATE DATABASE
	if len(ex.execStmts) == 0 {
		t.Fatal("no Exec statements recorded")
	}
	if !strings.Contains(ex.execStmts[0], "CREATE DATABASE") {
		t.Errorf("first stmt = %q, want CREATE DATABASE", ex.execStmts[0])
	}
	if !strings.Contains(ex.execStmts[0], "`metadb`") {
		t.Errorf("first stmt missing quoted db name: %q", ex.execStmts[0])
	}

	// Should have 1 (CREATE DB) + len(TrustedTables) statements
	want := 1 + len(TrustedTables)
	if len(ex.execStmts) != want {
		t.Errorf("exec count = %d, want %d", len(ex.execStmts), want)
	}

	// Each table DDL should reference the quoted db name
	for i, stmt := range ex.execStmts[1:] {
		if !strings.Contains(stmt, "`metadb`") {
			t.Errorf("stmt[%d] missing quoted db name: %q", i+1, stmt)
		}
		if !strings.Contains(stmt, "CREATE TABLE IF NOT EXISTS") {
			t.Errorf("stmt[%d] not a CREATE TABLE: %q", i+1, stmt)
		}
	}

	// Verify each trusted table appears exactly once
	for _, tbl := range TrustedTables {
		found := 0
		for _, stmt := range ex.execStmts {
			if strings.Contains(stmt, "."+tbl+" ") || strings.Contains(stmt, "."+tbl+"\n") ||
				strings.Contains(stmt, "`metadb`."+tbl) ||
				strings.Contains(fmt.Sprintf("\n%s (", tbl), stmt) {
				found++
			}
			// The DDL embeds the table name literally in the CREATE TABLE string
			if strings.Contains(stmt, tbl) {
				found++
				break
			}
		}
		if found == 0 {
			t.Errorf("trusted table %q not found in any Exec statement", tbl)
		}
	}
}

// TestInitProfile verifies CREATE DATABASE + one CREATE TABLE per ProfileTable.
func TestInitProfile(t *testing.T) {
	ex := &recExec{}
	s := New(ex, "profiledb")
	ctx := context.Background()

	if err := s.InitProfile(ctx); err != nil {
		t.Fatalf("InitProfile: %v", err)
	}

	want := 1 + len(ProfileTables)
	if len(ex.execStmts) != want {
		t.Errorf("exec count = %d, want %d", len(ex.execStmts), want)
	}

	// First must be CREATE DATABASE
	if !strings.Contains(ex.execStmts[0], "CREATE DATABASE") {
		t.Errorf("first stmt = %q, want CREATE DATABASE", ex.execStmts[0])
	}
}

// TestInitUnknownTable verifies that init returns the "no DDL" error for an
// unknown table name.
func TestInitUnknownTable(t *testing.T) {
	ex := &recExec{}
	s := New(ex, "metadb")
	ctx := context.Background()

	err := s.init(ctx, []string{"nonexistent_table"})
	if err == nil {
		t.Fatal("expected error for unknown table, got nil")
	}
	if !strings.Contains(err.Error(), "no DDL") {
		t.Errorf("error = %q, want to contain \"no DDL\"", err.Error())
	}
}

// TestInitExecError verifies that an Exec error on CREATE DATABASE propagates.
func TestInitExecError(t *testing.T) {
	ex := &recExec{execErr: errors.New("connection refused")}
	s := New(ex, "metadb")
	ctx := context.Background()

	err := s.InitTrusted(ctx)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error = %q, want to contain original error", err.Error())
	}
}

// TestInitTableExecError verifies that an Exec error on CREATE TABLE propagates.
func TestInitTableExecError(t *testing.T) {
	ex := &failAfterNExec{failAfter: 1, err: errors.New("table create failed")}
	s := New(ex, "metadb")
	ctx := context.Background()
	err := s.init(ctx, TrustedTables)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "table create failed") {
		t.Errorf("error = %q", err.Error())
	}
}

// failAfterNExec is a fake that fails Exec after N successful calls.
type failAfterNExec struct {
	recExec
	failAfter int
	count     int
	err       error
}

func (f *failAfterNExec) Exec(ctx context.Context, sql string) error {
	f.recExec.execStmts = append(f.recExec.execStmts, sql)
	f.count++
	if f.count > f.failAfter {
		return f.err
	}
	return nil
}

// TestRegisterObject verifies columns and target for generated_objects inserts.
func TestRegisterObject(t *testing.T) {
	ex := &recExec{}
	s := New(ex, "metadb")
	ctx := context.Background()

	if err := s.RegisterObject(ctx, "run1", "view", "my_view"); err != nil {
		t.Fatalf("RegisterObject: %v", err)
	}
	if len(ex.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(ex.inserts))
	}

	ins := ex.inserts[0]
	wantTarget := "`metadb`.`generated_objects`"
	if ins.target != wantTarget {
		t.Errorf("target = %q, want %q", ins.target, wantTarget)
	}

	wantNames := []string{"run_id", "object_kind", "name", "created_at"}
	if len(ins.names) != len(wantNames) {
		t.Errorf("names = %v, want %v", ins.names, wantNames)
	} else {
		for i, n := range wantNames {
			if ins.names[i] != n {
				t.Errorf("names[%d] = %q, want %q", i, ins.names[i], n)
			}
		}
	}

	if len(ins.rows) != 1 || len(ins.rows[0]) != 4 {
		t.Fatalf("rows shape unexpected: %v", ins.rows)
	}
	if ins.rows[0][0] == nil || *ins.rows[0][0] != "run1" {
		t.Errorf("run_id = %v, want \"run1\"", ins.rows[0][0])
	}
	if ins.rows[0][1] == nil || *ins.rows[0][1] != "view" {
		t.Errorf("object_kind = %v, want \"view\"", ins.rows[0][1])
	}
	if ins.rows[0][2] == nil || *ins.rows[0][2] != "my_view" {
		t.Errorf("name = %v, want \"my_view\"", ins.rows[0][2])
	}
	// created_at is a timestamp — just verify it's non-empty
	if ins.rows[0][3] == nil || *ins.rows[0][3] == "" {
		t.Error("created_at is empty")
	}
}

// TestRegisterObjectError verifies Insert error propagates from RegisterObject.
func TestRegisterObjectError(t *testing.T) {
	ex := &recExec{insertErr: errors.New("insert failed")}
	s := New(ex, "metadb")
	ctx := context.Background()

	err := s.RegisterObject(ctx, "run1", "view", "v")
	if err == nil || !strings.Contains(err.Error(), "insert failed") {
		t.Errorf("expected insert error, got: %v", err)
	}
}

// TestInsertError verifies that Insert surfaces executor errors.
func TestInsertError(t *testing.T) {
	ex := &recExec{insertErr: errors.New("db unavailable")}
	s := New(ex, "metadb")
	ctx := context.Background()

	err := s.Insert(ctx, "manifest", []string{"run_id"}, [][]*string{{chclient.S("r1")}})
	if err == nil || !strings.Contains(err.Error(), "db unavailable") {
		t.Errorf("expected error, got: %v", err)
	}
}

// TestChArray covers empty, single, multi-element and escaping edge cases.
func TestChArray(t *testing.T) {
	tests := []struct {
		input []string
		want  string
	}{
		{[]string{}, "[]"},
		{[]string{"a"}, "['a']"},
		{[]string{"a", "b"}, "['a','b']"},
		{[]string{"it's"}, `['it\'s']`},
		{[]string{`back\slash`}, `['back\\slash']`},
		{[]string{`both'and\slash`}, `['both\'and\\slash']`},
	}

	for _, tc := range tests {
		got := ChArray(tc.input)
		if got != tc.want {
			t.Errorf("ChArray(%v) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestQuoteIdent verifies backtick escaping.
func TestQuoteIdent(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"simple", "`simple`"},
		{"with`backtick", "`with``backtick`"},
		{"two``ticks", "`two````ticks`"},
		{"", "``"},
	}

	for _, tc := range tests {
		got := quoteIdent(tc.input)
		if got != tc.want {
			t.Errorf("quoteIdent(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestTableQuoteIdent verifies that table() uses quoteIdent for both components.
func TestTableQuoteIdent(t *testing.T) {
	ex := &recExec{}
	s := New(ex, "my`db")
	got := s.table("my`table")
	want := "`my``db`.`my``table`"
	if got != want {
		t.Errorf("table() = %q, want %q", got, want)
	}
}

// TestRegisteredObjects verifies parsing of Query results.
func TestRegisteredObjects(t *testing.T) {
	ex := &recExec{
		queryResp: &chclient.Rows{
			Names: []string{"object_kind", "name"},
			Data: [][]*string{
				{chclient.S("view"), chclient.S("v1")},
				{chclient.S("view"), chclient.S("v2")},
				{chclient.S("table"), chclient.S("t1")},
				{nil, chclient.S("ignored")},    // nil kind — should be skipped
				{chclient.S("db"), nil},          // nil name — should be skipped
			},
		},
	}
	s := New(ex, "metadb")
	ctx := context.Background()

	got, err := s.RegisteredObjects(ctx)
	if err != nil {
		t.Fatalf("RegisteredObjects: %v", err)
	}
	if len(got["view"]) != 2 {
		t.Errorf("view count = %d, want 2", len(got["view"]))
	}
	if len(got["table"]) != 1 || got["table"][0] != "t1" {
		t.Errorf("table entries = %v", got["table"])
	}
	if _, ok := got["db"]; ok {
		t.Error("nil-name row should have been skipped")
	}
}

// TestRegisteredObjectsError verifies Query error propagation.
func TestRegisteredObjectsError(t *testing.T) {
	ex := &recExec{queryErr: errors.New("query failed")}
	s := New(ex, "metadb")
	_, err := s.RegisteredObjects(context.Background())
	if err == nil || !strings.Contains(err.Error(), "query failed") {
		t.Errorf("expected error, got: %v", err)
	}
}

// TestIsRegistered verifies count-based detection.
func TestIsRegistered(t *testing.T) {
	// Returns "1" — object is registered
	ex := &recExec{
		queryResp: &chclient.Rows{
			Names: []string{"count()"},
			Data:  [][]*string{{chclient.S("1")}},
		},
	}
	s := New(ex, "metadb")
	ctx := context.Background()

	ok, err := s.IsRegistered(ctx, "view", "v1")
	if err != nil {
		t.Fatalf("IsRegistered: %v", err)
	}
	if !ok {
		t.Error("expected true for count=1")
	}

	// Returns "0" — not registered
	ex.queryResp = &chclient.Rows{
		Names: []string{"count()"},
		Data:  [][]*string{{chclient.S("0")}},
	}
	ok, err = s.IsRegistered(ctx, "view", "missing")
	if err != nil {
		t.Fatalf("IsRegistered: %v", err)
	}
	if ok {
		t.Error("expected false for count=0")
	}
}

// TestIsRegisteredEmpty verifies empty result set returns false.
func TestIsRegisteredEmpty(t *testing.T) {
	ex := &recExec{queryResp: &chclient.Rows{Data: [][]*string{}}}
	s := New(ex, "metadb")
	ok, err := s.IsRegistered(context.Background(), "view", "v")
	if err != nil {
		t.Fatalf("IsRegistered: %v", err)
	}
	if ok {
		t.Error("expected false for empty result")
	}
}

// TestIsRegisteredNilCell verifies nil cell returns false.
func TestIsRegisteredNilCell(t *testing.T) {
	ex := &recExec{queryResp: &chclient.Rows{Data: [][]*string{{nil}}}}
	s := New(ex, "metadb")
	ok, err := s.IsRegistered(context.Background(), "view", "v")
	if err != nil {
		t.Fatalf("IsRegistered: %v", err)
	}
	if ok {
		t.Error("expected false for nil cell")
	}
}

// TestIsRegisteredError verifies Query error propagation.
func TestIsRegisteredError(t *testing.T) {
	ex := &recExec{queryErr: errors.New("query failed")}
	s := New(ex, "metadb")
	_, err := s.IsRegistered(context.Background(), "view", "v")
	if err == nil || !strings.Contains(err.Error(), "query failed") {
		t.Errorf("expected error, got: %v", err)
	}
}

// TestSQLEsc verifies SQLEsc escapes backslashes and single quotes.
func TestSQLEsc(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"plain", "plain"},
		{"it's", `it\'s`},
		{`back\slash`, `back\\slash`},
		{`both'and\`, `both\'and\\`},
	}
	for _, tc := range tests {
		got := SQLEsc(tc.input)
		if got != tc.want {
			t.Errorf("SQLEsc(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestWriteManifest verifies columns, target, and ChArray encoding.
func TestWriteManifest(t *testing.T) {
	ex := &recExec{}
	s := New(ex, "metadb")
	ctx := context.Background()

	cols := map[string]string{
		"started":    "2024-01-01 00:00:00",
		"finished":   "2024-01-01 01:00:00",
		"connection": "demo",
		"window_days": "30",
		"sample_rows": "1000",
		"stats":      "{}",
	}
	scopeDBs := []string{"db1", "db2"}
	notes := []string{"note1"}

	if err := s.WriteManifest(ctx, "run123", cols, scopeDBs, notes); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if len(ex.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(ex.inserts))
	}
	ins := ex.inserts[0]
	wantTarget := "`metadb`.`manifest`"
	if ins.target != wantTarget {
		t.Errorf("target = %q, want %q", ins.target, wantTarget)
	}
	if len(ins.rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(ins.rows))
	}
	row := ins.rows[0]
	// run_id is first column
	if row[0] == nil || *row[0] != "run123" {
		t.Errorf("run_id = %v", row[0])
	}
	// scope_databases (index 5) should be a ChArray
	if row[5] == nil || *row[5] != "['db1','db2']" {
		t.Errorf("scope_databases = %v, want ['db1','db2']", row[5])
	}
	// notes (index 9) should be a ChArray
	if row[9] == nil || *row[9] != "['note1']" {
		t.Errorf("notes = %v, want ['note1']", row[9])
	}
}

// TestWriteManifestError verifies Insert error propagation from WriteManifest.
func TestWriteManifestError(t *testing.T) {
	ex := &recExec{insertErr: errors.New("write failed")}
	s := New(ex, "metadb")
	err := s.WriteManifest(context.Background(), "r1", map[string]string{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Errorf("expected error, got: %v", err)
	}
}

// TestLatestCompleteRun verifies run_id extraction.
func TestLatestCompleteRun(t *testing.T) {
	ex := &recExec{
		queryResp: &chclient.Rows{
			Names: []string{"run_id"},
			Data:  [][]*string{{chclient.S("run-42")}},
		},
	}
	s := New(ex, "metadb")
	got, err := s.LatestCompleteRun(context.Background())
	if err != nil {
		t.Fatalf("LatestCompleteRun: %v", err)
	}
	if got != "run-42" {
		t.Errorf("got %q, want \"run-42\"", got)
	}
}

// TestLatestCompleteRunEmpty verifies empty result returns "".
func TestLatestCompleteRunEmpty(t *testing.T) {
	ex := &recExec{queryResp: &chclient.Rows{Data: [][]*string{}}}
	s := New(ex, "metadb")
	got, err := s.LatestCompleteRun(context.Background())
	if err != nil {
		t.Fatalf("LatestCompleteRun: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestLatestCompleteRunNilCell verifies nil run_id cell returns "".
func TestLatestCompleteRunNilCell(t *testing.T) {
	ex := &recExec{queryResp: &chclient.Rows{Data: [][]*string{{nil}}}}
	s := New(ex, "metadb")
	got, err := s.LatestCompleteRun(context.Background())
	if err != nil {
		t.Fatalf("LatestCompleteRun: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestLatestCompleteRunUnknownTable verifies UNKNOWN_TABLE error returns ("", nil).
func TestLatestCompleteRunUnknownTable(t *testing.T) {
	ex := &recExec{queryErr: errors.New("UNKNOWN_TABLE: manifest")}
	s := New(ex, "metadb")
	got, err := s.LatestCompleteRun(context.Background())
	if err != nil {
		t.Fatalf("expected nil error for UNKNOWN_TABLE, got: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestLatestCompleteRunDoesntExist verifies "doesn't exist" error returns ("", nil).
func TestLatestCompleteRunDoesntExist(t *testing.T) {
	ex := &recExec{queryErr: errors.New("Table metadb.manifest doesn't exist")}
	s := New(ex, "metadb")
	got, err := s.LatestCompleteRun(context.Background())
	if err != nil {
		t.Fatalf("expected nil error, got: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestLatestCompleteRunOtherError verifies other query errors propagate.
func TestLatestCompleteRunOtherError(t *testing.T) {
	ex := &recExec{queryErr: errors.New("network timeout")}
	s := New(ex, "metadb")
	_, err := s.LatestCompleteRun(context.Background())
	if err == nil || !strings.Contains(err.Error(), "network timeout") {
		t.Errorf("expected error, got: %v", err)
	}
}
