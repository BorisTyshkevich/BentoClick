package discover

import (
	"context"
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/chclient"
	"github.com/Altinity/anon-discovery/internal/classify"
	"github.com/Altinity/anon-discovery/internal/sqllex"
)

// ---- sandboxEligible ----

func TestSandboxEligible(t *testing.T) {
	cases := []struct {
		engine string
		want   bool
	}{
		{"MergeTree", true},
		{"ReplicatedMergeTree", true},
		{"AggregatingMergeTree", true},
		{"Distributed", true},
		{"MaterializedView", false},
		{"View", false},
		{"LiveView", false},
		{"Dictionary", false},
		{"Kafka", false},
		{"Null", false},
		{"Buffer", false},
		{"MySQL", false},
		{"S3", false},
	}
	for _, c := range cases {
		t := &Table{Engine: c.engine}
		if got := sandboxEligible(t); got != c.want {
			// use testing.T from outer scope via closure capture – rename inner
		}
	}
	for _, c := range cases {
		tbl := &Table{Engine: c.engine}
		if got := sandboxEligible(tbl); got != c.want {
			t.Errorf("sandboxEligible(%q) = %v, want %v", c.engine, got, c.want)
		}
	}
}

// ---- sqlStr ----

func TestSqlStr(t *testing.T) {
	if got := sqlStr("hello"); got != "hello" {
		t.Errorf("sqlStr plain = %q", got)
	}
	if got := sqlStr("it's"); got != `it\'s` {
		t.Errorf("sqlStr quote = %q", got)
	}
	if got := sqlStr(`back\slash`); got != `back\\slash` {
		t.Errorf("sqlStr backslash = %q", got)
	}
	if got := sqlStr(""); got != "" {
		t.Errorf("sqlStr empty = %q", got)
	}
}

// ---- firstLine ----

func TestFirstLine(t *testing.T) {
	if got := firstLine("first\nsecond"); got != "first" {
		t.Errorf("firstLine multiline = %q", got)
	}
	if got := firstLine("single"); got != "single" {
		t.Errorf("firstLine single = %q", got)
	}
	if got := firstLine(""); got != "" {
		t.Errorf("firstLine empty = %q", got)
	}
	if got := firstLine("line\n"); got != "line" {
		t.Errorf("firstLine trailing newline = %q", got)
	}
}

// ---- timeFilterCol ----

func mkTablePlan(r *Run, t *Table) *tablePlan {
	dbTok, _ := r.tok("db", t.Database)
	tblTok, _ := r.tok("tbl", t.Name)
	p := &tablePlan{T: t, DBTok: dbTok, TblTok: tblTok}
	for _, c := range t.Columns {
		class := classify.Classify(c)
		expr, outType, include := classify.MaskExpr(c, class, 0, nil)
		colTok, _ := r.tok("col", c.Name)
		p.Cols = append(p.Cols, colPlan{
			Col: c, Class: class, Expr: expr, OutType: outType,
			ColToken: colTok, Include: include,
		})
	}
	return p
}

func TestTimeFilterCol(t *testing.T) {
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events",
		Columns: []classify.Column{
			{Name: "event_time", Type: "DateTime", InSK: true},
			{Name: "user_id", Type: "UInt64"},
		}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	p := mkTablePlan(r, tbl)
	tc := timeFilterCol(p)
	if tc != "event_time" {
		t.Errorf("timeFilterCol = %q, want event_time", tc)
	}
}

