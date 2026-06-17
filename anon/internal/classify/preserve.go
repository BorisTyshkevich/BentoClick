// Schema-preserving model (anond --model=schema-preserving).
//
// Unlike the tokenizing model (this package's Classify/MaskExpr), schema
// preservation keeps REAL table and column NAMES and masks only VALUES — for
// well-known schemas (e.g. ClickHouse `system`) that domain tools query by
// their real schema. It is a faithful port of the GENERIC rules in
// system-anon/gen.py; the per-table, ClickHouse-system-specific OVERRIDE map
// from gen.py is deliberately NOT hardcoded here (that is domain knowledge that
// belongs with the operator / altinity-skills, not in bentoclick) — it arrives
// as the `overrides` argument instead.
//
// Per-column class (first match wins):
//  1. drop    — nested subcolumns (name contains '.'); the parent Map/Nested stays.
//  2. override["table.column"] — operator-supplied keep|tok:<kind>|hash:<kind>|redact|drop.
//  3. keep    — Map(...) (e.g. ProfileEvents/Settings), and numeric/Date*/Bool/Enum/UUID
//     (incl. Array/Nullable/LowCardinality wrappers). IPv4/IPv6 are NOT kept.
//  4. redact  — IPv4/IPv6 (client addresses) → sentinel of the right type.
//  5. tok:<kind> — identifier-named columns (db/table/column/user/host), reversible:
//     a deterministic keyed token minted into the secret identifier_map.
//  6. hash:<kind> — id/uuid/key-shaped columns: deterministic, joinable, NOT reversible.
//  7. redact  — fail-closed default for any remaining string-bearing column; else drop.
package classify

import (
	"fmt"
	"strings"
)

// PreserveClass is the raw schema-preserving class: "keep", "tok:<kind>",
// "hash:<kind>", "redact", or "drop".
type PreserveClass string

// reversible namespace prefixes (the live detok UDF expands these). Must stay a
// subset of the detok regex namespace: db|tbl|col|user|role|dict|cluster|disk|host|sql|field|enum.
var preserveTokKinds = map[string]bool{
	"db": true, "tbl": true, "col": true, "user": true, "host": true, "dict": true,
}

// preserveTokExact maps common identifier column names to a reversible kind.
// Generic (a column that holds a db/table/column/user/host NAME), not tied to
// any one schema; extend per deployment via overrides.
var preserveTokExact = map[string]string{
	"user": "user", "initial_user": "user", "authenticated_user": "user", "os_user": "user",
	"hostname": "host", "client_hostname": "host",
	"database": "db", "current_database": "db", "databases": "db",
	"table": "tbl", "tables": "tbl", "view_name": "tbl", "view_target": "tbl", "views": "tbl",
	"column": "col", "columns": "col",
}

// preserveTokSuffix: identifier columns by suffix.
var preserveTokSuffix = []struct{ suf, kind string }{
	{"_database", "db"}, {"_table", "tbl"},
}

// ClassifyPreserve assigns the schema-preserving class for one column.
// overrides maps a column name to a forced class ("keep"|"tok:<kind>"|
// "hash:<kind>"|"redact"|"drop"); it is the operator's escape hatch for the
// columns whose safety can't be read from name+type alone.
func ClassifyPreserve(c Column, overrides map[string]string) PreserveClass {
	if strings.Contains(c.Name, ".") {
		return "drop"
	}
	if ov, ok := overrides[c.Name]; ok {
		return PreserveClass(ov)
	}
	if strings.HasPrefix(c.Type, "Map(") {
		// Keep only CH-internal name/number maps (ProfileEvents/Settings/counters)
		// real; any OTHER Map column's values are redacted (fail-closed) — a
		// generic Map could carry PII. Operators can re-include one via overrides.
		if isInternalMapColumn(c.Name) {
			return "keep"
		}
		return "redact"
	}
	if preserveKeepable(c.Type) {
		return "keep"
	}
	if preserveIsIP(c.Type) {
		return "redact" // client addresses are identifying
	}
	name := strings.ToLower(c.Name)
	if kind, ok := preserveTokExact[name]; ok {
		return PreserveClass("tok:" + kind)
	}
	for _, sf := range preserveTokSuffix {
		if strings.HasSuffix(name, sf.suf) {
			return PreserveClass("tok:" + sf.kind)
		}
	}
	// Only string-bearing columns can be hashed/redacted; an unhandled complex
	// type (Tuple/Nested/…) has no safe sentinel, so it is dropped (fail-closed).
	if preserveStringish(c.Type) {
		if idName.MatchString(c.Name) || c.InPK || c.InSK || c.InPart {
			return "hash:qid" // opaque id-shaped / key string: deterministic, non-reversible
		}
		return "redact" // fail-closed default for any remaining string-bearing column
	}
	return "drop"
}

