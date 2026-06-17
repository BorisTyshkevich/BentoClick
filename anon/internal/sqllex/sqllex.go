// Package sqllex is reversible SQL anonymization by token rewrite — a Go port
// of the collector's sqlrewrite.py.
//
// A LEXER, not a parser: Lex is a regex scanner that splits SQL into flat
// tokens (no grammar/AST). The Rewriter then classifies each word token and:
//
//   - keeps SQL keywords always (keyword-named columns arrive backtick-quoted
//     in CH DDL, so they show up as quoted-identifier tokens, not bare words),
//   - maps identifiers through the global injective map — reusing the
//     structured token when the name is a known db/tbl/col/... (neighbor-aware
//     probe resolves the kind of a qualified db.tbl.col), else minting a fresh
//     "sql" token,
//   - keeps data types, function names and setting names (the cluster's own
//     vocabulary, loaded into the keep registry),
//   - redacts string literals and removes comments,
//   - keeps numbers, except in strict mode where they are redacted too,
//   - fails closed (whole value -> '[redacted]') on an unbalanced quote.
package sqllex

import (
	"regexp"
	"strings"

	"github.com/Altinity/anon-discovery/internal/idmap"
)

const Redacted = "'[redacted]'"

var sqlKeywords = makeSet(strings.Fields(strings.ToUpper(
	"SELECT INSERT UPDATE DELETE CREATE ALTER DROP ATTACH DETACH RENAME TRUNCATE EXCHANGE " +
		"TABLE VIEW MATERIALIZED LIVE DICTIONARY DATABASE FUNCTION INDEX PROJECTION CONSTRAINT " +
		"FROM WHERE PREWHERE GROUP BY ORDER HAVING LIMIT OFFSET SETTINGS FORMAT QUALIFY " +
		"UNION ALL DISTINCT INTERSECT EXCEPT WITH RECURSIVE TOTALS ROLLUP CUBE GROUPING " +
		"JOIN INNER LEFT RIGHT FULL OUTER CROSS ANY SEMI ANTI ASOF ON USING ARRAY PASTE " +
		"AND OR NOT IN GLOBAL BETWEEN LIKE ILIKE IS NULL AS ASC DESC NULLS FIRST LAST COLLATE " +
		"CASE WHEN THEN ELSE END IF INTERVAL EXTRACT CAST WITHFILL STEP " +
		"ENGINE PARTITION PRIMARY KEY SAMPLE TTL CODEC DEFAULT ALIAS EPHEMERAL COMMENT " +
		"TO FINAL REPLACE VALUES SET MODIFY ADD COLUMN CLEAR MOVE FREEZE FETCH " +
		"GRANULARITY TYPE INTO OUTFILE CLUSTER ROWS ONLY TIES FOLLOWING PRECEDING UNBOUNDED " +
		"TRUE FALSE INF NAN DATABASES TABLES DICTIONARIES OVER WINDOW PARTITIONS DEFINER " +
		"INVOKER SECURITY POPULATE TEMPORARY EXISTS REMOVE")))

