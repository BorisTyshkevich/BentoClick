package discover

import (
	"strings"
	"testing"

	"github.com/Altinity/anon-discovery/internal/classify"
)

// ---- WordPresent ----

func TestWordPresentBasic(t *testing.T) {
	cases := []struct {
		text, name string
		want       bool
	}{
		{"hello world", "world", true},
		{"hello world", "hell", false}, // "hell" is a prefix of "hello", no right-boundary
		{"hello_world", "world", false},  // "world" has '_' on left — that IS a word char
		{"hello world", "hello", true},
		{"hello world foo", "foo", true},
		// word at start
		{"abc def", "abc", true},
		// word at end
		{"abc def", "def", true},
		// empty name
		{"hello", "", false},
		// name not in text
		{"hello", "xyz", false},
		// exact match
		{"abc", "abc", true},
		// name as part of longer word
		{"DateTime", "Date", false}, // Date is prefix — rightOK fails because 'T' is word char
		// word with non-alpha boundary chars
		{"(foo)", "foo", true},
		{"`column_name`", "column_name", true},
		// seed-like numeric
		{"ORDER BY 1234567890 foo", "1234567890", true},
		{"ORDER BY 12345678901 foo", "1234567890", false}, // longer number
	}
	for _, c := range cases {
		got := WordPresent(c.text, c.name)
		if got != c.want {
			t.Errorf("WordPresent(%q, %q) = %v, want %v", c.text, c.name, got, c.want)
		}
	}
}

func TestWordPresentRepeated(t *testing.T) {
	// Multiple occurrences: first doesn't match (partial), second does
	text := "DateTime Date foo"
	if !WordPresent(text, "Date") {
		t.Error("WordPresent should find 'Date' as a standalone word even if first occurrence is partial")
	}
}

// ---- findLeak ----

func TestFindLeakClean(t *testing.T) {
	// DDL with only tokens, no real identifiers or seed
	ddl := "CREATE TABLE biz_anon.tbl_abcd1234 (col_ef012345 UInt64, col_12345678 DateTime) ENGINE = MergeTree ORDER BY tuple()"
	if leak := findLeak(ddl, []string{"events", "user_id", "event_time"}, 9999999999); leak != "" {
		t.Errorf("clean DDL should have no leak, got %q", leak)
	}
}

func TestFindLeakRealTableName(t *testing.T) {
	// DDL contains real table name "events" as a word
	ddl := "CREATE TABLE dest.events (col_abc UInt64) ENGINE = MergeTree ORDER BY tuple()"
	leak := findLeak(ddl, []string{"events", "user_id"}, 0)
	if leak == "" {
		t.Error("DDL with real table name should be detected as leak")
	}
	if !strings.Contains(leak, "real identifier") {
		t.Errorf("leak message = %q", leak)
	}
}

func TestFindLeakRealColumnName(t *testing.T) {
	ddl := "CREATE TABLE dest.tbl_abc (user_email String) ENGINE = MergeTree ORDER BY tuple()"
	leak := findLeak(ddl, []string{"tbl_token", "user_email"}, 0)
	if leak == "" {
		t.Error("DDL with real column name should be detected as leak")
	}
}

func TestFindLeakSeedPresent(t *testing.T) {
	var seed uint64 = 1234567890
	ddl := "CREATE TABLE t (c UInt64 DEFAULT 1234567890) ENGINE = MergeTree"
	leak := findLeak(ddl, []string{"something_else"}, seed)
	if leak == "" {
		t.Error("DDL containing seed should be detected as leak")
	}
	if !strings.Contains(leak, "value seed present") {
		t.Errorf("leak message = %q", leak)
	}
}

func TestFindLeakShortNamesSkipped(t *testing.T) {
	// Names shorter than 4 chars are skipped to avoid false positives
	ddl := "CREATE TABLE dest.abc (id UInt64) ENGINE = MergeTree ORDER BY (id)"
	// "id" is 2 chars, "abc" is 3 chars — both should be skipped
	leak := findLeak(ddl, []string{"id", "abc"}, 0)
	if leak != "" {
		t.Errorf("names shorter than 4 chars should be skipped, got %q", leak)
	}
}

func TestFindLeakExactBoundary(t *testing.T) {
	// "events" appears embedded in "events_local" — should NOT trigger
	ddl := "CREATE TABLE dest.tbl_tok (events_local_col UInt64) ENGINE = MergeTree"
	// "events" (6 chars) is prefix of "events_local" — NOT a whole word
	leak := findLeak(ddl, []string{"events"}, 0)
	if leak != "" {
		t.Errorf("embedded name should not be a leak, got %q", leak)
	}
}

// ---- colNames ----

func TestColNames(t *testing.T) {
	tbl := &Table{
		Columns: []classify.Column{
			{Name: "event_time", Type: "DateTime"},
			{Name: "user_id", Type: "UInt64"},
			{Name: "status", Type: "String"},
		},
	}
	names := colNames(tbl)
	if len(names) != 3 {
		t.Fatalf("want 3 names, got %d", len(names))
	}
	if names[0] != "event_time" || names[1] != "user_id" || names[2] != "status" {
		t.Errorf("colNames = %v", names)
	}
}

func TestColNamesEmpty(t *testing.T) {
	tbl := &Table{}
	if names := colNames(tbl); len(names) != 0 {
		t.Errorf("empty columns should return empty, got %v", names)
	}
}
