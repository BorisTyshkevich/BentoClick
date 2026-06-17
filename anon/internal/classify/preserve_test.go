package classify

import "testing"

func TestClassifyPreserve(t *testing.T) {
	cases := []struct {
		name, typ string
		inPK      bool
		ov        map[string]string
		want      PreserveClass
	}{
		{"ProfileEvents.Names", "Array(String)", false, nil, "drop"},               // dotted → drop
		{"query", "String", false, map[string]string{"query": "redact"}, "redact"}, // override
		{"ProfileEvents", "Map(LowCardinality(String), UInt64)", false, nil, "keep"},
		{"Settings", "Map(String, String)", false, nil, "keep"},
		{"query_duration_ms", "UInt64", false, nil, "keep"},
		{"event_time", "DateTime", false, nil, "keep"},
		{"type", "Enum8('a' = 1)", false, nil, "keep"},
		{"some_uuid", "UUID", false, nil, "keep"},
		{"address", "IPv6", false, nil, "redact"},
		{"initial_address", "Nullable(IPv6)", false, nil, "redact"},
		{"user", "LowCardinality(String)", false, nil, "tok:user"},
		{"hostname", "LowCardinality(String)", false, nil, "tok:host"},
		{"databases", "Array(LowCardinality(String))", false, nil, "tok:db"},
		{"tables", "Array(LowCardinality(String))", false, nil, "tok:tbl"},
		{"columns", "Array(LowCardinality(String))", false, nil, "tok:col"},
		{"dependencies_database", "Array(String)", false, nil, "tok:db"}, // suffix
		{"query_id", "String", false, nil, "hash:qid"},                   // id-shaped → hash
		{"exception", "String", false, nil, "redact"},                    // fail-closed
		{"thread_ids", "Array(UInt64)", false, nil, "keep"},              // numeric array
		{"transaction_id", "Tuple(UInt64, UInt64)", false, nil, "drop"},  // non-string complex → drop
		{"pk_col", "String", true, nil, "hash:qid"},                      // PK string → hash
	}
	for _, c := range cases {
		got := ClassifyPreserve(Column{Name: c.name, Type: c.typ, InPK: c.inPK}, c.ov)
		if got != c.want {
			t.Errorf("ClassifyPreserve(%s %s) = %q, want %q", c.name, c.typ, got, c.want)
		}
	}
}

func TestPreserveMaskExpr(t *testing.T) {
	seed := "SEED"
	check := func(name, typ string, class PreserveClass, wantSub string, wantInc bool) {
		t.Helper()
		expr, inc := PreserveMaskExpr(Column{Name: name, Type: typ}, class, seed)
		if inc != wantInc {
			t.Fatalf("%s: include=%v want %v (expr=%q)", name, inc, wantInc, expr)
		}
		if wantSub != "" && !contains(expr, wantSub) {
			t.Errorf("%s: expr %q missing %q", name, expr, wantSub)
		}
	}
	check("dur", "UInt64", "keep", "`dur`", true)
	check("q", "String", "redact", "'[redacted]'", true)
	check("addr", "IPv6", "redact", "toIPv6('::')", true)
	check("tags", "Array(String)", "redact", "CAST([] AS Array(String))", true)
	check("user", "String", "tok:user", "concat('user_', lower(hex(sipHash64('SEED'", true)
	check("tbls", "Array(String)", "tok:tbl", "arrayMap(x ->", true)
	check("gone", "Tuple()", "drop", "", false)
}

func TestPreserveMintSelect(t *testing.T) {
	seed := "SEED"
	// reversible tok → mint
	sel, mint := PreserveMintSelect(Column{Name: "user", Type: "String"}, "tok:user", seed, "system", "query_log")
	if !mint || !contains(sel, "DISTINCT toString(`user`)") || !contains(sel, "'user' AS kind") {
		t.Errorf("tok mint: mint=%v sel=%q", mint, sel)
	}
	// array tok → arrayJoin
	sel, mint = PreserveMintSelect(Column{Name: "tables", Type: "Array(String)"}, "tok:tbl", seed, "system", "query_log")
	if !mint || !contains(sel, "arrayJoin(`tables`)") {
		t.Errorf("array tok mint: mint=%v sel=%q", mint, sel)
	}
	// hash is deterministic but NOT reversible → no mint
	if _, mint := PreserveMintSelect(Column{Name: "query_id", Type: "String"}, "hash:qid", seed, "system", "query_log"); mint {
		t.Error("hash class must not mint into identifier_map")
	}
	// keep/redact → no mint
	if _, mint := PreserveMintSelect(Column{Name: "x", Type: "UInt64"}, "keep", seed, "system", "t"); mint {
		t.Error("keep class must not mint")
	}
}

func TestPreserveKind(t *testing.T) {
	if PreserveKind("tok:user") != "user" {
		t.Error("tok kind")
	}
	if PreserveKind("hash:qid") != "qid" {
		t.Error("hash kind")
	}
	if PreserveKind("keep") != "" {
		t.Error("keep has no kind")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