// TypeKeywords: the type-system vocabulary kept verbatim (port of
// freetext.py TYPE_KEYWORDS, uppercased).
var TypeKeywords = makeSet(strings.Fields(
	"UINT8 UINT16 UINT32 UINT64 UINT128 UINT256 " +
		"INT8 INT16 INT32 INT64 INT128 INT256 " +
		"FLOAT32 FLOAT64 BFLOAT16 BOOL BOOLEAN " +
		"DECIMAL DECIMAL32 DECIMAL64 DECIMAL128 DECIMAL256 " +
		"STRING FIXEDSTRING UUID DATE DATE32 DATETIME DATETIME64 " +
		"TIME TIME64 INTERVAL ENUM ENUM8 ENUM16 " +
		"ARRAY TUPLE NESTED MAP NULLABLE LOWCARDINALITY " +
		"AGGREGATEFUNCTION SIMPLEAGGREGATEFUNCTION " +
		"POINT RING POLYGON MULTIPOLYGON LINESTRING MULTILINESTRING " +
		"NOTHING IPV4 IPV6 JSON OBJECT DYNAMIC VARIANT NULL " +
		"NANOSECOND MICROSECOND MILLISECOND SECOND MINUTE HOUR " +
		"DAY WEEK MONTH QUARTER YEAR " +
		"MINMAX SET BLOOM_FILTER NGRAMBF_V1 TOKENBF_V1 HYPOTHESIS " +
		"VECTOR_SIMILARITY INVERTED GIN FULL_TEXT " +
		"SUM COUNT AVG MIN MAX ANY ANYLAST ANYHEAVY " +
		"ARGMIN ARGMAX UNIQ UNIQEXACT UNIQCOMBINED UNIQHLL12 UNIQTHETA " +
		"QUANTILE QUANTILES QUANTILETDIGEST QUANTILESTIMING QUANTILEEXACT MEDIAN " +
		"GROUPARRAY GROUPUNIQARRAY GROUPBITMAP GROUPARRAYINSERTAT " +
		"SUMMAP MINMAP MAXMAP AVGWEIGHTED TOPK TOPKWEIGHTED " +
		"STDDEVSAMP STDDEVPOP VARSAMP VARPOP CORR COVARSAMP COVARPOP " +
		"ENTROPY SIMPLESTATE DELTASUM SEQUENCEMATCH"))

// safeLiterals: non-sensitive string literals kept verbatim (time units,
// format names) — port of freetext.py SAFE_LITERALS.
var safeLiterals = makeSet([]string{
	"second", "minute", "hour", "day", "week", "month", "quarter", "year",
	"CSV", "TSV", "TSVRaw", "TabSeparated", "JSON", "JSONEachRow", "Parquet",
	"Native", "Values", "Arrow", "ORC", "Protobuf", "Avro", "RowBinary",
})

func makeSet(ws []string) map[string]bool {
	m := make(map[string]bool, len(ws))
	for _, w := range ws {
		m[w] = true
	}
	return m
}

// Probe is the default kind-probe order (most specific first). No "func":
// builtin function names are CH vocabulary (kept via the registry), not
// mapped. "field" lets type-field names resolve.
var Probe = []string{"col", "tbl", "db", "dict", "user", "role", "cluster", "disk", "field"}

type tokKind int

const (
	tWS tokKind = iota
	tLineComment
	tBlockComment
	tString
	tBacktick
	tDQuote
	tNumber
	tWord
	tPunct
)

type tok struct {
	kind tokKind
	text string
}

var tokenRe = regexp.MustCompile(
	`(\s+)` + // 1 ws
		`|(--[^\n]*)` + // 2 line comment
		`|(/\*[\s\S]*?\*/|/\*[\s\S]*)` + // 3 block comment
		"|('(?:\\\\.|''|[^'\\\\])*')" + // 4 string
		"|(`(?:``|[^`])*`)" + // 5 backtick ident
		`|("(?:\\.|""|[^"\\])*")` + // 6 double-quoted ident
		`|(0[xX][0-9a-fA-F]+|0[bB][01]+|(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?)` + // 7 number
		`|([A-Za-z_][A-Za-z0-9_$]*)` + // 8 word
		`|([\s\S])`) // 9 punct (any single byte)

var groupKinds = []tokKind{tWS, tLineComment, tBlockComment, tString, tBacktick, tDQuote, tNumber, tWord, tPunct}

// lex yields tokens covering the whole string with no gaps.
func lex(sql string) []tok {
	var out []tok
	for _, m := range tokenRe.FindAllStringSubmatchIndex(sql, -1) {
		for g := 1; g <= 9; g++ {
			if m[2*g] >= 0 {
				out = append(out, tok{groupKinds[g-1], sql[m[2*g]:m[2*g+1]]})
				break
			}
		}
	}
	return out
}