// isInternalMapColumn allowlists ClickHouse-internal Map columns whose values
// are counters / setting values (no user data), kept real in schema-preserving.
func isInternalMapColumn(name string) bool {
	switch name {
	case "ProfileEvents", "Settings", "replica_is_active", "asynchronous_read_counters":
		return true
	}
	return strings.HasSuffix(name, "_counters") || strings.HasSuffix(name, "ProfileEvents")
}

// preserveKeepable: numeric / temporal / bool / enum / UUID, possibly wrapped in
// Array/Nullable/LowCardinality. IPv4/IPv6 are intentionally NOT keepable.
func preserveKeepable(t string) bool {
	base := preserveUnwrapAll(t)
	if base == "String" || strings.HasPrefix(base, "FixedString") {
		return false
	}
	return base == "UUID" || isNumeric(base) || isTemporal(base) || strings.HasPrefix(base, "Enum")
}

func preserveIsIP(t string) bool {
	return strings.Contains(t, "IPv4") || strings.Contains(t, "IPv6")
}

func preserveStringish(t string) bool {
	return strings.Contains(t, "String") || strings.HasPrefix(t, "Array(") || strings.HasPrefix(t, "Map(")
}

// preserveUnwrapAll strips Array/Nullable/LowCardinality wrappers to the base.
func preserveUnwrapAll(t string) string {
	for {
		switch {
		case strings.HasPrefix(t, "Array(") && strings.HasSuffix(t, ")"):
			t = t[len("Array(") : len(t)-1]
		case strings.HasPrefix(t, "Nullable(") && strings.HasSuffix(t, ")"):
			t = t[len("Nullable(") : len(t)-1]
		case strings.HasPrefix(t, "LowCardinality(") && strings.HasSuffix(t, ")"):
			t = t[len("LowCardinality(") : len(t)-1]
		default:
			return t
		}
	}
}

// PreserveKind returns the token/hash kind for a "tok:<kind>"/"hash:<kind>"
// class, else "".
func PreserveKind(class PreserveClass) string {
	if i := strings.IndexByte(string(class), ':'); i >= 0 {
		return string(class)[i+1:]
	}
	return ""
}

// preserveRedactExpr is the sentinel value for a redacted column, matching its type.
func preserveRedactExpr(t string) string {
	switch {
	case strings.HasPrefix(t, "Array("):
		return "[]"
	case strings.HasPrefix(t, "Map("):
		return "map()"
	case strings.Contains(t, "IPv6"):
		return "toIPv6('::')"
	case strings.Contains(t, "IPv4"):
		return "toIPv4('0.0.0.0')"
	default:
		return "'[redacted]'"
	}
}

// PreserveMaskExpr returns the SELECT expression for one column in the
// schema-preserving sandbox: real column NAME out, masked VALUE. seed is the
// secret value seed; it appears only in the source-side INSERT...SELECT.
func PreserveMaskExpr(c Column, class PreserveClass, seed string) (expr string, include bool) {
	q := quoteIdent(c.Name)
	kind := PreserveKind(class)
	switch {
	case class == "drop":
		return "", false
	case class == "keep":
		return q, true
	case class == "redact":
		return fmt.Sprintf("CAST(%s AS %s) AS %s", preserveRedactExpr(c.Type), c.Type, q), true
	case strings.HasPrefix(string(class), "tok:") || strings.HasPrefix(string(class), "hash:"):
		body := preserveTokBody(q, kind, seed, strings.HasPrefix(c.Type, "Array("))
		return fmt.Sprintf("CAST(%s AS %s) AS %s", body, c.Type, q), true
	default:
		return "", false
	}
}