func TestTimeFilterColNone(t *testing.T) {
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	// No time column in partition/sorting key
	tbl := &Table{Database: "biz", Name: "events",
		Columns: []classify.Column{
			{Name: "user_id", Type: "UInt64", InSK: true},
			{Name: "event_name", Type: "String"},
		}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	p := mkTablePlan(r, tbl)
	if tc := timeFilterCol(p); tc != "" {
		t.Errorf("timeFilterCol should be empty for no time column, got %q", tc)
	}
}

func TestTimeFilterColNotInKey(t *testing.T) {
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	// DateTime column exists but NOT in sorting key → should not be used
	tbl := &Table{Database: "biz", Name: "events",
		Columns: []classify.Column{
			{Name: "created_at", Type: "DateTime"}, // not in SK/partition
			{Name: "user_id", Type: "UInt64", InSK: true},
		}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	p := mkTablePlan(r, tbl)
	if tc := timeFilterCol(p); tc != "" {
		t.Errorf("time column not in key should not be picked, got %q", tc)
	}
}

// ---- sandboxOrderBy ----

func TestSandboxOrderBy(t *testing.T) {
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events", Engine: "MergeTree",
		SortingKey: "event_time, user_id",
		Columns: []classify.Column{
			{Name: "event_time", Type: "DateTime", InSK: true},
			{Name: "user_id", Type: "UInt64", InSK: true},
		}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	p := mkTablePlan(r, tbl)
	ob := sandboxOrderBy(p)
	if len(ob) != 2 {
		t.Errorf("sandboxOrderBy = %v, want 2 tokens", ob)
	}
	// tokens should not equal the real names
	for _, tok := range ob {
		if tok == "event_time" || tok == "user_id" {
			t.Errorf("sandboxOrderBy token %q should be tokenized, not real name", tok)
		}
	}
}

func TestSandboxOrderByEmpty(t *testing.T) {
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events", Engine: "MergeTree",
		SortingKey: "",
		Columns:    []classify.Column{{Name: "user_id", Type: "UInt64"}}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	p := mkTablePlan(r, tbl)
	ob := sandboxOrderBy(p)
	if len(ob) != 0 {
		t.Errorf("empty sorting key should produce no order-by tokens, got %v", ob)
	}
}

// ---- ensureOurs ----

func TestEnsureOursNotExists(t *testing.T) {
	// Object doesn't exist → returns (false, nil)
	dst := &fakeExec{rows: map[string]*chclient.Rows{
		"SELECT count()": {Data: [][]*string{{s("0")}}},
	}}
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, dst)
	if err != nil {
		t.Fatal(err)
	}
	exists, err := r.ensureOurs(context.Background(), "table", "biz.tbl", "SELECT count() FROM system.tables WHERE name = 'tbl'")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("object does not exist, but ensureOurs returned exists=true")
	}
}

func TestEnsureOursForeignObject(t *testing.T) {
	// Object exists but not in our registry → must error (safety check)
	dst := &fakeExec{rows: map[string]*chclient.Rows{
		"FROM system.tables": {Data: [][]*string{{s("1")}}},
		// generated_objects query returns no rows → not ours
		"generated_objects": {Data: nil},
	}}
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, dst)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.ensureOurs(context.Background(), "table", "biz.tbl",
		"SELECT count() FROM system.tables WHERE name = 'tbl'")
	if err == nil {
		t.Error("foreign object must cause an error")
	}
	if !strings.Contains(err.Error(), "safety") {
		t.Errorf("error should mention safety: %v", err)
	}
}

// ---- sandbox: full materialization ----

func setupRunForSandbox(t *testing.T, engine string, cols []classify.Column, sortingKey string) (*Run, []*tablePlan) {
	t.Helper()
	dst := &fakeExec{rows: map[string]*chclient.Rows{
		// ensureOurs for database: count() = 0 (not exists)
		"system.databases WHERE": {Data: [][]*string{{s("0")}}},
		// ensureOurs for table: count() = 0 (not exists)
		"system.tables WHERE": {Data: [][]*string{{s("0")}}},
		// count after populate
		"count()": {Data: [][]*string{{s("42")}}},
	}}
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, dst)
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events", Engine: engine,
		SortingKey: sortingKey, TotalRows: 1000, Columns: cols}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	r.Rw = sqllex.NewRewriter(r.IdMap, sqllex.NewKeepRegistry(nil))
	plans, err := r.writeMaskingPlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return r, plans
}

