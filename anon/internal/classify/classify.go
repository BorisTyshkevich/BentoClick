// Package classify assigns every column a masking class and generates the
// trusted-side masking expression used to materialize the sandbox.
//
// Fail-closed like the collector: anything string-bearing or schemaless that
// no rule positively classifies is excluded from the sandbox entirely.
// Masking expressions embed the value seed and real column names — they
// appear ONLY in the job's INSERT...SELECT (trusted side), never in any
// object readable by the untrusted role (a materialized table's SHOW CREATE
// exposes token columns + engine only).
package classify

import (
	"fmt"
	"regexp"
	"strings"
)

type Class string

const (
	ClassTime       Class = "time"       // Date*/DateTime*/Time* — kept (continuity is analytically load-bearing)
	ClassMeasure    Class = "measure"    // plain numerics — kept (v1; per-deployment noise is an open question)
	ClassEnum       Class = "enum"       // Enum8/16 — kept (closed vocabulary)
	ClassJoinKey    Class = "joinkey"    // ids/UUIDs/IPs/key columns — deterministic keyed hash (joins survive)
	ClassLabel      Class = "label"      // LowCardinality(String) — short keyed-hash token per distinct value
	ClassFreeText   Class = "freetext"   // other String/FixedString — constant redaction
	ClassAttrMap    Class = "attrmap"    // Map(String|LowCardinality(String), String) — keys kept, values masked per-value
	ClassSchemaless Class = "schemaless" // JSON/Dynamic/complex-with-strings — EXCLUDED (fail closed)
)

type Column struct {
	Name   string
	Type   string
	InPK   bool // is_in_primary_key
	InSK   bool // is_in_sorting_key
	InPart bool // is_in_partition_key
}

// idName matches identifier-shaped column names.
var idName = regexp.MustCompile(`(?i)(^|_)(id|uuid|guid|key|hash|token|sid)s?$`)

// unwrap strips Nullable(...) and LowCardinality(...) wrappers.
// Returns base type, nullable, lowCard.
func unwrap(t string) (string, bool, bool) {
	nullable, lowCard := false, false
	for {
		switch {
		case strings.HasPrefix(t, "Nullable(") && strings.HasSuffix(t, ")"):
			nullable = true
			t = t[len("Nullable(") : len(t)-1]
		case strings.HasPrefix(t, "LowCardinality(") && strings.HasSuffix(t, ")"):
			lowCard = true
			t = t[len("LowCardinality(") : len(t)-1]
		default:
			return t, nullable, lowCard
		}
	}
}

func isInteger(base string) bool {
	return regexp.MustCompile(`^(U?Int)(8|16|32|64|128|256)$`).MatchString(base)
}

func isNumeric(base string) bool {
	return isInteger(base) ||
		strings.HasPrefix(base, "Float") || strings.HasPrefix(base, "BFloat") ||
		strings.HasPrefix(base, "Decimal") || base == "Bool"
}

func isTemporal(base string) bool {
	return base == "Date" || base == "Date32" ||
		strings.HasPrefix(base, "DateTime") || strings.HasPrefix(base, "Time")
}

// attrMapKey returns the key type when t is exactly Map(K, V) with
// K ∈ {String, LowCardinality(String)} and V == String — the OTel attribute
// shape (ResourceAttributes, LogAttributes). Neither accepted K contains a
// nested comma, so prefix matching on the canonical system.columns spelling
// ("Map(K, V)") is exact. Nullable/LowCardinality values are NOT accepted in
// v1: anything else stays string-bearing and fails closed.
func attrMapKey(t string) (string, bool) {
	inner, ok := strings.CutPrefix(t, "Map(")
	if !ok || !strings.HasSuffix(inner, ")") {
		return "", false
	}
	inner = inner[:len(inner)-1]
	for _, k := range []string{"String", "LowCardinality(String)"} {
		if v, ok := strings.CutPrefix(inner, k+", "); ok && v == "String" {
			return k, true
		}
	}
	return "", false
}

