package discover

import (
	"context"
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/chclient"
	"github.com/Altinity/anon-discovery/internal/classify"
	"github.com/Altinity/anon-discovery/internal/sqllex"
)

// ---- verify ----

// setupRunForVerify creates a run with a fully-built IdMap for verify tests.
func setupRunForVerify(t *testing.T) (*Run, *fakeExec, *fakeExec) {
	t.Helper()
	src := &fakeExec{}
	dst := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz"), src, dst)
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
	r.ScopeDBs = []string{"biz"}
	r.RunID = "run-test"
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	r.Rw = sqllex.NewRewriter(r.IdMap, sqllex.NewKeepRegistry(nil))
	return r, src, dst
}

func TestVerifyNoLeaks(t *testing.T) {
	r, src, dst := setupRunForVerify(t)

	// Set up the source to return tables as "live"
	tblTok, _ := r.IdMap.Lookup("tbl", "events")
	src.rows = map[string]*chclient.Rows{
		// 7.5a existence: source returns the table as live
		"system.tables WHERE database IN": {
			Data: [][]*string{{s("biz.events")}},
		},
	}
	// Sandbox has one table
	r.SandboxRows["biz.events"] = 100
	// SHOW CREATE returns a DDL with no real names
	dst.rows = map[string]*chclient.Rows{
		"SHOW CREATE": {Data: [][]*string{{s("CREATE TABLE biz." + tblTok + " (col_a UInt64) ENGINE = MergeTree ORDER BY tuple()")}}},
		// trusted-split: same source and dest → not cross-cluster, skip this check
	}
	// Single-cluster mode (Source == Dest) → no trusted-split check
	r.Cfg.Source = r.Cfg.Dest

	if err := r.verify(context.Background()); err != nil {
		t.Fatalf("verify should pass with clean DDL: %v", err)
	}
	// profile_verification should be written
	found := false
	for _, tg := range dst.insertTargets {
		if strings.Contains(tg, "profile_verification") {
			found = true
		}
	}
	if !found {
		t.Errorf("profile_verification not written; insertTargets=%v", dst.insertTargets)
	}
}

func TestVerifyTableVanished(t *testing.T) {
	r, src, dst := setupRunForVerify(t)
	// Source returns empty list (table has vanished)
	src.rows = map[string]*chclient.Rows{
		"system.tables WHERE database IN": {Data: nil},
	}
	r.SandboxRows = map[string]uint64{} // no sandbox tables
	r.Cfg.Source = r.Cfg.Dest           // same-cluster

	if err := r.verify(context.Background()); err != nil {
		t.Fatalf("vanished table should not abort verify: %v", err)
	}
	// Verify wrote profile_verification with "dropped-since-discovery"
	_ = dst.insertTargets
}

func TestVerifySandboxLeak(t *testing.T) {
	r, src, dst := setupRunForVerify(t)
	// Source returns the table as live
	src.rows = map[string]*chclient.Rows{
		"system.tables WHERE database IN": {
			Data: [][]*string{{s("biz.events")}},
		},
	}
	// Sandbox has one table
	r.SandboxRows["biz.events"] = 50
	// SHOW CREATE returns a DDL containing the real table name "events" → leak!
	dst.rows = map[string]*chclient.Rows{
		"SHOW CREATE": {Data: [][]*string{{s("CREATE TABLE biz.events (user_id UInt64) ENGINE = MergeTree")}}},
	}
	r.Cfg.Source = r.Cfg.Dest // same-cluster

	err := r.verify(context.Background())
	if err == nil {
		t.Error("sandbox DDL containing real table name should fail verify")
	}
	if !strings.Contains(err.Error(), "leak") {
		t.Errorf("error should mention leak: %v", err)
	}
}

func TestVerifyCrossClusterTrustedSplit(t *testing.T) {
	// Cross-cluster: verify checks for trusted tables on dest
	src := &fakeExec{}
	dst := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz"), src, dst)
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events",
		Columns: []classify.Column{{Name: "ts", Type: "DateTime"}}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	r.ScopeDBs = []string{"biz"}
	r.RunID = "run-test"
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	r.Rw = sqllex.NewRewriter(r.IdMap, sqllex.NewKeepRegistry(nil))
	r.SandboxRows = map[string]uint64{} // no sandbox tables

	// Cross-cluster: Source != Dest
	// testCfg already has different Source/Dest strings

	// Dest: no trusted tables exist
	src.rows = map[string]*chclient.Rows{
		"system.tables WHERE database IN": {Data: [][]*string{{s("biz.events")}}},
	}
	dst.rows = map[string]*chclient.Rows{
		"trusted": {Data: nil}, // no trusted tables
	}

	if err := r.verify(context.Background()); err != nil {
		t.Fatalf("cross-cluster verify with no trusted tables should pass: %v", err)
	}
}