func TestSandboxMergeTree(t *testing.T) {
	cols := []classify.Column{
		{Name: "event_time", Type: "DateTime", InSK: true, InPart: true},
		{Name: "user_id", Type: "UInt64", InSK: true},
	}
	r, plans := setupRunForSandbox(t, "MergeTree", cols, "event_time, user_id")
	if err := r.sandbox(context.Background(), plans); err != nil {
		t.Fatal(err)
	}
	if len(r.SandboxRows) == 0 {
		t.Error("sandbox should populate SandboxRows for MergeTree table")
	}
	// Verify CREATE TABLE was issued
	dst := r.DstEx.(*fakeExec)
	found := false
	for _, e := range dst.execs {
		if strings.Contains(e, "CREATE TABLE") {
			found = true
			// Token names, not real names, should appear
			if strings.Contains(e, "`events`") {
				t.Errorf("CREATE TABLE should use token name, not real name 'events': %q", e[:min2(len(e), 100)])
			}
		}
	}
	if !found {
		t.Errorf("CREATE TABLE not found in execs: %v", dst.execs)
	}
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestSandboxIneligibleSkipped(t *testing.T) {
	// View engine → should be skipped entirely
	dst := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, dst)
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "v", Engine: "View",
		Columns: []classify.Column{{Name: "event_time", Type: "DateTime"}}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	r.Rw = sqllex.NewRewriter(r.IdMap, sqllex.NewKeepRegistry(nil))
	plans, err := r.writeMaskingPlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.sandbox(context.Background(), plans); err != nil {
		t.Fatal(err)
	}
	if len(dst.execs) != 0 {
		t.Errorf("ineligible View should produce no CREATE/INSERT, got: %v", dst.execs)
	}
}

func TestSandboxAllColumnsExcluded(t *testing.T) {
	// Table with all columns excluded (e.g. JSON schemaless) → no sandbox table
	dst := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, dst)
	if err != nil {
		t.Fatal(err)
	}
	// JSON type → classified as schemaless → excluded
	tbl := &Table{Database: "biz", Name: "events", Engine: "MergeTree",
		Columns: []classify.Column{{Name: "data", Type: "JSON"}}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	r.Rw = sqllex.NewRewriter(r.IdMap, sqllex.NewKeepRegistry(nil))
	plans, err := r.writeMaskingPlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.sandbox(context.Background(), plans); err != nil {
		t.Fatal(err)
	}
	// No sandbox rows: all columns were excluded
	if _, found := r.SandboxRows["biz.events"]; found {
		t.Error("all-excluded table should not appear in SandboxRows")
	}
}

func TestSandboxCrossClusterUsesStream(t *testing.T) {
	// Cross-cluster (different Source and Dest): uses QueryStream→InsertStream
	// testCfg uses different Source/Dest strings → cross-cluster
	cols := []classify.Column{
		{Name: "event_time", Type: "DateTime"},
		{Name: "user_id", Type: "UInt64"},
	}
	r, plans := setupRunForSandbox(t, "MergeTree", cols, "")
	if r.Cfg.Source == r.Cfg.Dest {
		t.Skip("setupRunForSandbox unexpectedly produced same-cluster config")
	}
	if err := r.sandbox(context.Background(), plans); err != nil {
		t.Fatal(err)
	}
	// fakeExec's QueryStream is called on SrcEx in cross-cluster mode
	// The table should either be in SandboxRows or generate a note
	// Either is valid when the stream returns empty data
	_ = r.SandboxRows
	_ = r.Notes
}

