package discover

import (
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/classify"
)

func mkTables(engines map[string]int) []*Table {
	var out []*Table
	i := 0
	for engine, n := range engines {
		for j := 0; j < n; j++ {
			out = append(out, &Table{Database: "biz", Name: engine + "_t", Engine: engine})
			i++
		}
	}
	return out
}

func TestArchetypeRules(t *testing.T) {
	cases := []struct {
		name    string
		engines map[string]int
		primary string
		rule    string
	}{
		{"small-simple", map[string]int{"MergeTree": 20}, "A", "rule-10 small-simple"},
		{"kafka-streaming", map[string]int{"Kafka": 16, "MergeTree": 200}, "E", "rule-3 kafka-streaming"},
		{"sharded", map[string]int{"Distributed": 60, "MergeTree": 150}, "B", "rule-8 sharded-plain"},
		{"sharded-replacing", map[string]int{"Distributed": 60, "ReplicatedReplacingMergeTree": 60, "MergeTree": 120}, "B", "rule-7 sharded-replacing"},
		{"cube-mv", map[string]int{"MaterializedView": 40, "SummingMergeTree": 10, "MergeTree": 250}, "C", "rule-6 cube-mv"},
		{"star-dict", map[string]int{"Dictionary": 10, "MaterializedView": 30, "MergeTree": 200}, "D", "rule-5 star-dict"},
		{"plain-fallback", map[string]int{"MergeTree": 150}, "A", "rule-11 plain-mt-fallback"},
	}
	for _, c := range cases {
		counts := CountTables(mkTables(c.engines))
		p, rule := DetectPrimary(counts)
		if p != c.primary || rule != c.rule {
			t.Errorf("%s: got %s (%s), want %s (%s)", c.name, p, rule, c.primary, c.rule)
		}
	}
}

func TestArchetypeSecondaries(t *testing.T) {
	// Kafka over the halved threshold but primary decided by table count
	counts := CountTables(mkTables(map[string]int{"Kafka": 8, "MergeTree": 30}))
	p, _ := DetectPrimary(counts)
	if p != "A" {
		t.Fatalf("primary: %s", p)
	}
	sec := DetectSecondaries(counts, p)
	found := false
	for _, s := range sec {
		if s == "E" {
			found = true
		}
	}
	if !found {
		t.Errorf("kafka_n=8 must add secondary E, got %v", sec)
	}
}

func mkRun(hot []*HotTable, tables map[string]string) *Run {
	r := &Run{byFull: map[string]*Table{}, Hot: hot}
	for full, engine := range tables {
		db, name, _ := strings.Cut(full, ".")
		r.byFull[full] = &Table{Database: db, Name: name, Engine: engine}
	}
	return r
}

func TestDemoteRules(t *testing.T) {
	hot := []*HotTable{
		{Full: "biz.events", Execs: 1000, Sels: 900, Ins: 100, Users: []string{"alice", "bob"}},
		{Full: "biz.raw_ingest", Execs: 500, Sels: 10, Ins: 490, Users: []string{"etl"}},
		{Full: "biz.batch_table", Execs: 300, Sels: 300, Users: []string{"airflow_prod", "default"}},
		{Full: "biz.events_old", Execs: 50, Sels: 10, Ins: 40, Users: []string{"default"}},
		{Full: "biz.kafka_feed", Execs: 200, Sels: 200, Users: []string{"alice"}},
		{Full: "biz.live_new", Execs: 400, Sels: 390, Ins: 10, Users: []string{"alice"}},
		{Full: "biz.events_test_v2", Execs: 600, Sels: 600, Users: []string{"alice"}},
	}
	r := mkRun(hot, map[string]string{
		"biz.events":         "MergeTree",
		"biz.raw_ingest":     "MergeTree",
		"biz.batch_table":    "MergeTree",
		"biz.events_old":     "MergeTree",
		"biz.kafka_feed":     "Kafka",
		"biz.live_new":       "MergeTree",
		"biz.events_test_v2": "MergeTree",
	})
	r.demote("A", 7)

	get := func(full string) *HotTable {
		for _, h := range hot {
			if h.Full == full {
				return h
			}
		}
		t.Fatalf("missing %s", full)
		return nil
	}
	if get("biz.events").Demoted {
		t.Error("healthy human-selected table must stay hot")
	}
	if !get("biz.raw_ingest").Demoted {
		t.Error("insert-dominated must demote")
	}
	if !get("biz.batch_table").Demoted {
		t.Error("service-users-only must demote")
	}
	if !get("biz.events_old").Demoted {
		t.Error("staging-suffix + service users must demote")
	}
	if !get("biz.kafka_feed").Demoted {
		t.Error("infra engine must demote")
	}
	ln := get("biz.live_new")
	if ln.Demoted {
		t.Error("misleading staging name (human select-dominated) must stay hot")
	}
	if len(ln.ReviewFlags) == 0 || !strings.Contains(ln.ReviewFlags[0], "misleading-staging-name") {
		t.Errorf("live_new must carry the review flag, got %v", ln.ReviewFlags)
	}
	sv := get("biz.events_test_v2")
	if sv.Demoted {
		t.Error("shadow traffic at 60% of base must stay hot")
	}
	if len(sv.ReviewFlags) == 0 || !strings.Contains(sv.ReviewFlags[0], "shadow-traffic-vs-biz.events") {
		t.Errorf("shadow-traffic flag missing: %v", sv.ReviewFlags)
	}
}

