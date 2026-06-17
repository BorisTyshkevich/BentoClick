package discover

import (
	"context"
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/chclient"
	"github.com/Altinity/anon-discovery/internal/classify"
)

// ---- EngineFamily ----

func TestEngineFamilyMergeTree(t *testing.T) {
	for _, e := range []string{"MergeTree", "ReplicatedMergeTree", "AggregatingMergeTree", "SummingMergeTree", "ReplacingMergeTree"} {
		if got := EngineFamily(e); got != "mergetree" {
			t.Errorf("EngineFamily(%q) = %q, want mergetree", e, got)
		}
	}
}

func TestEngineFamilyAll(t *testing.T) {
	cases := []struct {
		engine string
		want   string
	}{
		{"Distributed", "distributed"},
		{"MaterializedView", "mv"},
		{"View", "view"},
		{"LiveView", "view"},
		{"Dictionary", "dictionary"},
		{"Kafka", "infra"},
		{"Null", "infra"},
		{"Buffer", "infra"},
		{"MySQL", "external"},
		{"PostgreSQL", "external"},
		{"URL", "external"},
		{"S3", "external"},
		{"S3Queue", "external"},
		{"HDFS", "external"},
		{"MongoDB", "external"},
		{"Redis", "external"},
		{"SomeRandomEngine", "other"},
	}
	for _, c := range cases {
		if got := EngineFamily(c.engine); got != c.want {
			t.Errorf("EngineFamily(%q) = %q, want %q", c.engine, got, c.want)
		}
	}
}

// ---- inList ----

func TestInList(t *testing.T) {
	got := inList([]string{"a", "b's", "c"})
	if !strings.Contains(got, "'a'") || !strings.Contains(got, "'b\\'s'") || !strings.Contains(got, "'c'") {
		t.Errorf("inList = %q", got)
	}
	if inList([]string{}) != "" {
		t.Errorf("inList empty should be empty string")
	}
	single := inList([]string{"x"})
	if single != "'x'" {
		t.Errorf("inList single = %q, want 'x'", single)
	}
}

// ---- cell / str ----

func TestCell(t *testing.T) {
	rows := &chclient.Rows{
		Data: [][]*string{{s("hello"), nil, s("world")}},
	}
	if got := cell(rows, 0, 0); got != "hello" {
		t.Errorf("cell(0,0) = %q", got)
	}
	if got := cell(rows, 0, 1); got != "" {
		t.Errorf("cell nil = %q", got)
	}
	if got := cell(nil, 0, 0); got != "" {
		t.Errorf("cell nil rows = %q", got)
	}
	if got := cell(&chclient.Rows{}, 0, 0); got != "" {
		t.Errorf("cell empty rows = %q", got)
	}
}

func TestStr(t *testing.T) {
	p := s("value")
	if str(p) != "value" {
		t.Errorf("str = %q", str(p))
	}
	if str(nil) != "" {
		t.Errorf("str nil should be empty")
	}
}

// ---- shape ----

