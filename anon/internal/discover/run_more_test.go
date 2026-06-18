package discover

import (
	"context"
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/chclient"
	"github.com/Altinity/anon-discovery/internal/classify"
	"github.com/Altinity/anon-discovery/internal/sqllex"
)

// ---- tokenizeFlag ----

func TestTokenizeFlag(t *testing.T) {
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, &fakeExec{})
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

	// Non-shadow flag: pass through unchanged
	flag, err := r.tokenizeFlag("per-tenant-hash-pattern: summarize once")
	if err != nil {
		t.Fatal(err)
	}
	if flag != "per-tenant-hash-pattern: summarize once" {
		t.Errorf("non-shadow flag should pass through: %q", flag)
	}

	// Shadow-traffic flag with a real db.table reference
	flag2, err := r.tokenizeFlag("shadow-traffic-vs-biz.events: kept hot (execs=600 vs base=1000)")
	if err != nil {
		t.Fatal(err)
	}
	// Should not contain the real table name "events"
	if strings.Contains(flag2, "biz.events") {
		t.Errorf("shadow-traffic flag should have tokens, not real names: %q", flag2)
	}
	if !strings.Contains(flag2, "shadow-traffic-vs-") {
		t.Errorf("shadow-traffic prefix should remain: %q", flag2)
	}
}

func TestTokenizeFlagNoMatch(t *testing.T) {
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, &fakeExec{})
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

	// misleading-staging-name flag: no shadow prefix → pass through
	got, err := r.tokenizeFlag("misleading-staging-name: select-dominated by human users")
	if err != nil {
		t.Fatal(err)
	}
	if got != "misleading-staging-name: select-dominated by human users" {
		t.Errorf("non-matching flag modified: %q", got)
	}
}

// ---- manifest ----

func setupRunForProfile(t *testing.T) *Run {
	t.Helper()
	dst := &fakeExec{}
	src := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz"), src, dst)
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events", Engine: "MergeTree",
		SortingKey: "event_time", TotalRows: 1000,
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
	r.Rw = sqllex.NewRewriter(r.IdMap, sqllex.NewKeepRegistry(nil))
	r.ScopeDBs = []string{"biz"}
	r.RunID = "run-20260101-120000"
	return r
}

func TestManifest(t *testing.T) {
	r := setupRunForProfile(t)
	r.Version = "24.3.5.1"
	r.Shape["db_disclosure"] = "disclosed"

	if err := r.manifest(context.Background()); err != nil {
		t.Fatal(err)
	}
	dst := r.DstEx.(*fakeExec)
	// manifest inserts into bentoclick.manifest
	foundManifest := false
	for _, tg := range dst.insertTargets {
		if strings.Contains(tg, "manifest") {
			foundManifest = true
		}
	}
	if !foundManifest {
		t.Errorf("manifest did not write to manifest table; insertTargets=%v", dst.insertTargets)
	}
}

// ---- writeProfile ----

