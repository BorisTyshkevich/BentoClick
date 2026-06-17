package discover

import (
	"context"
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/chclient"
)

// ---- dominant ----

func TestDominant(t *testing.T) {
	if got := dominant(8, 10, "win", "lose"); got != "win" {
		t.Errorf("dominant(8,10) = %q, want win", got)
	}
	if got := dominant(4, 10, "win", "lose"); got != "lose" {
		t.Errorf("dominant(4,10) = %q, want lose", got)
	}
	if got := dominant(0, 0, "win", "lose"); got != "no-signal" {
		t.Errorf("dominant(0,0) = %q, want no-signal", got)
	}
	if got := dominant(5, 10, "win", "lose"); got != "lose" {
		t.Errorf("dominant(5,10) boundary = %q, want lose (>0.5 not >=0.5)", got)
	}
	if got := dominant(6, 10, "win", "lose"); got != "win" {
		t.Errorf("dominant(6,10) = %q, want win", got)
	}
}

// ---- mining: catalog-only mode (qlog unavailable) ----

func TestMiningCatalogOnly(t *testing.T) {
	r, err := NewRun(testCfg("biz", ""), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Shape["qlog"] = "unavailable"
	if err := r.mining(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.Notes) == 0 {
		t.Error("mining must record a note in catalog-only mode")
	}
	found := false
	for _, n := range r.Notes {
		if strings.Contains(n, "catalog-only") {
			found = true
		}
	}
	if !found {
		t.Errorf("catalog-only note missing: %v", r.Notes)
	}
}

func TestMiningQlogZeroRows(t *testing.T) {
	r, err := NewRun(testCfg("biz", ""), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Shape["qlog_rows"] = "0"
	if err := r.mining(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.Hot) != 0 {
		t.Error("no hot tables expected when qlog_rows=0")
	}
}

// ---- hotTables ----

func TestHotTablesBasic(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		// hotTables query uses ARRAY JOIN (unique to hotTables)
		"ARRAY JOIN": {
			Data: [][]*string{
				{s("biz.events"), s("1000"), s("50000"), s("900"), s("100"), s("['alice','bob']")},
				{s("biz.unknown"), s("500"), s("10000"), s("400"), s("100"), s("['alice']")}, // not in scope
			},
		},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.byFull["biz.events"] = &Table{Database: "biz", Name: "events"}
	r.Shape["qlog_rows"] = "1000"

	if err := r.hotTables(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.Hot) != 1 {
		t.Fatalf("want 1 hot table (scoped), got %d: %v", len(r.Hot), r.Hot)
	}
	h := r.Hot[0]
	if h.Full != "biz.events" || h.Execs != 1000 || h.Sels != 900 || h.Ins != 100 {
		t.Errorf("hot table = %+v", h)
	}
	if len(h.Users) != 2 || h.Users[0] != "alice" || h.Users[1] != "bob" {
		t.Errorf("users = %v", h.Users)
	}
}

func TestHotTablesQueryScoped(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"ARRAY JOIN": {Data: nil},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Shape["qlog_rows"] = "100"
	if err := r.hotTables(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Verify the query contains the expected window/filter patterns
	found := false
	for _, q := range src.queries {
		if strings.Contains(q, "system.query_log") || strings.Contains(q, "from system.query_log") {
			found = true
			if !strings.Contains(q, "QueryFinish") {
				t.Errorf("hotTables query missing QueryFinish filter: %q", q)
			}
		}
	}
	if !found {
		t.Errorf("no query_log query found: %v", src.queries)
	}
}

// ---- hotColumns ----

func TestHotColumnsEmpty(t *testing.T) {
	r, err := NewRun(testCfg("biz", ""), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	// Hot is empty → hotColumns should return nil without querying
	src := &fakeExec{}
	r.SrcEx = src
	if err := r.hotColumns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(src.queries) != 0 {
		t.Errorf("hotColumns with empty Hot should not query, but did: %v", src.queries)
	}
}

func TestHotColumnsBasic(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		// hotColumns query contains "arrayJoin(columns)"
		"arrayJoin(columns)": {
			Data: [][]*string{
				{s("biz.events"), s("biz.events.event_time"), s("500")},
				{s("biz.events"), s("biz.events.user_id"), s("300")},
			},
		},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Hot = []*HotTable{{Full: "biz.events"}}
	r.Shape["qlog_rows"] = "1000"

	if err := r.hotColumns(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.HotCols) != 2 {
		t.Fatalf("want 2 hot columns, got %d", len(r.HotCols))
	}
	if r.HotCols[0].Column != "event_time" {
		t.Errorf("hotCols[0].Column = %q, want event_time", r.HotCols[0].Column)
	}
	if r.HotCols[1].Column != "user_id" {
		t.Errorf("hotCols[1].Column = %q, want user_id", r.HotCols[1].Column)
	}
}

// ---- repQueries ----

func TestRepQueriesBasic(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		// repQueries query contains "normalized_query_hash"
		"normalized_query_hash": {
			Data: [][]*string{
				{s("12345"), s("SELECT ? FROM events WHERE ? = ?"), s("200")},
			},
		},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Hot = []*HotTable{{Full: "biz.events", Execs: 200}}

	if err := r.repQueries(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.RepQueries) != 1 {
		t.Fatalf("want 1 rep query, got %d", len(r.RepQueries))
	}
	if r.RepQueries[0].Hash != "12345" || r.RepQueries[0].Execs != 200 {
		t.Errorf("repQuery = %+v", r.RepQueries[0])
	}
}

func TestRepQueriesTop15Cap(t *testing.T) {
	// repQueries only queries the top 15 hot tables
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"normalized_query_hash": {Data: nil},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	// Create 20 hot tables
	for i := 0; i < 20; i++ {
		r.Hot = append(r.Hot, &HotTable{Full: "biz.events"})
	}
	if err := r.repQueries(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Should only query 15 tables (one query per hot table)
	if len(src.queries) > 15 {
		t.Errorf("repQueries should cap at top 15, got %d queries", len(src.queries))
	}
}

// ---- formRatios ----

func TestFormRatiosBasic(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		// formRatios query contains "uses_prewhere"
		"uses_prewhere": {
			Data: [][]*string{
				// uses_prewhere, uses_where_only, uses_final, uses_argmax, uses_anylast,
				// uses_todate, uses_tostartofday, uses_select_star, total
				{s("80"), s("10"), s("5"), s("60"), s("30"), s("70"), s("20"), s("15"), s("100")},
			},
		},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Hot = []*HotTable{{Full: "biz.events"}}

	if err := r.formRatios(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.Conventions) == 0 {
		t.Fatal("expected conventions to be non-empty")
	}
	// prewhere dominant (80 > 50% of 90)
	var prewhere *Convention
	for i, cv := range r.Conventions {
		if cv.Metric == "prewhere" {
			prewhere = &r.Conventions[i]
		}
	}
	if prewhere == nil {
		t.Fatal("prewhere convention missing")
	}
	if prewhere.Convention != "prewhere-idiom" {
		t.Errorf("prewhere convention = %q, want prewhere-idiom", prewhere.Convention)
	}
}

func TestFormRatiosEmpty(t *testing.T) {
	r, err := NewRun(testCfg("biz", ""), &fakeExec{}, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	// No hot tables → formRatios should no-op
	if err := r.formRatios(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.Conventions) != 0 {
		t.Errorf("expected no conventions, got %v", r.Conventions)
	}
}

func TestFormRatiosNoDataRows(t *testing.T) {
	// Query returns no rows — formRatios should return nil gracefully
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"uses_prewhere": {Data: nil},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Hot = []*HotTable{{Full: "biz.events"}}
	if err := r.formRatios(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.Conventions) != 0 {
		t.Errorf("no rows → no conventions, got %v", r.Conventions)
	}
}

func TestFormRatiosSelectStar(t *testing.T) {
	// select_star = 10, total = 100 → >5% → wide-projections-present
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"uses_prewhere": {
			Data: [][]*string{
				{s("0"), s("0"), s("0"), s("0"), s("0"), s("0"), s("0"), s("10"), s("100")},
			},
		},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Hot = []*HotTable{{Full: "biz.events"}}

	if err := r.formRatios(context.Background()); err != nil {
		t.Fatal(err)
	}
	var starConv *Convention
	for i, cv := range r.Conventions {
		if cv.Metric == "select_star" {
			starConv = &r.Conventions[i]
		}
	}
	if starConv == nil {
		t.Fatal("select_star convention missing")
	}
	if starConv.Convention != "wide-projections-present" {
		t.Errorf("select_star convention = %q, want wide-projections-present", starConv.Convention)
	}
}

func TestFormRatiosNoSignal(t *testing.T) {
	// All zeros → no-signal for prewhere, anylast_vs_argmax, todate_vs_tostartofday
	src := &fakeExec{rows: map[string]*chclient.Rows{
		"uses_prewhere": {
			Data: [][]*string{
				{s("0"), s("0"), s("0"), s("0"), s("0"), s("0"), s("0"), s("0"), s("0")},
			},
		},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.Hot = []*HotTable{{Full: "biz.events"}}

	if err := r.formRatios(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, cv := range r.Conventions {
		if cv.Metric == "prewhere" && cv.Convention != "no-signal" {
			t.Errorf("all-zero prewhere should be no-signal, got %q", cv.Convention)
		}
	}
}

// ---- mining: full pipeline ----

func TestMiningFullPipeline(t *testing.T) {
	src := &fakeExec{rows: map[string]*chclient.Rows{
		// hotTables query uses ARRAY JOIN (unique to that query)
		"ARRAY JOIN": {
			Data: [][]*string{
				{s("biz.events"), s("500"), s("20000"), s("450"), s("50"), s("['alice']")},
			},
		},
		// hotColumns query uses arrayJoin(columns)
		"arrayJoin(columns)": {
			Data: [][]*string{
				{s("biz.events"), s("biz.events.event_time"), s("200")},
			},
		},
		// repQueries uses normalized_query_hash
		"normalized_query_hash": {
			Data: [][]*string{
				{s("99"), s("SELECT ? FROM biz.events"), s("50")},
			},
		},
		// formRatios uses uses_prewhere
		"uses_prewhere": {
			Data: [][]*string{
				{s("0"), s("0"), s("0"), s("0"), s("0"), s("0"), s("0"), s("0"), s("0")},
			},
		},
	}}
	r, err := NewRun(testCfg("biz", ""), src, &fakeExec{})
	if err != nil {
		t.Fatal(err)
	}
	r.byFull["biz.events"] = &Table{Database: "biz", Name: "events"}
	r.Shape["qlog_rows"] = "500"

	if err := r.mining(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.Hot) != 1 {
		t.Errorf("want 1 hot table, got %d", len(r.Hot))
	}
}

// ---- mostlyServiceUsers ----

func TestMostlyServiceUsers(t *testing.T) {
	service := map[string]bool{"default": true, "etl": true}
	if mostlyServiceUsers([]string{}, service, 0.9) {
		t.Error("empty users should return false")
	}
	if !mostlyServiceUsers([]string{"default", "etl", "default"}, service, 0.9) {
		t.Error("all service users should return true for fraction 0.9")
	}
	if mostlyServiceUsers([]string{"default", "alice", "bob"}, service, 0.9) {
		t.Error("1/3 service users should not meet 0.9 fraction")
	}
	if !mostlyServiceUsers([]string{"default", "alice"}, service, 0.5) {
		t.Error("1/2 service users should meet 0.5 fraction")
	}
}

// ---- shortName ----

func TestShortName(t *testing.T) {
	if got := shortName("biz.events"); got != "events" {
		t.Errorf("shortName(biz.events) = %q", got)
	}
	if got := shortName("nodb"); got != "nodb" {
		t.Errorf("shortName(nodb) = %q", got)
	}
	if got := shortName(""); got != "" {
		t.Errorf("shortName('') = %q", got)
	}
}

// ---- demote: archetype B huge enterprise (mostlyServiceUsers path) ----

func TestDemoteArchetypeBHuge(t *testing.T) {
	// Archetype B with >5000 biz tabs triggers the mostlyServiceUsers (90%) path
	hot := []*HotTable{
		// 5 users, 5 are service users (100% ≥ 90%)
		{Full: "biz.bigfact", Execs: 100, Sels: 100, Ins: 0,
			Users: []string{"default", "airflow_prod", "bot_x", "monitor1", "oncall_y"}},
		// 5 users, 3 are service (60% < 90%) → must stay hot
		{Full: "biz.mixfact", Execs: 100, Sels: 100, Ins: 0,
			Users: []string{"alice", "bob", "default", "airflow_prod", "carol"}},
	}
	r := mkRun(hot, map[string]string{
		"biz.bigfact": "MergeTree",
		"biz.mixfact": "MergeTree",
	})
	r.Cfg.ServiceUsers = DefaultServiceUsers
	r.demote("B", 6000)
	for _, h := range hot {
		if h.Full == "biz.bigfact" && !h.Demoted {
			t.Error("bigfact: 5 service users (100%) in arch B huge should demote")
		}
		if h.Full == "biz.mixfact" && h.Demoted {
			t.Error("mixfact: 3/5 service users (60%) should not meet 90% threshold")
		}
	}
}

// ---- demote: per-tenant hash pattern ----

func TestDemotePerTenantHash(t *testing.T) {
	hot := []*HotTable{
		{Full: "biz.events_00ab1234_raw", Execs: 100, Sels: 100, Users: []string{"alice"}},
	}
	r := mkRun(hot, map[string]string{"biz.events_00ab1234_raw": "MergeTree"})
	r.demote("A", 5)
	h := hot[0]
	found := false
	for _, flag := range h.ReviewFlags {
		if strings.Contains(flag, "per-tenant-hash-pattern") {
			found = true
		}
	}
	if !found {
		t.Errorf("per-tenant hash should add review flag, got %v", h.ReviewFlags)
	}
}