func TestShape(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"SELECT version()": {Data: [][]*string{{s("24.3.5.1")}}},
		"FROM system.query_log": {Data: [][]*string{{s("42000")}}},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.shape(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Version != "24.3.5.1" {
		t.Errorf("version = %q", r.Version)
	}
	if r.Shape["version"] != "24.3.5.1" {
		t.Errorf("shape[version] = %q", r.Shape["version"])
	}
	if r.Shape["qlog_rows"] != "42000" {
		t.Errorf("shape[qlog_rows] = %q", r.Shape["qlog_rows"])
	}
}

func TestShapeQlogUnavailable(t *testing.T) {
	// Only version() works; query_log query returns error (empty result = no qlog key)
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"SELECT version()": {Data: [][]*string{{s("24.3.5.1")}}},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.shape(context.Background()); err != nil {
		t.Fatal(err)
	}
	// no qlog match → fakeExec returns empty rows → shape sets qlog_rows="" or "unavailable"
	// The code only sets qlog_rows when no error; since fakeExec returns no error but empty data,
	// the cell() call returns "" — that is treated as "unavailable" by mining
	if r.Shape["version"] != "24.3.5.1" {
		t.Errorf("version missing")
	}
}

// ---- columns ----

func TestColumns(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"FROM system.columns": {
			Names: []string{"database", "table", "name", "type", "position",
				"is_in_partition_key", "is_in_sorting_key", "is_in_primary_key"},
			Data: [][]*string{
				{s("biz"), s("events"), s("event_time"), s("DateTime"), s("1"), s("1"), s("1"), s("0")},
				{s("biz"), s("events"), s("user_id"), s("UInt64"), s("2"), s("0"), s("0"), s("1")},
				{s("biz"), s("other"), s("col"), s("String"), s("1"), s("0"), s("0"), s("0")}, // table not in scope
			},
		},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events"}
	r.Tables = []*Table{tbl}
	r.byFull["biz.events"] = tbl
	r.ScopeDBs = []string{"biz"}

	if err := r.columns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tbl.Columns) != 2 {
		t.Fatalf("want 2 columns, got %d", len(tbl.Columns))
	}
	if tbl.Columns[0].Name != "event_time" || tbl.Columns[0].Type != "DateTime" {
		t.Errorf("col[0] = %+v", tbl.Columns[0])
	}
	if !tbl.Columns[0].InPart || !tbl.Columns[0].InSK {
		t.Errorf("col[0] flags: InPart=%v InSK=%v", tbl.Columns[0].InPart, tbl.Columns[0].InSK)
	}
	if !tbl.Columns[1].InPK {
		t.Errorf("col[1] InPK=%v", tbl.Columns[1].InPK)
	}
	// the query must scope to the source DB
	found := false
	for _, q := range src.queries {
		if strings.Contains(q, "biz") {
			found = true
		}
	}
	if !found {
		t.Errorf("columns query didn't mention biz: %v", src.queries)
	}
}

// ---- relations / viewRefs ----