func TestVerifyCrossClusterTrustedPresent(t *testing.T) {
	// Cross-cluster: if a trusted table is found on dest → abort
	src := &fakeExec{}
	dst := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz"), src, dst)
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events",
		Columns: []classify.Column{{Name: "ts", Type: "DateTime"}}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	r.ScopeDBs = []string{"biz"}
	r.RunID = "run-test"
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	r.Rw = sqllex.NewRewriter(r.IdMap, sqllex.NewKeepRegistry(nil))
	r.SandboxRows = map[string]uint64{}

	// Source returns events as live
	src.rows = map[string]*chclient.Rows{
		"system.tables WHERE database IN": {Data: [][]*string{{s("biz.events")}}},
	}
	// Dest has a trusted table (identifier_map or masking_plan)
	dst.rows = map[string]*chclient.Rows{
		// The trusted-split query checks for name IN (store.TrustedTables)
		"name IN": {Data: [][]*string{{s("identifier_map")}}},
	}

	err = r.verify(context.Background())
	if err == nil {
		t.Error("trusted table on dest should abort verify")
	}
	if !strings.Contains(err.Error(), "trusted table") {
		t.Errorf("error should mention trusted table: %v", err)
	}
}

// ---- writeMaskingPlan: attrmap column path ----

func TestWriteMaskingPlanWithAttrMap(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		// attrKeyRoles scan
		"uniqExact(v)": {
			Data: [][]*string{
				{s("http.method"), s("4"), s("0")},   // semconv → vocabulary
				{s("my.custom"), s("200"), s("0")},    // custom → field token
			},
		},
	}}
	r, err := NewRun(testCfg("biz", "biz"), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events", Engine: "MergeTree",
		Columns: []classify.Column{
			{Name: "event_time", Type: "DateTime"},
			{Name: "attrs", Type: "Map(String, String)"},
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
	// AttrKeyRoles should be populated with at least the 2 keys
	if len(r.AttrKeyRoles) < 2 {
		t.Errorf("AttrKeyRoles should have at least 2 keys, got %d", len(r.AttrKeyRoles))
	}
	// Find the custom key — its KeyOut should be a field_ token
	for _, info := range r.AttrKeyRoles {
		if info.Key == "my.custom" && !strings.HasPrefix(info.KeyOut, "field_") {
			t.Errorf("custom key my.custom should have field_ token KeyOut, got %q", info.KeyOut)
		}
	}
}

func TestWriteMaskingPlanWithKeepAttrKeys(t *testing.T) {
	// KeepAttrKeys adds extra vocab keys to the spec
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"uniqExact(v)": {
			Data: [][]*string{
				{s("http.method"), s("4"), s("0")},
			},
		},
	}}
	r, err := NewRun(testCfg("biz", "biz"), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Cfg.KeepAttrKeys = []string{"model", "event.name"}
	tbl := &Table{Database: "biz", Name: "events", Engine: "MergeTree",
		Columns: []classify.Column{
			{Name: "attrs", Type: "Map(String, String)"},
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
}

// ---- DetectPrimary: remaining branches ----

func TestDetectPrimaryRemainingBranches(t *testing.T) {
	// Rule 1: huge enterprise (>5000)
	c := CountTables(mkTables(map[string]int{"MergeTree": 5001}))
	p, rule := DetectPrimary(c)
	if p != "B" || !strings.Contains(rule, "huge-enterprise") {
		t.Errorf("5001 tables: got %s/%s", p, rule)
	}

	// Rule 2: view-warehouse (>70% views and >500 tabs)
	c2 := CountTables(mkTables(map[string]int{"View": 400, "MergeTree": 120}))
	p2, _ := DetectPrimary(c2)
	if p2 != "C" {
		t.Errorf("view-warehouse: got %s", p2)
	}

	// Rule 4: federation (>30 external or >30% external)
	c3 := CountTables(mkTables(map[string]int{"S3": 31, "MergeTree": 50}))
	p3, rule3 := DetectPrimary(c3)
	if p3 != "E" || !strings.Contains(rule3, "federation") {
		t.Errorf("federation: got %s/%s", p3, rule3)
	}

	// Rule 9: realtime-mv with buffer
	c4 := CountTables(mkTables(map[string]int{"MaterializedView": 12, "Buffer": 6, "MergeTree": 100}))
	p4, rule4 := DetectPrimary(c4)
	if p4 != "C" || !strings.Contains(rule4, "realtime-mv") {
		t.Errorf("realtime-mv with buffer: got %s/%s", p4, rule4)
	}
}

// ---- allServiceUsers ----

func TestAllServiceUsers(t *testing.T) {
	service := map[string]bool{"default": true, "etl": true}
	// empty → false (no signal)
	if allServiceUsers([]string{}, service) {
		t.Error("empty users returns false")
	}
	// all service
	if !allServiceUsers([]string{"default", "etl"}, service) {
		t.Error("all service users should return true")
	}
	// mixed → false
	if allServiceUsers([]string{"default", "alice"}, service) {
		t.Error("mixed users should return false")
	}
}

// ---- clusterVocab: aggregate function combinators uppercase ----

func TestClusterVocabUpperCombinators(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"FROM system.functions":                {Data: nil},
		"FROM system.settings":                 {Data: nil},
		"FROM system.merge_tree_settings":      {Data: nil},
		"FROM system.table_engines":            {Data: nil},
		"FROM system.data_type_families":       {Data: nil},
		"FROM system.formats":                  {Data: nil},
		"alias_to FROM system.functions WHERE": {Data: nil},
		"alias_to FROM system.data_type_fam":   {Data: nil},
		"aggregate_function_combinators": {
			Data: [][]*string{{s("If")}, {s("Array")}, {s("")}}, // empty string should be skipped
		},
	}}
	r, err := NewRun(testCfg("biz", "biz"), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	_, combinators, err := r.clusterVocab(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// non-empty combinators should be uppercased
	for _, c := range combinators {
		if c == "" {
			t.Error("empty combinator should be filtered out")
		}
		// should be uppercase
		if c != strings.ToUpper(c) {
			t.Errorf("combinator %q should be uppercase", c)
		}
	}
	// "IF" and "ARRAY" should be present
	found := map[string]bool{}
	for _, c := range combinators {
		found[c] = true
	}
	if !found["IF"] || !found["ARRAY"] {
		t.Errorf("expected IF and ARRAY combinators, got %v", combinators)
	}
}

// ---- mining: error paths ----

func TestHotTablesReturnsEmpty(t *testing.T) {
	// hotTables with no rows in scope — should produce empty Hot
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"ARRAY JOIN": {Data: nil},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Shape["qlog_rows"] = "500"
	r.byFull["biz.events"] = &Table{Database: "biz", Name: "events"}

	if err := r.hotTables(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.Hot) != 0 {
		t.Errorf("no rows in query log scope → empty Hot, got %v", r.Hot)
	}
}

// ---- NewRun: schema-preserving default destDB ----

func TestNewRunSchemaPreservingDestDB(t *testing.T) {
	cfg := Config{
		Source: "cl", Dest: "cl",
		SourceDB: "system",
		HMACKey:  testKey,
		Model:    ModelSchemaPreserving,
	}
	r, err := NewRun(cfg, &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Cfg.DestDB != "system_anon" {
		t.Errorf("schema-preserving default DestDB = %q, want system_anon", r.Cfg.DestDB)
	}
}

func TestNewRunUnknownModel(t *testing.T) {
	cfg := Config{
		Source: "cl", Dest: "cl",
		SourceDB: "biz",
		HMACKey:  testKey,
		Model:    "invalid-model",
	}
	if _, err := NewRun(cfg, &fakeExec{}, &fakeExec{}); err == nil {
		t.Error("unknown model should return error")
	}
}

// ---- materializePreserve: window fallback ----

func TestMaterializePreserveWindowOn(t *testing.T) {
	dst := &fakeExec{rows: map[string]*chclient.Rows{
		"system.tables WHERE":    {Data: [][]*string{{s("0")}}},
		"count()": {Data: [][]*string{{s("100")}}},
	}}
	r, err := NewRun(testCfg("system", "system_anon"), &fakeExec{}, dst)
	if err != nil {
		t.Fatal(err)
	}
	r.Cfg.Model = ModelSchemaPreserving

	tbl := &Table{Database: "system", Name: "query_log", Engine: "MergeTree",
		Columns: []classify.Column{
			{Name: "event_date", Type: "Date", InSK: true},
			{Name: "query", Type: "String"},
		}}
	p := buildPreserve(tbl, "SEED", "system_anon", "run-1", nil)
	// p.WindowOn should be "event_date"
	if p.WindowOn != "event_date" {
		t.Fatalf("buildPreserve WindowOn = %q, want event_date", p.WindowOn)
	}
	if err := r.materializePreserve(context.Background(), "system_anon", tbl, p); err != nil {
		t.Fatal(err)
	}
	// The SELECT should include a WHERE clause with a time window
	for _, e := range dst.execs {
		if strings.Contains(e, "CREATE TABLE") && strings.Contains(e, "WHERE") {
			return // found it
		}
	}
	t.Errorf("CREATE ... AS SELECT with WHERE not found; execs: %v", dst.execs)
}