func TestWriteProfile(t *testing.T) {
	r := setupRunForProfile(t)

	// Add a hot table to exercise workload tables path
	tbl := r.Tables[0]
	// Pre-observe users so the IdMap can mint tokens for them
	r.IdMap.Observe("user", "alice")
	r.IdMap.Observe("user", "bob")
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	r.Hot = []*HotTable{{
		Full: tbl.Full(), Execs: 100, Sels: 90, Ins: 10,
		Users: []string{"alice", "bob"},
	}}
	r.HotCols = []HotColumn{{Full: tbl.Full(), Column: "event_time", Touches: 50}}
	r.RepQueries = []RepQuery{{
		Full: tbl.Full(), Hash: "12345",
		Normalized: "SELECT ? FROM biz.events", Execs: 100,
	}}
	r.Conventions = []Convention{{
		Full: tbl.Full(), Metric: "prewhere",
		Numerator: 80, Denominator: 100, Convention: "prewhere-idiom",
	}}

	if err := r.writeProfile(context.Background()); err != nil {
		t.Fatal(err)
	}
	src := r.SrcEx.(*fakeExec)
	dst := r.DstEx.(*fakeExec)

	// identifier_map must go to src (SecretDB)
	foundIDMap := false
	for _, tg := range src.insertTargets {
		if strings.Contains(tg, "identifier_map") {
			foundIDMap = true
		}
	}
	if !foundIDMap {
		t.Errorf("identifier_map not written to src; insertTargets=%v", src.insertTargets)
	}

	// profile_catalog and profile_columns must go to dst
	foundCat, foundCols := false, false
	for _, tg := range dst.insertTargets {
		if strings.Contains(tg, "profile_catalog") {
			foundCat = true
		}
		if strings.Contains(tg, "profile_columns") {
			foundCols = true
		}
	}
	if !foundCat {
		t.Errorf("profile_catalog not written; insertTargets=%v", dst.insertTargets)
	}
	if !foundCols {
		t.Errorf("profile_columns not written; insertTargets=%v", dst.insertTargets)
	}
}

func TestWriteProfileWithAttrKeys(t *testing.T) {
	// Test the profile_attr_keys path
	r := setupRunForProfile(t)
	tbl := r.Tables[0]
	r.AttrKeyRoles = []AttrKeyInfo{
		{DB: "biz", Table: "events", Column: "user_id", Key: "http.method", Role: "vocabulary", KeyOut: "http.method"},
	}
	if err := r.writeProfile(context.Background()); err != nil {
		t.Fatal(err)
	}
	dst := r.DstEx.(*fakeExec)
	_ = tbl
	foundAttr := false
	for _, tg := range dst.insertTargets {
		if strings.Contains(tg, "profile_attr_keys") {
			foundAttr = true
		}
	}
	if !foundAttr {
		t.Errorf("profile_attr_keys not written when AttrKeyRoles present; insertTargets=%v", dst.insertTargets)
	}
}

func TestWriteProfileWithRelations(t *testing.T) {
	r := setupRunForProfile(t)
	r.Relations = []Relation{
		{Kind: "dist_to", SrcDB: "biz", SrcTbl: "events", DstDB: "biz", DstTbl: "events_local"},
	}
	// Need DstTbl token in map — observe and rebuild
	r.IdMap.Observe("tbl", "events_local")
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}

	if err := r.writeProfile(context.Background()); err != nil {
		t.Fatal(err)
	}
	dst := r.DstEx.(*fakeExec)
	foundRel := false
	for _, tg := range dst.insertTargets {
		if strings.Contains(tg, "profile_relations") {
			foundRel = true
		}
	}
	if !foundRel {
		t.Errorf("profile_relations not written; insertTargets=%v", dst.insertTargets)
	}
}

func TestWriteProfileDemoted(t *testing.T) {
	r := setupRunForProfile(t)
	tbl := r.Tables[0]
	// Demoted table with a review flag
	r.Hot = []*HotTable{{
		Full: tbl.Full(), Execs: 50, Demoted: true,
		DemoteReasons: []string{"insert-dominated"},
		ReviewFlags:   []string{"per-tenant-hash-pattern: summarize"},
	}}
	if err := r.writeProfile(context.Background()); err != nil {
		t.Fatal(err)
	}
	dst := r.DstEx.(*fakeExec)
	// profile_catalog should be written (it includes demote info)
	found := false
	for _, tg := range dst.insertTargets {
		if strings.Contains(tg, "profile_catalog") {
			found = true
		}
	}
	if !found {
		t.Errorf("profile_catalog not found; insertTargets=%v", dst.insertTargets)
	}
}

// ---- archetype share / DetectSecondaries ----