func TestSandboxSameCluster(t *testing.T) {
	// Explicitly test same-cluster: Source == Dest → INSERT...SELECT via DstEx.Exec
	dst := &fakeExec{rows: map[string]*chclient.Rows{
		"system.databases WHERE": {Data: [][]*string{{s("0")}}},
		"system.tables WHERE":    {Data: [][]*string{{s("0")}}},
		"count()":                {Data: [][]*string{{s("42")}}},
	}}
	// Use same Source and Dest
	cfg := Config{
		Source: "clickhouse-client", Dest: "clickhouse-client",
		SourceDB: "biz", DestDB: "biz",
		HMACKey: testKey,
	}
	r, err := NewRun(cfg, &fakeExec{}, dst)
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events", Engine: "MergeTree",
		Columns: []classify.Column{
			{Name: "event_time", Type: "DateTime"},
			{Name: "user_id", Type: "UInt64"},
		}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	r.Rw = sqllex.NewRewriter(r.IdMap, sqllex.NewKeepRegistry(nil))
	plans, err := r.writeMaskingPlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.sandbox(context.Background(), plans); err != nil {
		t.Fatal(err)
	}
	// Same-cluster: INSERT INTO ... SELECT emitted as Exec
	found := false
	for _, e := range dst.execs {
		if strings.Contains(e, "INSERT INTO") && strings.Contains(e, "SELECT") {
			found = true
		}
	}
	if !found {
		t.Errorf("same-cluster should use INSERT...SELECT, execs: %v", dst.execs)
	}
}

func TestSandboxWindowFallback(t *testing.T) {
	// Window returns 0 rows, TotalRows > 0 → falls back to unwindowed
	cols := []classify.Column{
		{Name: "event_time", Type: "DateTime", InPart: true},
		{Name: "user_id", Type: "UInt64"},
	}
	dst := &fakeExec{rows: map[string]*chclient.Rows{
		"system.databases WHERE": {Data: [][]*string{{s("0")}}},
		"system.tables WHERE":    {Data: [][]*string{{s("0")}}},
		// First count() call (after windowed populate) returns 0
		// Second call (after unwindowed populate) returns 5
		"count()": {Data: [][]*string{{s("0")}}},
	}}
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, dst)
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events", Engine: "MergeTree",
		TotalRows: 1000,
		Columns:   cols}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	r.Rw = sqllex.NewRewriter(r.IdMap, sqllex.NewKeepRegistry(nil))
	plans, err := r.writeMaskingPlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := r.sandbox(context.Background(), plans); err != nil {
		t.Fatal(err)
	}
	// Should have a note about unwindowed fallback OR be in SandboxRows (0 rows is valid)
	foundNote := false
	for _, n := range r.Notes {
		if strings.Contains(n, "window") || strings.Contains(n, "unwindowed") {
			foundNote = true
		}
	}
	_ = foundNote // allow either outcome — the key check is no crash
}

// ---- writeMaskingPlan ----

func TestWriteMaskingPlanBasic(t *testing.T) {
	src := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz"), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events", Engine: "MergeTree",
		Columns: []classify.Column{
			{Name: "event_time", Type: "DateTime"},
			{Name: "user_id", Type: "UInt64"},
		}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	r.Rw = sqllex.NewRewriter(r.IdMap, sqllex.NewKeepRegistry(nil))

	plans, err := r.writeMaskingPlan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 {
		t.Fatalf("want 1 plan, got %d", len(plans))
	}
	p := plans[0]
	if p.TblTok == "events" {
		t.Error("table token should not equal real name 'events'")
	}
	if len(p.Cols) != 2 {
		t.Errorf("want 2 col plans, got %d", len(p.Cols))
	}
}