func TestClassifyRole(t *testing.T) {
	timed := func(rows uint64) *Table {
		return &Table{Engine: "MergeTree", TotalRows: rows, Columns: []classify.Column{
			{Name: "event_time", Type: "DateTime"}, {Name: "user_id", Type: "UInt64"},
		}}
	}
	if role, conf := ClassifyRole(timed(50_000_000)); role != "Fact" || conf != "Confident" {
		t.Errorf("big timed table: %s/%s", role, conf)
	}
	if role, conf := ClassifyRole(timed(5_000_000)); role != "Fact" || conf != "Likely" {
		t.Errorf("mid timed table: %s/%s", role, conf)
	}
	lookup := &Table{Engine: "MergeTree", TotalRows: 5000, Columns: []classify.Column{
		{Name: "country_id", Type: "UInt32", InPK: true}, {Name: "country_name", Type: "String"},
	}}
	if role, conf := ClassifyRole(lookup); role != "Dim" || conf != "Likely" {
		t.Errorf("lookup table: %s/%s", role, conf)
	}
	if role, _ := ClassifyRole(&Table{Engine: "Dictionary"}); role != "Dim" {
		t.Errorf("dictionary: %s", role)
	}
	if role, _ := ClassifyRole(&Table{Engine: "View"}); role != "Mart" {
		t.Errorf("view: %s", role)
	}
	if role, _ := ClassifyRole(&Table{Engine: "Kafka"}); role != "Pipeline" {
		t.Errorf("kafka: %s", role)
	}
}

func TestParseStrArray(t *testing.T) {
	cases := map[string][]string{
		"['a','b']":    {"a", "b"},
		"[]":           nil,
		"['with\\'q']": {"with'q"},
		"['x']":        {"x"},
		"not-an-array": nil,
	}
	for in, want := range cases {
		got := parseStrArray(in)
		if len(got) != len(want) {
			t.Errorf("parseStrArray(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("parseStrArray(%q)[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}

func TestDistributedParse(t *testing.T) {
	r := &Run{byFull: map[string]*Table{}}
	r.Tables = []*Table{{
		Database: "biz", Name: "events", Engine: "Distributed",
		EngineFull: "Distributed('main', 'biz', 'events_local', rand())",
	}}
	for _, t2 := range r.Tables {
		r.byFull[t2.Full()] = t2
	}
	// relations() also queries dictionaries; use the pure part directly
	if m := distRe.FindStringSubmatch(r.Tables[0].EngineFull); m == nil ||
		m[2] != "biz" || m[3] != "events_local" || strings.TrimSpace(m[4]) != "rand()" {
		t.Errorf("distributed parse failed: %v", m)
	}
}

func TestMVToParse(t *testing.T) {
	ddl := "CREATE MATERIALIZED VIEW biz.mv1 TO biz.agg_daily (`d` Date) AS SELECT ..."
	m := mvToRe.FindStringSubmatch(ddl)
	if m == nil || pick(m, 1, 2) != "biz" || pick(m, 3, 4) != "agg_daily" {
		t.Errorf("mv TO parse failed: %v", m)
	}
	ddl2 := "CREATE MATERIALIZED VIEW x TO `we-ird`.`ta ble` AS SELECT 1"
	m = mvToRe.FindStringSubmatch(ddl2)
	if m == nil || pick(m, 1, 2) != "we-ird" || pick(m, 3, 4) != "ta ble" {
		t.Errorf("quoted mv TO parse failed: %v", m)
	}
}
