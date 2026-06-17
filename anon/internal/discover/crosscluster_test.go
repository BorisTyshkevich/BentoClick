package discover

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/chclient"
	"github.com/Altinity/anon-discovery/internal/classify"
	"github.com/Altinity/anon-discovery/internal/sqllex"
)

// fakeExec satisfies chclient.Executor for wiring tests: Query answers from
// substring-matched canned rows (empty result otherwise), writes are recorded.
type fakeExec struct {
	rows          map[string]*chclient.Rows // SQL substring -> result
	queries       []string
	execs         []string
	insertTargets []string // db.table each Insert targeted
}

func (f *fakeExec) Query(_ context.Context, sql string) (*chclient.Rows, error) {
	f.queries = append(f.queries, sql)
	for k, v := range f.rows {
		if strings.Contains(sql, k) {
			return v, nil
		}
	}
	return &chclient.Rows{}, nil
}

func (f *fakeExec) Exec(_ context.Context, sql string) error {
	f.execs = append(f.execs, sql)
	return nil
}

func (f *fakeExec) Insert(_ context.Context, target string, _ []string, _ [][]*string) error {
	f.insertTargets = append(f.insertTargets, target)
	return nil
}

func (f *fakeExec) QueryStream(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeExec) InsertStream(_ context.Context, _ string, _ []string, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

var testKey = []byte("0123456789abcdef0123456789abcdef")

func testCfg(srcDB, dstDB string) Config {
	return Config{
		Source: "cl otel", Dest: "clickhouse-client --connection demo",
		SourceDB: srcDB, DestDB: dstDB, HMACKey: testKey,
	}
}

// TestObserveTokenizesCustomAttrKeys guards the observe->build->tok pipeline for
// custom attrmap keys (P3): a non-semconv key must be Observe("field")'d BEFORE
// Build so writeMaskingPlan can mint its field_<hex> token afterwards. Regression
// for the bug where the "field" kind was never observed, so a real tokenizing run
// failed at masking-plan time with "unobserved field identifier".
func TestObserveTokenizesCustomAttrKeys(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		// attrKeyRoles scan: one custom (non-semconv) key + one semconv key.
		"uniqExact(v) AS card": {Data: [][]*string{
			{s("my.custom_key"), s("100"), s("0")}, // custom -> field_ token
			{s("http.method"), s("4"), s("0")},     // semconv -> kept real, no field token
		}},
	}}
	r, err := NewRun(testCfg("biz", "biz_anon"), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Tables = []*Table{{Database: "biz", Name: "events",
		Columns: []classify.Column{{Name: "attrs", Type: "Map(String, String)"}}}}
	if err := r.observe(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.IdMap.Build(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.tok("field", "my.custom_key"); err != nil {
		t.Fatalf("custom attr key not observed for field tokenization: %v", err)
	}
	if _, err := r.tok("field", "http.method"); err == nil {
		t.Error("semconv key should NOT be observed as a field token (kept real)")
	}
}

// TestSchemaGuideWritesBackingDataTables guards the registry write target: the
// LLM-facing bentoclick.schema_guide / attr_guide are VIEWs (read target for the
// MCP's view_regexp tools), so anond must INSERT into the writable *_data backing
// tables. Regression for the bug where the writer targeted the views directly and
// a real tokenizing run died at the final step with "Method write is not supported
// by storage View".
func TestSchemaGuideWritesBackingDataTables(t *testing.T) {
	dst := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz_anon"), &fakeExec{}, dst)
	if err != nil {
		t.Fatal(err)
	}
	tbl := &Table{Database: "biz", Name: "events", Engine: "MergeTree",
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
	// one attrmap key so the attr_guide write path is exercised too
	r.AttrKeyRoles = []AttrKeyInfo{{DB: "biz", Table: "events", Column: "event_time",
		Key: "http.method", Role: "vocabulary", KeyOut: "http.method"}}

	if err := r.writeSchemaGuideTokenizing(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"`bentoclick`.`schema_guide_data`": false, "`bentoclick`.`attr_guide_data`": false}
	for _, target := range dst.insertTargets {
		if target == "`bentoclick`.`schema_guide`" || target == "`bentoclick`.`attr_guide`" {
			t.Errorf("registry write targeted the VIEW %q (not writable); must target the *_data backing table", target)
		}
		if _, ok := want[target]; ok {
			want[target] = true
		}
	}
	for target, seen := range want {
		if !seen {
			t.Errorf("expected an INSERT into %q, got targets %v", target, dst.insertTargets)
		}
	}
}

// TestSecretWritesTargetSecretDB guards the trust boundary: the de-anon secret
// (identifier_map, masking_plan) must be written to the dedicated SecretDB, never
// the MetaDB/RegistryDB that backs the LLM-facing read path. Regression for the
// tokenizing model co-locating the secret in MetaDB (run.go) while the
// schema-preserving model correctly used SecretDB (preserve.go) — see M1.
func TestSecretWritesTargetSecretDB(t *testing.T) {
	src := &fakeExec{}
	r, err := NewRun(testCfg("biz", "biz_anon"), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	// SecretStore must point at SecretDB, not MetaDB (the LLM-facing meta/registry DB).
	if r.SecretStore.MetaDB != r.Cfg.SecretDB {
		t.Fatalf("SecretStore DB = %q, want SecretDB %q", r.SecretStore.MetaDB, r.Cfg.SecretDB)
	}
	if r.Cfg.SecretDB == r.Cfg.MetaDB {
		t.Fatalf("SecretDB %q must differ from MetaDB %q", r.Cfg.SecretDB, r.Cfg.MetaDB)
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
	if _, err := r.writeMaskingPlan(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantTarget := "`" + r.Cfg.SecretDB + "`.`masking_plan`"
	badTarget := "`" + r.Cfg.MetaDB + "`.`masking_plan`"
	var sawWant, sawBad bool
	for _, tg := range src.insertTargets {
		if tg == wantTarget {
			sawWant = true
		}
		if tg == badTarget {
			sawBad = true
		}
	}
	if !sawWant {
		t.Errorf("masking_plan was not written to the SecretDB %q; insert targets: %v", wantTarget, src.insertTargets)
	}
	if sawBad {
		t.Errorf("masking_plan written to the MetaDB %q (de-anon secret leaked into the LLM-facing meta DB)", badTarget)
	}
}

func TestNewRunDefaults(t *testing.T) {
	r, err := NewRun(testCfg("biz", ""), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Cfg.DestDB != "biz" {
		t.Errorf("DestDB default = %q, want source DB", r.Cfg.DestDB)
	}
	if r.Cfg.MetaDB != "bentoclick" || r.Cfg.WindowDays != 7 || r.Cfg.SampleRows != 1_000_000 {
		t.Errorf("defaults: %+v", r.Cfg)
	}
	if _, err := NewRun(testCfg("", ""), &fakeExec{}, &fakeExec{}); err == nil {
		t.Error("empty SourceDB must error")
	}
	if _, err := NewRun(testCfg("biz", "bentoclick"), &fakeExec{}, &fakeExec{}); err == nil {
		t.Error("DestDB == meta DB must error")
	}
}

// TestDisclosureRule: DestDB == SourceDB discloses the DB name (kept
// verbatim, consistent with the sandbox DB); any other dest name keeps it
// tokenized.
func TestDisclosureRule(t *testing.T) {
	for _, tc := range []struct {
		destDB    string
		disclosed bool
	}{
		{"biz", true},
		{"sandbox_biz", false},
	} {
		r, err := NewRun(testCfg("biz", tc.destDB), &fakeExec{}, &fakeExec{})
		if err != nil {
			t.Fatal(err)
		}
		tbl := &Table{Database: "biz", Name: "events", Columns: []classify.Column{
			{Name: "event_time", Type: "DateTime"},
		}}
		r.Tables = []*Table{tbl}
		r.byFull[tbl.Full()] = tbl
		if err := r.observe(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := r.IdMap.Build(); err != nil {
			t.Fatal(err)
		}
		tok, ok := r.IdMap.Lookup("db", "biz")
		if !ok {
			t.Fatalf("destDB=%s: db not in map", tc.destDB)
		}
		wantShape := "tokenized"
		if tc.disclosed {
			wantShape = "disclosed"
			if tok != "biz" {
				t.Errorf("destDB=%s: token %q, want verbatim", tc.destDB, tok)
			}
		} else if !strings.HasPrefix(tok, "db_") || tok == "biz" {
			t.Errorf("destDB=%s: token %q, want tokenized", tc.destDB, tok)
		}
		if r.Shape["db_disclosure"] != wantShape {
			t.Errorf("destDB=%s: db_disclosure=%q, want %q", tc.destDB, r.Shape["db_disclosure"], wantShape)
		}
	}
}

// TestOwnQueryFilterLogComment: the mining filter must exclude anond's own
// tagged queries — cross-cluster masking SELECTs reference only real tables,
// so the token-DB filter alone cannot catch them.
func TestOwnQueryFilterLogComment(t *testing.T) {
	r, err := NewRun(testCfg("biz", ""), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	f := r.ownQueryFilter()
	if !strings.Contains(f, "log_comment != '"+chclient.LogComment+"'") {
		t.Errorf("ownQueryFilter misses the log_comment exclusion: %s", f)
	}
	if !strings.Contains(f, "arrayExists") {
		t.Errorf("ownQueryFilter dropped the token-DB defense-in-depth filter: %s", f)
	}
}

// TestRosterSingleDB: roster scopes to exactly the source DB and fails on
// empty/missing DBs and on refused names.
func TestRosterSingleDB(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"FROM system.tables": {
			Names: []string{"database", "name", "engine", "engine_full", "create_table_query",
				"partition_key", "sorting_key", "total_rows", "total_bytes"},
			Data: [][]*string{
				{s("biz"), s("events"), s("MergeTree"), s("MergeTree"), s("CREATE TABLE biz.events ..."),
					s(""), s("id"), s("100"), s("1000")},
			},
		},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.roster(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.ScopeDBs) != 1 || r.ScopeDBs[0] != "biz" || len(r.Tables) != 1 {
		t.Errorf("scope = %v, tables = %d", r.ScopeDBs, len(r.Tables))
	}
	wantQuery := false
	for _, q := range src.queries {
		if strings.Contains(q, "database = 'biz'") {
			wantQuery = true
		}
	}
	if !wantQuery {
		t.Errorf("roster did not scope the query to the source DB: %v", src.queries)
	}

	r2, _ := NewRun(testCfg("nosuch", ""), &fakeExec{}, &fakeExec{})
	if err := r2.roster(context.Background()); err == nil || !strings.Contains(err.Error(), "does not exist or has no tables") {
		t.Errorf("missing DB: err = %v", err)
	}
	for _, bad := range []string{"system", "altinity", "db_0123abcd"} {
		r3, err := NewRun(testCfg(bad, "sb"), &fakeExec{}, &fakeExec{})
		if err != nil {
			t.Fatal(err)
		}
		if err := r3.roster(context.Background()); err == nil {
			t.Errorf("source DB %q must be refused", bad)
		}
	}
}