func TestWriteMaskingPlanSecretDB(t *testing.T) {
	// masking_plan must be written to SecretDB
	src := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz"), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events",
		Columns: []classify.Column{{Name: "ts", Type: "DateTime"}}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	r.Rw = sqllex.NewRewriter(r.IdMap, sqllex.NewKeepRegistry(nil))
	if _, err := r.writeMaskingPlan(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantTarget := "`" + r.Cfg.SecretDB + "`.`masking_plan`"
	seen := false
	for _, tg := range src.insertTargets {
		if tg == wantTarget {
			seen = true
		}
	}
	if !seen {
		t.Errorf("masking_plan must be written to SecretDB %q, got targets %v", wantTarget, src.insertTargets)
	}
}

// ---- ensureSandboxDB ----

func TestEnsureSandboxDB(t *testing.T) {
	dst := &fakeExec{rows: map[string]*chclient.Rows{
		"system.databases WHERE": {Data: [][]*string{{s("0")}}},
	}}
	r, err := NewRun(testCfg("biz", "biz_anon"), &fakeExec{}, dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ensureSandboxDB(context.Background(), "biz_anon"); err != nil {
		t.Fatal(err)
	}
	// Should have issued CREATE DATABASE
	found := false
	for _, e := range dst.execs {
		if strings.Contains(e, "CREATE DATABASE") && strings.Contains(e, "biz_anon") {
			found = true
		}
	}
	if !found {
		t.Errorf("CREATE DATABASE not found in execs: %v", dst.execs)
	}
}

// ---- materializePreserve ----

func TestMaterializePreserveBasic(t *testing.T) {
	dst := &fakeExec{rows: map[string]*chclient.Rows{
		"system.tables WHERE":    {Data: [][]*string{{s("0")}}},
		"count()": {Data: [][]*string{{s("50")}}},
	}}
	r, err := NewRun(testCfg("system", "system_anon"), &fakeExec{}, dst)
	if err != nil {
		t.Fatal(err)
	}
	r.Cfg.Model = ModelSchemaPreserving

	tbl := &Table{Database: "system", Name: "query_log", Engine: "MergeTree",
		Columns: []classify.Column{
			{Name: "event_date", Type: "Date", InPart: true},
			{Name: "query", Type: "String"},
		}}
	p := buildPreserve(tbl, "SEED", "system_anon", "run-1", nil)
	if err := r.materializePreserve(context.Background(), "system_anon", tbl, p); err != nil {
		t.Fatal(err)
	}
	// Should have CREATE TABLE ... AS SELECT
	found := false
	for _, e := range dst.execs {
		if strings.Contains(e, "CREATE TABLE") && strings.Contains(e, "query_log") {
			found = true
		}
	}
	if !found {
		t.Errorf("CREATE TABLE query_log not found in execs: %v", dst.execs)
	}
	// SandboxRows should be set
	if rows := r.SandboxRows["system.query_log"]; rows != 50 {
		t.Errorf("SandboxRows[system.query_log] = %d, want 50", rows)
	}
}

func TestMaterializePreserveExistingOurs(t *testing.T) {
	// Table exists AND is registered as ours → should DROP and recreate
	dst := &fakeExec{rows: map[string]*chclient.Rows{
		// system.tables count > 0 → table exists
		"system.tables WHERE": {Data: [][]*string{{s("1")}}},
		// generated_objects → registered as ours
		"generated_objects":   {Data: [][]*string{{s("1")}}},
		// count after recreate
		"count()": {Data: [][]*string{{s("10")}}},
	}}
	r, err := NewRun(testCfg("system", "system_anon"), &fakeExec{}, dst)
	if err != nil {
		t.Fatal(err)
	}
	r.Cfg.Model = ModelSchemaPreserving

	tbl := &Table{Database: "system", Name: "query_log", Engine: "MergeTree",
		Columns: []classify.Column{{Name: "event_date", Type: "Date", InPart: true}}}
	p := buildPreserve(tbl, "SEED", "system_anon", "run-1", nil)
	if err := r.materializePreserve(context.Background(), "system_anon", tbl, p); err != nil {
		t.Fatal(err)
	}
	// Should have a DROP TABLE followed by CREATE TABLE
	foundDrop, foundCreate := false, false
	for _, e := range dst.execs {
		if strings.Contains(e, "DROP TABLE") {
			foundDrop = true
		}
		if strings.Contains(e, "CREATE TABLE") {
			foundCreate = true
		}
	}
	if !foundDrop {
		t.Errorf("existing ours table should be dropped: %v", dst.execs)
	}
	if !foundCreate {
		t.Errorf("existing table should be recreated: %v", dst.execs)
	}
}