func TestArchetypeShare(t *testing.T) {
	c := Counts{BizTabs: 100, Dist: 20}
	if got := c.share(20); got != 0.2 {
		t.Errorf("share(20) = %v, want 0.2", got)
	}
	c0 := Counts{BizTabs: 0}
	if got := c0.share(5); got != 0.0 {
		t.Errorf("share with zero BizTabs = %v", got)
	}
}

func TestDetectSecondariesVariants(t *testing.T) {
	// Large-B secondary
	c := CountTables(mkTables(map[string]int{"MergeTree": 2600}))
	secs := DetectSecondaries(c, "A")
	hasB := false
	for _, s := range secs {
		if s == "B" {
			hasB = true
		}
	}
	if !hasB {
		t.Errorf("2600 tables should add secondary B; got %v", secs)
	}

	// Large-C: view share > 0.35 and bizTabs > 250
	c2 := CountTables(mkTables(map[string]int{"View": 200, "MergeTree": 300}))
	secs2 := DetectSecondaries(c2, "A")
	hasC := false
	for _, s := range secs2 {
		if s == "C" {
			hasC = true
		}
	}
	if !hasC {
		t.Errorf("big view-warehouse should add secondary C; got %v", secs2)
	}

	// Primary is excluded from secondaries
	c3 := CountTables(mkTables(map[string]int{"Kafka": 8, "MergeTree": 2600}))
	p, _ := DetectPrimary(c3)
	secs3 := DetectSecondaries(c3, p)
	for _, s := range secs3 {
		if s == p {
			t.Errorf("primary %s should not appear in secondaries %v", p, secs3)
		}
	}

	// D secondary: dictShare > 0.015 and mvShare > 0.05
	c4 := CountTables(mkTables(map[string]int{"Dictionary": 3, "MaterializedView": 10, "MergeTree": 150}))
	secs4 := DetectSecondaries(c4, "A")
	hasD := false
	for _, s := range secs4 {
		if s == "D" {
			hasD = true
		}
	}
	if !hasD {
		t.Errorf("dict+mv should add secondary D; got %v", secs4)
	}

	// E: null > 10 and mvShare > 0.05
	c5 := CountTables(mkTables(map[string]int{"Null": 11, "MaterializedView": 10, "MergeTree": 100}))
	secs5 := DetectSecondaries(c5, "A")
	hasE := false
	for _, s := range secs5 {
		if s == "E" {
			hasE = true
		}
	}
	if !hasE {
		t.Errorf("null+mv should add secondary E; got %v", secs5)
	}
}

// ---- writeSchemaGuideTokenizing: attrUsage paths ----

func TestWriteSchemaGuideTokenizingWithAttrRoles(t *testing.T) {
	dst := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, dst)
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

	// Set various attr roles to cover all attrUsage branches
	r.AttrKeyRoles = []AttrKeyInfo{
		{DB: "biz", Table: "events", Column: "attrs", Key: "http.method", Role: "vocabulary", KeyOut: "http.method"},
		{DB: "biz", Table: "events", Column: "attrs", Key: "latency_ms", Role: "measure", KeyOut: "latency_ms"},
		{DB: "biz", Table: "events", Column: "attrs", Key: "session_id", Role: "identity", KeyOut: "session_id"},
		{DB: "biz", Table: "events", Column: "attrs", Key: "notes", Role: "sensitive", KeyOut: "notes"},
	}
	r.SandboxRows["biz.events"] = 100

	if err := r.writeSchemaGuideTokenizing(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Should write to schema_guide_data and attr_guide_data
	foundSchema, foundAttr := false, false
	for _, tg := range dst.insertTargets {
		if strings.Contains(tg, "schema_guide_data") {
			foundSchema = true
		}
		if strings.Contains(tg, "attr_guide_data") {
			foundAttr = true
		}
	}
	if !foundSchema {
		t.Errorf("schema_guide_data not written; targets=%v", dst.insertTargets)
	}
	if !foundAttr {
		t.Errorf("attr_guide_data not written when attr roles exist; targets=%v", dst.insertTargets)
	}
}