func identName(k tokKind, text string) string {
	switch k {
	case tBacktick:
		return strings.ReplaceAll(text[1:len(text)-1], "``", "`")
	case tDQuote:
		return strings.ReplaceAll(text[1:len(text)-1], `""`, `"`)
	}
	return text
}

func safeString(text string) bool {
	content := text[1 : len(text)-1]
	content = strings.ReplaceAll(content, `\'`, `'`)
	content = strings.ReplaceAll(content, `''`, `'`)
	content = strings.ReplaceAll(content, `\\`, `\`)
	if content == "" || safeLiterals[content] {
		return true
	}
	for _, r := range content {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func sigTok(toks []tok, from, step int) (tok, bool) {
	for i := from; i >= 0 && i < len(toks); i += step {
		switch toks[i].kind {
		case tWS, tLineComment, tBlockComment:
			continue
		}
		return toks[i], true
	}
	return tok{}, false
}

func prevIsDot(toks []tok, i int) bool {
	t, ok := sigTok(toks, i-1, -1)
	return ok && t.kind == tPunct && t.text == "."
}

func nextIs(toks []tok, i int, ch string) bool {
	t, ok := sigTok(toks, i+1, +1)
	return ok && t.kind == tPunct && t.text == ch
}

// probeOrder biases the kind of a qualified a.b.c name by dot position:
// leftmost -> db/tbl, middle -> tbl, rightmost/member -> col.
func probeOrder(toks []tok, i int) []string {
	before, after := prevIsDot(toks, i), nextIs(toks, i, ".")
	switch {
	case after && !before:
		return []string{"db", "tbl", "dict"}
	case before && after:
		return []string{"tbl", "db", "dict"}
	case before:
		return []string{"col", "tbl"}
	}
	return Probe
}

// Rewriter rewrites SQL text through the identifier map.
type Rewriter struct {
	IdMap *idmap.IdMap
	Keep  map[string]bool // UPPER vocab: data types + cluster funcs/settings/engines
	// Combinators are aggregate-function combinator suffixes (UPPER, e.g.
	// "IF", "ARRAY", "MERGE") — sumIf/groupUniqArrayArray etc. are vocabulary
	// even though system.functions doesn't enumerate the combined forms.
	Combinators []string
	cache       map[[2]string]string

	IdsSubstituted  int
	StringsRedacted int
	ValuesUnparsed  int
}

// vocabKeep reports whether a word is CH vocabulary: directly in the keep
// registry, or a keep-registry function with combinator suffixes stacked on.
func (rw *Rewriter) vocabKeep(word string) bool {
	u := strings.ToUpper(word)
	if rw.Keep[u] {
		return true
	}
	for changed := true; changed; {
		changed = false
		for _, c := range rw.Combinators {
			if len(u) > len(c) && strings.HasSuffix(u, c) {
				u = u[:len(u)-len(c)]
				if rw.Keep[u] {
					return true
				}
				changed = true
				break
			}
		}
	}
	return false
}

// NewKeepRegistry combines SQL keywords, type keywords and the cluster's own
// vocabulary (function + setting names), uppercased.
func NewKeepRegistry(clusterVocab []string) map[string]bool {
	keep := map[string]bool{}
	for w := range sqlKeywords {
		keep[w] = true
	}
	for w := range TypeKeywords {
		keep[w] = true
	}
	for _, w := range clusterVocab {
		if w != "" {
			keep[strings.ToUpper(w)] = true
		}
	}
	return keep
}

func NewRewriter(im *idmap.IdMap, keep map[string]bool) *Rewriter {
	return &Rewriter{IdMap: im, Keep: keep, cache: map[[2]string]string{}}
}

// IdentifierWords yields the identifier names in a SQL value, for the Phase-B
// observe wave: bare words that aren't keywords/vocabulary/function calls,
// plus all quoted identifiers.
func (rw *Rewriter) IdentifierWords(value string) []string {
	if value == "" {
		return nil
	}
	toks := lex(value)
	var out []string
	for _, t := range toks {
		switch t.kind {
		case tWord:
			// Keep only true SQL keywords and known CH vocabulary (the keep
			// registry is loaded from system.functions + data_type_families).
			// An unknown word followed by '(' is a USER-DEFINED function, not a
			// builtin — collect it so it gets tokenized (fail-closed: a sensitive
			// UDF/object name must not survive un-anonymized).
			if sqlKeywords[strings.ToUpper(t.text)] || rw.vocabKeep(t.text) {
				continue
			}
			out = append(out, t.text)
		case tBacktick, tDQuote:
			out = append(out, identName(t.kind, t.text))
		}
	}
	return out
}

// Rewrite anonymizes one SQL value. strict additionally redacts numeric
// literals (row_policies.select_filter, mutations.command class of fields).
func (rw *Rewriter) Rewrite(value string, strict bool) string {
	if value == "" {
		return value
	}
	key := [2]string{value, ""}
	if strict {
		key[1] = "s"
	}
	if out, ok := rw.cache[key]; ok {
		return out
	}
	toks := lex(value)
	out := ""
	unbalanced := false
	for _, t := range toks {
		if t.kind == tPunct && (t.text == "'" || t.text == "`" || t.text == `"`) {
			unbalanced = true // a stray quote: the string/ident groups missed it
			break
		}
	}
	if unbalanced {
		rw.ValuesUnparsed++
		out = Redacted
	} else {
		var b strings.Builder
		for i, t := range toks {
			b.WriteString(rw.tok(toks, i, t, strict))
		}
		out = b.String()
	}
	rw.cache[key] = out
	return out
}

func (rw *Rewriter) known(name string, order []string) (string, bool) {
	for _, kind := range order {
		if tok, ok := rw.IdMap.Lookup(kind, name); ok {
			return tok, true
		}
	}
	for _, kind := range Probe { // fall back to any kind
		if contains(order, kind) {
			continue
		}
		if tok, ok := rw.IdMap.Lookup(kind, name); ok {
			return tok, true
		}
	}
	return "", false
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func (rw *Rewriter) tok(toks []tok, i int, t tok, strict bool) string {
	switch t.kind {
	case tWS, tPunct:
		return t.text
	case tLineComment:
		return ""
	case tBlockComment:
		return " "
	case tNumber:
		if strict {
			rw.StringsRedacted++
			return "0"
		}
		return t.text
	case tString:
		if safeString(t.text) {
			return t.text
		}
		rw.StringsRedacted++
		return Redacted
	}
	// identifier (word / backtick / dquote)
	name := identName(t.kind, t.text)
	if t.kind != tWord { // quoted identifier -> always map
		rw.IdsSubstituted++
		if tok, ok := rw.known(name, Probe); ok {
			return tok
		}
		return rw.mustSQL(name)
	}
	if sqlKeywords[strings.ToUpper(t.text)] { // structural keyword wins
		return t.text
	}
	if tok, ok := rw.known(name, probeOrder(toks, i)); ok {
		rw.IdsSubstituted++
		return tok
	}
	if rw.vocabKeep(t.text) { // data type / function / setting / engine / combinator form
		return t.text
	}
	// An unknown word followed by '(' is a USER-DEFINED function (builtins are in
	// the keep registry from system.functions). Fail closed: tokenize its name so
	// a sensitive UDF/object name cannot survive into the sandbox/profile.
	rw.IdsSubstituted++
	return rw.mustSQL(name)
}

// mustSQL maps an unknown identifier to its "sql" token. Fail-closed: if the
// observe wave missed it (a bug), redact rather than leak.
func (rw *Rewriter) mustSQL(name string) string {
	if tok, ok := rw.IdMap.Lookup("sql", name); ok {
		return tok
	}
	rw.StringsRedacted++
	return Redacted
}
