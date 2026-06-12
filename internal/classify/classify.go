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

// MaskExpr returns the SELECT expression and the sandbox column type for one
// column. include=false means the column is excluded (fail closed).
// seed is the trusted-side value seed (never leaves the INSERT...SELECT).
func MaskExpr(c Column, class Class, seed uint64) (expr, outType string, include bool) {
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
	default: // ClassSchemaless
		return "", "", false
	}
}
