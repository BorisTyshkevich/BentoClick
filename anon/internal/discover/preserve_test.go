package discover

import (
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/classify"
)

func col(name, typ string) classify.Column { return classify.Column{Name: name, Type: typ} }

func TestBuildPreserve(t *testing.T) {
	tbl := &Table{
		Database:   "system",
		Name:       "query_log",
		Engine:     "MergeTree",
		SortingKey: "event_date, event_time",
		TotalRows:  1000,
		Columns: []classify.Column{
			col("event_date", "Date"),
			col("event_time", "DateTime"),
			col("query_duration_ms", "UInt64"),
			col("query", "String"),
			col("databases", "Array(LowCardinality(String))"),
			col("user", "LowCardinality(String)"),
			col("query_id", "String"),
			col("address", "IPv6"),
			col("transaction_id", "Tuple(UInt64, UInt64)"),
		},
	}
	tbl.Columns[0].InPart = true // event_date in partition key for the window probe
	p := buildPreserve(tbl, "SEED", "system_anon", "run-1", nil)

	joined := strings.Join(p.Cols, " | ")
	// kept verbatim
	mustContain(t, joined, "`query_duration_ms` AS `query_duration_ms`")
	// redacted free text + IP sentinel
	mustContain(t, joined, "'[redacted]'")
	mustContain(t, joined, "toIPv6('::')")
	// reversible tok (user, databases) and non-reversible hash (query_id)
	mustContain(t, joined, "concat('user_'")
	mustContain(t, joined, "concat('db_'")
	mustContain(t, joined, "concat('qid_'")
	// transaction_id (Tuple) dropped — not present
	if strings.Contains(joined, "transaction_id") {
		t.Errorf("transaction_id (unhandled complex) should be dropped; got %q", joined)
	}
	// window detected on event_date (kept temporal in partition key)
	if p.WindowOn != "event_date" {
		t.Errorf("WindowOn = %q, want event_date", p.WindowOn)
	}
	// ORDER BY kept the real sorting-key columns
	if strings.Join(p.OrderBy, ",") != "`event_date`,`event_time`" {
		t.Errorf("OrderBy = %v", p.OrderBy)
	}
	// mints only for reversible tok columns (user, databases) — not query_id/keep/redact
	if len(p.Mints) != 2 {
		t.Errorf("want 2 mint selects (user, databases), got %d: %v", len(p.Mints), p.Mints)
	}
	// registry rows: one per included column (everything except dropped transaction_id = 8)
	if len(p.RegRows) != 8 {
		t.Errorf("want 8 registry rows, got %d", len(p.RegRows))
	}
	// override forces a column's class
	p2 := buildPreserve(tbl, "SEED", "system_anon", "run-1", map[string]string{"query": "keep"})
	if !strings.Contains(strings.Join(p2.Cols, " | "), "`query` AS `query`") {
		t.Error("override query->keep should keep the column verbatim")
	}
}

func TestRegistryClassMapping(t *testing.T) {
	// tokenizing
	for _, c := range []struct {
		in   classify.Class
		want string
	}{
		{classify.ClassTime, "real"}, {classify.ClassMeasure, "real"}, {classify.ClassEnum, "real"},
		{classify.ClassLabel, "identifier"}, {classify.ClassJoinKey, "identifier"},
		{classify.ClassFreeText, "redacted"}, {classify.ClassAttrMap, "attrmap"},
	} {
		got, _, ok := classify.RegistryClassTokenizing(c.in)
		if !ok || got != c.want {
			t.Errorf("RegistryClassTokenizing(%s) = %q ok=%v, want %q", c.in, got, ok, c.want)
		}
	}
	if _, _, ok := classify.RegistryClassTokenizing(classify.ClassSchemaless); ok {
		t.Error("schemaless must be excluded from the registry")
	}
	// schema-preserving
	for _, c := range []struct {
		in   classify.PreserveClass
		want string
	}{
		{"keep", "real"}, {"tok:user", "identifier"}, {"hash:qid", "identifier"}, {"redact", "redacted"},
	} {
		got, _, ok := classify.RegistryClassPreserving(c.in)
		if !ok || got != c.want {
			t.Errorf("RegistryClassPreserving(%s) = %q ok=%v, want %q", c.in, got, ok, c.want)
		}
	}
	if _, _, ok := classify.RegistryClassPreserving("drop"); ok {
		t.Error("drop must be excluded from the registry")
	}
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("missing %q in %q", sub, s)
	}
}