// ---- observe: cluster names path ----

func TestObserveClusterNames(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"DISTINCT cluster": {
			Data: [][]*string{{s("cluster1")}, {s("cluster2")}},
		},
	}}
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
	// cluster1 should be in the map under "cluster" kind
	if _, ok := r.IdMap.Lookup("cluster", "cluster1"); !ok {
		t.Error("cluster1 should be in IdMap")
	}
}

// ---- observe: sql wave (CreateQuery, RepQueries) ----

func TestObserveSQLWave(t *testing.T) {
	src := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz"), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{
		Database: "biz", Name: "events",
		CreateQuery: "CREATE TABLE biz.events (event_time DateTime, user_id UInt64) ENGINE = MergeTree ORDER BY event_time",
		SortingKey:  "event_time",
		Columns:     []classify.Column{{Name: "event_time", Type: "DateTime"}, {Name: "user_id", Type: "UInt64"}},
	}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	r.RepQueries = []RepQuery{{
		Full:       tbl.Full(),
		Normalized: "SELECT ? FROM biz.events WHERE user_id = ?",
	}}
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	// The identifier wave should have processed the SQL and added sql tokens
	pairs := r.IdMap.Pairs()
	if len(pairs) == 0 {
		t.Error("observe should produce at least some id-map pairs")
	}
}

// ---- observe: hot users ----

func TestObserveHotUsers(t *testing.T) {
	src := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz"), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events",
		Columns: []classify.Column{{Name: "ts", Type: "DateTime"}}}
	r.Tables = []*Table{tbl}
	r.byFull[tbl.Full()] = tbl
	r.Hot = []*HotTable{{Full: tbl.Full(), Users: []string{"alice", "bob"}}}
	r.HotCols = []HotColumn{{Full: tbl.Full(), Column: "ts"}}
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	// alice and bob should be in map
	if _, ok := r.IdMap.Lookup("user", "alice"); !ok {
		t.Error("alice should be in IdMap as a user")
	}
}

// ---- attrParams: custom PIIKeyPattern ----

func TestAttrParamsCustomPattern(t *testing.T) {
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Cfg.PIIKeyPattern = "mysecret"
	threshold, _, re, err := r.attrParams()
	if err != nil {
		t.Fatal(err)
	}
	if threshold == 0 {
		t.Error("threshold should not be zero")
	}
	if !re.MatchString("mysecret_field") {
		t.Error("custom PIIKeyPattern should match mysecret_field")
	}
}

func TestAttrParamsDefaultThreshold(t *testing.T) {
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	threshold, piiThr, _, err := r.attrParams()
	if err != nil {
		t.Fatal(err)
	}
	if threshold != 64 {
		t.Errorf("default threshold = %d, want 64", threshold)
	}
	if piiThr != 0.5 {
		t.Errorf("default PII-value threshold = %v, want 0.5", piiThr)
	}
}

func TestAttrParamsCustomThreshold(t *testing.T) {
	r, err := NewRun(testCfg("biz", "biz"), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Cfg.AttrCardThreshold = 128
	threshold, _, _, err := r.attrParams()
	if err != nil {
		t.Fatal(err)
	}
	if threshold != 128 {
		t.Errorf("custom threshold = %d, want 128", threshold)
	}
}

// ---- attrRolesFor memoization ----

func TestAttrRolesForMemoized(t *testing.T) {
	callCount := 0
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"uniq(v)": {
			Data: [][]*string{
				{s("http.method"), s("4"), s("0"), s("0")},
			},
		},
	}}
	// Override the query counter
	_ = callCount
	r, err := NewRun(testCfg("biz", "biz"), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Call twice with same args
	info1, err := r.attrRolesFor(ctx, "biz", "events", "attrs", "")
	if err != nil {
		t.Fatal(err)
	}
	info2, err := r.attrRolesFor(ctx, "biz", "events", "attrs", "")
	if err != nil {
		t.Fatal(err)
	}
	// Only 1 query should have been made (second call is memoized)
	if len(src.queries) > 1 {
		t.Errorf("attrRolesFor should memoize; got %d queries", len(src.queries))
	}
	if len(info1) != len(info2) {
		t.Errorf("memoized result differs: %v vs %v", info1, info2)
	}
}