// stringBearing reports whether a COMPLEX type can carry free text anywhere
// inside (Array(String), Map(String,..), Tuple(.. String ..), FixedString...).
// Mirrors the collector's _NON_VALUE_TYPES fail-closed test.
func stringBearing(t string) bool {
	for _, s := range []string{"String", "IPv4", "IPv6", "JSON", "Object", "Dynamic", "Variant"} {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

// Classify assigns the masking class for one column.
func Classify(c Column) Class {
	base, _, lowCard := unwrap(c.Type)
	switch {
	case base == "UUID":
		return ClassJoinKey
	case base == "IPv4" || base == "IPv6":
		return ClassJoinKey
	case isTemporal(base):
		return ClassTime
	case strings.HasPrefix(base, "Enum"):
		return ClassEnum
	case base == "String" || strings.HasPrefix(base, "FixedString"):
		if idName.MatchString(c.Name) || c.InPK || c.InSK || c.InPart {
			return ClassJoinKey
		}
		if lowCard {
			return ClassLabel
		}
		return ClassFreeText
	case isNumeric(base):
		if idName.MatchString(c.Name) {
			return ClassJoinKey
		}
		return ClassMeasure
	default:
		// OTel-shaped attribute maps before the generic fail-closed check:
		// the keys carry the analytical signal, values get masked per-value.
		if _, ok := attrMapKey(c.Type); ok {
			return ClassAttrMap
		}
		// complex / aggregate / geo / anything unrecognized: keep only when
		// provably string-free, else fail closed.
		if stringBearing(c.Type) {
			return ClassSchemaless
		}
		return ClassMeasure // pure-value complex type (Array(UInt64), Tuple of dates, ...)
	}
}

// quoteIdent backtick-quotes a real identifier for use in trusted-side SQL.
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// nullWrap wraps expr to preserve NULLs of a Nullable source column.
func nullWrap(nullable bool, col, expr string) string {
	if !nullable {
		return expr
	}
	return fmt.Sprintf("if(isNull(%s), NULL, %s)", col, expr)
}

// semconvKeyPattern matches OpenTelemetry semantic-convention attribute KEY
// namespaces (dot-separated, a public standard) whose NAMES are safe to keep
// real. Custom (non-semconv) keys are tokenized instead (P3) so internal
// product/customer/feature names don't leak. Extensible.
const semconvKeyPattern = `^(service|telemetry|otel|http|db|rpc|messaging|network|net|url|server|client|source|destination|exception|code|thread|process|host|os|cloud|k8s|container|faas|event|session|enduser|user|peer|gen_ai|error|deployment|device|browser|feature_flag|log|span|trace)\.`

var semconvKeyRe = regexp.MustCompile(`(?i)` + semconvKeyPattern)

// IsSemconvKey reports whether an attribute key name is a known OTel
// semantic-convention key (kept real); everything else is custom (tokenized).
func IsSemconvKey(key string) bool { return semconvKeyRe.MatchString(key) }

// sqlLit escapes a string for a single-quoted ClickHouse literal.
func sqlLit(s string) string { return "'" + strings.ReplaceAll(s, "'", `\'`) + "'" }

// sqlInList renders a comma-separated quoted IN-list (empty -> "").
func sqlInList(ks []string) string {
	if len(ks) == 0 {
		return ""
	}
	qs := make([]string, len(ks))
	for i, k := range ks {
		qs[i] = sqlLit(k)
	}
	return strings.Join(qs, ", ")
}

// AttrMaskSpec carries the per-key info MaskExpr needs to mask an attrmap column
// role-awarely (P1) and tokenize custom keys (P3). All slices hold REAL key names
// observed during discovery; CustomToks is parallel to CustomKeys (each custom
// key's field_<hex> token, minted by the caller into the identifier map so the
// human report can de-tokenize it). A nil spec masks every value (fail-closed).
type AttrMaskSpec struct {
	VocabKeys   []string // value kept real (low-card vocabulary)
	MeasureKeys []string // value kept real IFF numeric
	CustomKeys  []string // non-semconv key names to tokenize
	CustomToks  []string // parallel field_<hex> tokens for CustomKeys
}

// MaskExpr returns the SELECT expression and the sandbox column type for one
// column. include=false means the column is excluded (fail closed).
// seed is the trusted-side value seed (never leaves the INSERT...SELECT).
// attr is required (non-nil) for ClassAttrMap and ignored otherwise.
func MaskExpr(c Column, class Class, seed uint64, attr *AttrMaskSpec) (expr, outType string, include bool) {
	q := quoteIdent(c.Name)
	_, nullable, lowCard := unwrap(c.Type)
	nn := q
	if nullable {
		nn = fmt.Sprintf("assumeNotNull(%s)", q)
	}
	hash := func(inner string) string { return fmt.Sprintf("sipHash64(%d, %s)", seed, inner) }
	maybeNullable := func(t string) string {
		if nullable {
			return "Nullable(" + t + ")"
		}
		return t
	}
	switch class {
	case ClassTime, ClassMeasure, ClassEnum:
		return q, c.Type, true
	case ClassJoinKey:
		base, _, _ := unwrap(c.Type)
		if isNumeric(base) {
			return nullWrap(nullable, q, hash(nn)), maybeNullable("UInt64"), true
		}
		// strings, UUID, IP, FixedString -> 16-hex string (toString first:
		// sipHash64 accepts strings, but UUID/IP need explicit conversion)
		return nullWrap(nullable, q, fmt.Sprintf("lower(hex(%s))", hash("toString("+nn+")"))),
			maybeNullable("String"), true
	case ClassLabel:
		// CH nests these as LowCardinality(Nullable(String)), not the reverse.
		t := maybeNullable("String")
		if lowCard {
			t = "LowCardinality(" + t + ")"
		}
		return nullWrap(nullable, q, fmt.Sprintf("substring(lower(hex(%s)), 1, 8)", hash("toString("+nn+")"))), t, true
	case ClassFreeText:
		return "'[redacted]'", maybeNullable("String"), true
	case ClassAttrMap:
		// Role-gated value masking (P1) + custom-key tokenization (P3).
		//   VALUE kept real only for a vocabulary key, or a measure key whose
		//   value is numeric. identity/sensitive AND unknown keys -> hashed (even
		//   numeric/bool/empty), so an identity key like user.id='12345' can't leak.
		//   KEY kept real only when it is a known OTel semconv key; observed custom
		//   keys map to their field_<hex> token (de-tokenizable for the human); any
		//   unobserved non-semconv key collapses to a sentinel so its name never
		//   leaks. A nil/empty spec masks every value (fail-closed).
		keyType, _ := attrMapKey(c.Type)
		if attr == nil {
			attr = &AttrMaskSpec{}
		}
		numRe := strings.ReplaceAll(MeasureValueSQL, `\`, `\\`)
		var keep []string
		if s := sqlInList(attr.VocabKeys); s != "" {
			keep = append(keep, "k IN ("+s+")")
		}
		if s := sqlInList(attr.MeasureKeys); s != "" {
			keep = append(keep, "(k IN ("+s+") AND match(v, '"+numRe+"'))")
		}
		keepPred := "0" // nothing kept -> every value hashed
		if len(keep) > 0 {
			keepPred = strings.Join(keep, " OR ")
		}
		valExpr := fmt.Sprintf(
			"arrayMap((k, v) -> if(%s, v, substring(lower(hex(%s)), 1, 12)), mapKeys(%s), mapValues(%s))",
			keepPred, hash("v"), q, q)
		semconvLit := sqlLit("(?i)" + strings.ReplaceAll(semconvKeyPattern, `\`, `\\`))
		keepKeyDefault := fmt.Sprintf("if(match(k, %s), k, 'field_redacted')", semconvLit)
		var keyExpr string
		if len(attr.CustomKeys) > 0 {
			toks := make([]string, len(attr.CustomToks))
			for i, t := range attr.CustomToks {
				toks[i] = sqlLit(t)
			}
			keyExpr = fmt.Sprintf("arrayMap(k -> transform(k, [%s], [%s], %s), mapKeys(%s))",
				sqlInList(attr.CustomKeys), strings.Join(toks, ", "), keepKeyDefault, q)
		} else {
			keyExpr = fmt.Sprintf("arrayMap(k -> %s, mapKeys(%s))", keepKeyDefault, q)
		}
		return fmt.Sprintf("mapFromArrays(%s, %s)", keyExpr, valExpr),
			fmt.Sprintf("Map(%s, String)", keyType), true
	default: // ClassSchemaless
		return "", "", false
	}
}

// AttrRole is the per-key semantic role of an attribute-map (Map) key. Unlike a
// column Class it is decided from the KEY name + the distribution of its VALUES,
// and it drives two things: how the value is masked, and how the LLM should use
// the key (told via the profile / describe_attributes).
type AttrRole string

const (
	// RoleVocabulary: low-cardinality categorical vocabulary (event.name, model).
	// Value KEPT REAL — the LLM may filter AND group; the human sees real values.
	RoleVocabulary AttrRole = "vocabulary"
	// RoleIdentity: names a person/entity/secret (PII denylist). Value MASKED —
	// the LLM may GROUP BY it (relabels to real for the human) but must NOT filter
	// on a literal; prefer it over an opaque id for "by X" breakdowns.
	RoleIdentity AttrRole = "identity"
	// RoleMeasure: values are numeric. KEPT (by the numeric masking rule) — aggregate.
	RoleMeasure AttrRole = "measure"
	// RoleSensitive: high-cardinality non-numeric free text. Value MASKED — avoid.
	RoleSensitive AttrRole = "sensitive"
)

// DefaultPIIKeyPattern matches attribute keys that name a person, entity, or
// secret — forced to RoleIdentity (masked) regardless of cardinality, because a
// LOW-cardinality key can still be sensitive (e.g. organization, user.email).
// It deliberately targets only what cardinality would otherwise wrongly keep:
//   - substrings for unambiguous PII words (email/account/organization/secret/…)
//   - boundary-gated segments for the short ambiguous ones (user/id/ip/org)
// It does NOT list bare 'token'/'auth': those over-match numeric measures
// (output_tokens, cache_read_tokens) and vocabulary (auth_method); real secrets
// are high-cardinality and fall to RoleSensitive (also masked) on their own.
// Bare-numeric-named identity keys (account/customer/tenant/zip/phone…) are
// forced to identity here even when their values are numeric, so the per-key
// role gate in MaskExpr masks them despite the numeric-keep rule.
const DefaultPIIKeyPattern = `(?i)(email|phone|ssn|passw|secret|credential|cookie|apikey|api[._-]?key|access[._-]?key|authorization|bearer|uuid|guid|hostname|account|organization|tenant|customer|workspace|zip|postal|address|latitude|longitude|geo|fingerprint)|(^|[._-])(user|id|ip|org|sid|session)([._-]|$)`

// measureValueRe (strict): a pure number with at most one decimal point — so a
// dotted version string like "10.0.26200" is NOT treated as a measure.
var measureValueRe = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// MeasureValueSQL is the same strict numeric test as a ClickHouse expression,
// for computing the numeric fraction of a key's values in the discovery query.
const MeasureValueSQL = `^-?[0-9]+(\.[0-9]+)?$`

// ClassifyAttrKey assigns a role to one attribute key. denyRe (PII) is checked
// FIRST (safety), then numeric→measure, then low-cardinality→vocabulary, else
// sensitive. cardinality and numericFraction come from a source-side scan of the
// key's values; threshold is the vocabulary cardinality ceiling.
func ClassifyAttrKey(key string, cardinality uint64, numericFraction float64, threshold uint64, denyRe *regexp.Regexp) AttrRole {
	if denyRe != nil && denyRe.MatchString(key) {
		return RoleIdentity
	}
	if numericFraction >= 0.9 {
		return RoleMeasure
	}
	if cardinality <= threshold {
		return RoleVocabulary
	}
	return RoleSensitive
}

var _ = measureValueRe // reserved for in-process classification if ever needed