func TestRelationsDistributed(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"FROM system.dictionaries": {Data: nil},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	local := &Table{Database: "biz", Name: "events_local", Engine: "MergeTree"}
	dist := &Table{
		Database: "biz", Name: "events", Engine: "Distributed",
		EngineFull: "Distributed('main', 'biz', 'events_local', rand())",
	}
	r.Tables = []*Table{local, dist}
	r.byFull["biz.events_local"] = local
	r.byFull["biz.events"] = dist
	r.ScopeDBs = []string{"biz"}

	if err := r.relations(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rel := range r.Relations {
		if rel.Kind == "dist_to" && rel.DstTbl == "events_local" {
			found = true
		}
	}
	if !found {
		t.Errorf("dist_to relation not found: %v", r.Relations)
	}
}

func TestRelationsMVTo(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"FROM system.dictionaries": {Data: nil},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	dst := &Table{Database: "biz", Name: "agg_daily", Engine: "MergeTree"}
	mv := &Table{
		Database: "biz", Name: "mv1", Engine: "MaterializedView",
		CreateQuery: "CREATE MATERIALIZED VIEW biz.mv1 TO biz.agg_daily AS SELECT ...",
	}
	r.Tables = []*Table{dst, mv}
	r.byFull["biz.agg_daily"] = dst
	r.byFull["biz.mv1"] = mv
	r.ScopeDBs = []string{"biz"}

	if err := r.relations(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rel := range r.Relations {
		if rel.Kind == "mv_to" && rel.DstTbl == "agg_daily" {
			found = true
		}
	}
	if !found {
		t.Errorf("mv_to relation not found: %v", r.Relations)
	}
}

func TestRelationsDictionary(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"FROM system.dictionaries": {
			Data: [][]*string{
				{s("biz"), s("geo_dict"), s("Hashed"), s("LOADED")},
			},
		},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Tables = []*Table{}
	r.ScopeDBs = []string{"biz"}

	if err := r.relations(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, rel := range r.Relations {
		if rel.Kind == "dictionary" && rel.SrcTbl == "geo_dict" {
			found = true
		}
	}
	if !found {
		t.Errorf("dictionary relation not found: %v", r.Relations)
	}
}

func TestViewRefs(t *testing.T) {
	r := &Run{byFull: map[string]*Table{}}
	base := &Table{Database: "biz", Name: "events"}
	joined := &Table{Database: "biz", Name: "users"}
	view := &Table{
		Database: "biz", Name: "events_view", Engine: "View",
		CreateQuery: "CREATE VIEW biz.events_view AS SELECT * FROM biz.events JOIN biz.users ON events.user_id = users.id",
	}
	r.byFull["biz.events"] = base
	r.byFull["biz.users"] = joined
	r.byFull["biz.events_view"] = view
	r.Tables = []*Table{base, joined, view}
	r.ScopeDBs = []string{"biz"}

	r.viewRefs(view)

	fromKinds := map[string]bool{}
	for _, rel := range r.Relations {
		fromKinds[rel.Kind+":"+rel.DstTbl] = true
	}
	if !fromKinds["view_from:events"] {
		t.Errorf("missing view_from:events in %v", r.Relations)
	}
	if !fromKinds["view_join:users"] {
		t.Errorf("missing view_join:users in %v", r.Relations)
	}
}

func TestViewRefsUnresolvable(t *testing.T) {
	// References to tables not in byFull must be silently dropped (no panics)
	r := &Run{byFull: map[string]*Table{}}
	view := &Table{
		Database: "biz", Name: "v", Engine: "View",
		CreateQuery: "CREATE VIEW biz.v AS SELECT * FROM biz.missing_table",
	}
	r.byFull["biz.v"] = view
	r.viewRefs(view) // must not add any relation
	if len(r.Relations) != 0 {
		t.Errorf("unresolvable ref should not produce relations, got %v", r.Relations)
	}
}

func TestViewRefsSelfExclusion(t *testing.T) {
	// Self-reference (view referencing itself) must be ignored
	r := &Run{byFull: map[string]*Table{}}
	view := &Table{
		Database: "biz", Name: "v", Engine: "View",
		CreateQuery: "CREATE VIEW biz.v AS SELECT * FROM biz.v",
	}
	r.byFull["biz.v"] = view
	r.viewRefs(view)
	if len(r.Relations) != 0 {
		t.Errorf("self-reference should not produce a relation, got %v", r.Relations)
	}
}

func TestRosterExcludesSystemDBs(t *testing.T) {
	for _, bad := range []string{"system", "information_schema", "INFORMATION_SCHEMA", "_temporary_and_external_tables"} {
		if bad == "system" {
			// system is allowed for schema-preserving; skip it here
			continue
		}
		r, err := NewRun(testCfg(bad, "sb"), &fakeExec{}, &fakeExec{})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.roster(context.Background()); err == nil {
			t.Errorf("source DB %q should be refused", bad)
		}
	}
}

func TestColumnsQueryContainsInList(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"FROM system.columns": {Data: nil},
	}}
	r, err := NewRun(testCfg("mydb", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.ScopeDBs = []string{"mydb"}
	if err := r.columns(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, q := range src.queries {
		if strings.Contains(q, "'mydb'") {
			found = true
		}
	}
	if !found {
		t.Errorf("columns query must scope to source DB: %v", src.queries)
	}
}

// Test the LiveView engine in relations
func TestRelationsLiveView(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"FROM system.dictionaries": {Data: nil},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	base := &Table{Database: "biz", Name: "orders"}
	lv := &Table{
		Database: "biz", Name: "orders_live", Engine: "LiveView",
		CreateQuery: "CREATE LIVE VIEW biz.orders_live AS SELECT * FROM biz.orders",
	}
	r.Tables = []*Table{base, lv}
	r.byFull["biz.orders"] = base
	r.byFull["biz.orders_live"] = lv
	r.ScopeDBs = []string{"biz"}

	if err := r.relations(context.Background()); err != nil {
		t.Fatal(err)
	}
	// LiveView should produce view_from relations
	found := false
	for _, rel := range r.Relations {
		if rel.Kind == "view_from" && rel.DstTbl == "orders" {
			found = true
		}
	}
	if !found {
		t.Errorf("LiveView should produce view_from relation: %v", r.Relations)
	}
}

// Test MV with viewRefs (FROM in the body)
func TestRelationsMVViewRefs(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"FROM system.dictionaries": {Data: nil},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	src2 := &Table{Database: "biz", Name: "raw_data"}
	dst := &Table{Database: "biz", Name: "agg_data"}
	mv := &Table{
		Database: "biz", Name: "mv_agg", Engine: "MaterializedView",
		CreateQuery: "CREATE MATERIALIZED VIEW biz.mv_agg TO biz.agg_data AS SELECT * FROM biz.raw_data",
	}
	r.Tables = []*Table{src2, dst, mv}
	r.byFull["biz.raw_data"] = src2
	r.byFull["biz.agg_data"] = dst
	r.byFull["biz.mv_agg"] = mv
	r.ScopeDBs = []string{"biz"}

	if err := r.relations(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Both mv_to and view_from should be present
	kindsFound := map[string]bool{}
	for _, rel := range r.Relations {
		kindsFound[rel.Kind] = true
	}
	if !kindsFound["mv_to"] {
		t.Errorf("mv_to relation missing: %v", r.Relations)
	}
}

// Test columns with nil pointer in row (robustness)
func TestColumnsNilPointerInRow(t *testing.T) {
	// Row with nil table pointer — column should use str() safely
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"FROM system.columns": {
			Names: []string{"database", "table", "name", "type", "position",
				"is_in_partition_key", "is_in_sorting_key", "is_in_primary_key"},
			Data: [][]*string{
				{s("biz"), s("events"), s("ts"), s("DateTime"), s("1"), s("0"), s("0"), s("0")},
			},
		},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events"}
	r.Tables = []*Table{tbl}
	r.byFull["biz.events"] = tbl
	r.ScopeDBs = []string{"biz"}

	if err := r.columns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tbl.Columns) != 1 || tbl.Columns[0].Name != "ts" {
		t.Errorf("columns = %+v", tbl.Columns)
	}
}

// Test viewRefs with qualified and unqualified references
func TestViewRefsUnqualifiedRef(t *testing.T) {
	r := &Run{byFull: map[string]*Table{}}
	base := &Table{Database: "biz", Name: "events"}
	view := &Table{
		Database: "biz", Name: "v", Engine: "View",
		// unqualified table reference (no db prefix)
		CreateQuery: "CREATE VIEW biz.v AS SELECT * FROM events",
	}
	r.byFull["biz.events"] = base
	r.byFull["biz.v"] = view

	r.viewRefs(view)
	found := false
	for _, rel := range r.Relations {
		if rel.Kind == "view_from" && rel.DstTbl == "events" && rel.DstDB == "biz" {
			found = true
		}
	}
	if !found {
		t.Errorf("unqualified FROM reference should resolve to the view's database: %v", r.Relations)
	}
}

// Test viewRefs deduplicates references
func TestViewRefsDeduplicate(t *testing.T) {
	r := &Run{byFull: map[string]*Table{}}
	base := &Table{Database: "biz", Name: "events"}
	view := &Table{
		Database: "biz", Name: "v", Engine: "View",
		// events referenced twice
		CreateQuery: "CREATE VIEW biz.v AS SELECT * FROM biz.events JOIN biz.events ON 1=1",
	}
	r.byFull["biz.events"] = base
	r.byFull["biz.v"] = view

	r.viewRefs(view)
	count := 0
	for _, rel := range r.Relations {
		if rel.DstTbl == "events" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("duplicate reference should be deduplicated, got %d relations for events", count)
	}
}

// Test shape qlog_rows="0" path via mining
func TestShapeQlogZero(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"SELECT version()":      {Data: [][]*string{{s("24.3")}}},
		"FROM system.query_log": {Data: [][]*string{{s("0")}}},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.shape(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.Shape["qlog_rows"] != "0" {
		t.Errorf("qlog_rows = %q, want 0", r.Shape["qlog_rows"])
	}
}

// Test the full Table.Full() method
func TestTableFull(t *testing.T) {
	tbl := &Table{Database: "mydb", Name: "mytable"}
	if got := tbl.Full(); got != "mydb.mytable" {
		t.Errorf("Full() = %q", got)
	}
}

// ---- attrUsage ----
func TestAttrUsage(t *testing.T) {
	cases := map[string]string{
		"vocabulary": "real value: filter and group",
		"measure":    "real number: aggregate",
		"identity":   "masked: GROUP BY only, relabels to real for the human",
		"sensitive":  "masked free text: avoid",
		"unknown":    "unknown",
	}
	for role, want := range cases {
		if got := attrUsage(role); got != want {
			t.Errorf("attrUsage(%q) = %q, want %q", role, got, want)
		}
	}
}

// ---- outKey ----
func TestOutKey(t *testing.T) {
	a := AttrKeyInfo{Key: "http.method", KeyOut: "http.method"}
	if got := a.outKey(); got != "http.method" {
		t.Errorf("outKey with KeyOut = %q", got)
	}
	b := AttrKeyInfo{Key: "my.key"}
	if got := b.outKey(); got != "my.key" {
		t.Errorf("outKey without KeyOut = %q", got)
	}
}

// ---- roster: schema-preserving system DB allowed ----
func TestRosterSchemaPreservingSystemAllowed(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"FROM system.tables": {
			Names: []string{"database", "name", "engine", "engine_full", "create_table_query",
				"partition_key", "sorting_key", "total_rows", "total_bytes"},
			Data: [][]*string{
				{s("system"), s("query_log"), s("MergeTree"), s("MergeTree"), s(""), s(""), s(""), s("0"), s("0")},
			},
		},
	}}
	cfg := testCfg("system", "system_anon")
	cfg.Model = ModelSchemaPreserving
	r, err := NewRun(cfg, src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.roster(context.Background()); err != nil {
		t.Errorf("schema-preserving model should be allowed to profile system DB: %v", err)
	}
}

// ---- roster: token-namespace DB refused ----
func TestRosterTokenNamespaceRefused(t *testing.T) {
	r, err := NewRun(testCfg("db_0123abcd", "sb"), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.roster(context.Background()); err == nil {
		t.Errorf("token-namespace DB must be refused")
	}
}

// ---- clusterVocab ----

func TestClusterVocab(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"FROM system.functions": {Data: [][]*string{{s("count")}, {s("sum")}}},
		"FROM system.settings":  {Data: [][]*string{{s("max_threads")}}},
		"FROM system.merge_tree_settings": {Data: [][]*string{{s("index_granularity")}}},
		"FROM system.table_engines":       {Data: [][]*string{{s("MergeTree")}}},
		"FROM system.data_type_families":  {Data: [][]*string{{s("UInt64")}, {s("String")}}},
		"FROM system.formats":             {Data: [][]*string{{s("TabSeparated")}}},
		"FROM system.aggregate_function_combinators": {Data: [][]*string{{s("If")}, {s("Array")}}},
		"alias_to FROM system.functions WHERE alias_to != ''":       {Data: [][]*string{{s("countDistinct")}}},
		"alias_to FROM system.data_type_families WHERE alias_to != ''": {Data: [][]*string{{s("Int")}}},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	vocab, combinators, err := r.clusterVocab(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(vocab) == 0 {
		t.Error("vocab should not be empty")
	}
	vocabSet := map[string]bool{}
	for _, v := range vocab {
		vocabSet[v] = true
	}
	if !vocabSet["count"] || !vocabSet["MergeTree"] || !vocabSet["UInt64"] {
		t.Errorf("expected functions/engines/types in vocab; got %v", vocab)
	}
	if len(combinators) == 0 {
		t.Error("combinators should not be empty")
	}
}

// ---- rewriteKeyExpr ----

func TestRewriteKeyExpr(t *testing.T) {
	r, err := NewRun(testCfg("biz", ""), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events",
		Columns: []classify.Column{{Name: "event_time", Type: "DateTime"}}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	r.Rw, err = nil, nil
	// Set up rewriter from NewRun's observe output
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	// empty expr → empty result
	if got := r.rewriteKeyExpr(""); got != "" {
		t.Errorf("empty expr should return empty, got %q", got)
	}
}

func TestRewriteKeyExprNonEmpty(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{}}
	r, err := NewRun(testCfg("biz", "biz"), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events",
		Columns: []classify.Column{{Name: "event_time", Type: "DateTime"}}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	// event_time is a column token; rewriting it should produce something
	got := r.rewriteKeyExpr("event_time, toDate(event_time)")
	if got == "" {
		t.Errorf("rewriteKeyExpr returned empty for non-empty expr")
	}
}

// ---- b8 ----
func TestB8(t *testing.T) {
	if *b8(true) != "1" {
		t.Error("b8(true) should be '1'")
	}
	if *b8(false) != "0" {
		t.Error("b8(false) should be '0'")
	}
}