// TestAttrKeyScanQuery (H2 #1/#2): the attr-key scan uses global uniq() + a
// value-PII fraction, windows to the sandbox window when a time column exists,
// and never falls back to the recency-biased LIMIT 200000 prefix.
func TestAttrKeyScanQuery(t *testing.T) {
	src := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz"), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.attrRolesFor(context.Background(), "biz", "events", "attrs", "ts"); err != nil {
		t.Fatal(err)
	}
	if len(src.queries) != 1 {
		t.Fatalf("want 1 scan query, got %d", len(src.queries))
	}
	q := src.queries[0]
	for _, want := range []string{"uniq(v)", "piifrac", "INTERVAL", "`ts` >="} {
		if !strings.Contains(q, want) {
			t.Errorf("scan query missing %q:\n%s", want, q)
		}
	}
	if strings.Contains(q, "LIMIT 200000") || strings.Contains(q, "uniqExact") {
		t.Errorf("scan query still uses the prefix/exact path:\n%s", q)
	}

	// no time column -> full table, no window
	src2 := &fakeExec{}
	r2, _ := NewRun(testCfg("biz", "biz"), src2, &fakeExec{})
	if _, err := r2.attrRolesFor(context.Background(), "biz", "events", "attrs", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(src2.queries[0], "INTERVAL") {
		t.Errorf("no-time-column scan must not window:\n%s", src2.queries[0])
	}
}

// ---- CountTables: all engine branches ----

func TestCountTablesAllBranches(t *testing.T) {
	tables := []*Table{
		{Engine: "Distributed"},
		{Engine: "View"},
		{Engine: "LiveView"},
		{Engine: "MaterializedView"},
		{Engine: "Dictionary"},
		{Engine: "Buffer"},
		{Engine: "Kafka"},
		{Engine: "Null"},
		{Engine: "MergeTree"},
		{Engine: "SummingMergeTree"},
		{Engine: "AggregatingMergeTree"},
		{Engine: "ReplicatedReplacingMergeTree"},
		{Engine: "S3"},
	}
	c := CountTables(tables)
	if c.BizTabs != len(tables) {
		t.Errorf("BizTabs = %d, want %d", c.BizTabs, len(tables))
	}
	if c.Dist != 1 {
		t.Errorf("Dist = %d, want 1", c.Dist)
	}
	if c.View != 2 { // View + LiveView
		t.Errorf("View = %d, want 2", c.View)
	}
	if c.MV != 1 {
		t.Errorf("MV = %d, want 1", c.MV)
	}
	if c.Dict != 1 {
		t.Errorf("Dict = %d, want 1", c.Dict)
	}
	if c.Buffer != 1 {
		t.Errorf("Buffer = %d, want 1", c.Buffer)
	}
	if c.Kafka != 1 {
		t.Errorf("Kafka = %d, want 1", c.Kafka)
	}
	if c.Null != 1 {
		t.Errorf("Null = %d, want 1", c.Null)
	}
	if c.MT != 1 { // only plain MergeTree
		t.Errorf("MT = %d, want 1", c.MT)
	}
	if c.Summ != 1 {
		t.Errorf("Summ = %d, want 1", c.Summ)
	}
	if c.Agg != 1 {
		t.Errorf("Agg = %d, want 1", c.Agg)
	}
	if c.Repl != 1 {
		t.Errorf("Repl = %d, want 1", c.Repl)
	}
	if c.External != 1 { // S3
		t.Errorf("External = %d, want 1", c.External)
	}
}
