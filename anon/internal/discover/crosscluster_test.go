package discover

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/chclient"
	"github.com/Altinity/anon-discovery/internal/classify"
)

// fakeExec satisfies chclient.Executor for wiring tests: Query answers from
// substring-matched canned rows (empty result otherwise), writes are recorded.
type fakeExec struct {
	rows    map[string]*chclient.Rows // SQL substring -> result
	queries []string
	execs   []string
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

func (f *fakeExec) Insert(context.Context, string, []string, [][]*string) error { return nil }

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