// preserveTokBody builds the deterministic token expression: a scalar gets
// concat('<kind>_', hex(sipHash64(seed, v))); an array maps it element-wise.
// Empty values pass through (an empty string is not an identifier).
func preserveTokBody(q, kind, seed string, isArray bool) string {
	mk := func(v string) string {
		return fmt.Sprintf("if(empty(%[1]s), %[1]s, concat('%[2]s_', lower(hex(sipHash64('%[3]s', %[1]s)))))", v, kind, seed)
	}
	if isArray {
		return fmt.Sprintf("arrayMap(x -> %s, %s)", mk("x"), q)
	}
	return mk("toString(" + q + ")")
}

// RegistryClassTokenizing maps a tokenizing Class to the unified
// bentoclick.schema_guide class + a usage hint. ok=false means the column is
// excluded from the sandbox (no registry row).
func RegistryClassTokenizing(c Class) (class, usage string, ok bool) {
	switch c {
	case ClassTime:
		return "real", "time axis / range filter", true
	case ClassMeasure:
		return "real", "aggregate: sum/avg/count", true
	case ClassEnum:
		return "real", "closed vocabulary: filter / group", true
	case ClassLabel:
		return "identifier", "GROUP BY dimension (token relabels to real for the human)", true
	case ClassJoinKey:
		return "identifier", "JOIN / uniq / high-card GROUP BY (deterministic token)", true
	case ClassFreeText:
		return "redacted", "REDACTED; never group or filter", true
	case ClassAttrMap:
		return "attrmap", "Map: call describe_attributes per key", true
	default: // ClassSchemaless — excluded
		return "", "", false
	}
}

// RegistryClassPreserving maps a schema-preserving class to the unified
// registry class + usage hint. ok=false (drop) means no registry row.
func RegistryClassPreserving(c PreserveClass) (class, usage string, ok bool) {
	switch {
	case c == "keep":
		return "real", "verbatim value: filter / group / aggregate", true
	case c == "redact":
		return "redacted", "REDACTED; never group or filter", true
	case strings.HasPrefix(string(c), "tok:"):
		return "identifier", "identifier token (" + PreserveKind(c) + "); GROUP BY only, relabels to real for the human", true
	case strings.HasPrefix(string(c), "hash:"):
		return "identifier", "deterministic id; GROUP BY / JOIN only, stays masked", true
	default: // drop
		return "", "", false
	}
}

// PreserveMintSelect returns the SELECT that yields (original, token) rows for
// one reversible (tok:<kind>) column, to be inserted into the secret
// identifier_map so the human report can detok them. Returns ("", false) for
// keep/hash/redact/drop (hash is deterministic but intentionally non-reversible).
func PreserveMintSelect(c Column, class PreserveClass, seed, srcDB, srcTable string) (sel string, mint bool) {
	if !strings.HasPrefix(string(class), "tok:") {
		return "", false
	}
	kind := PreserveKind(class)
	if !preserveTokKinds[kind] {
		return "", false
	}
	q := quoteIdent(c.Name)
	val := "toString(" + q + ")"
	if strings.HasPrefix(c.Type, "Array(") {
		val = "arrayJoin(" + q + ")"
	}
	tok := fmt.Sprintf("concat('%s_', lower(hex(sipHash64('%s', o))))", kind, seed)
	return fmt.Sprintf(
		"SELECT '%s' AS kind, o AS original, %s AS token FROM (SELECT DISTINCT %s AS o FROM %s.%s) WHERE notEmpty(o)",
		kind, tok, val, quoteIdent(srcDB), quoteIdent(srcTable)), true
}
