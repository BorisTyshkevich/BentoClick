// Workload mining — phase 5 of the profiler pipeline (raw query_log shape):
// hot tables (5a), co-occurrence with engine-resolution dedup (5b/5e), hot
// columns (5c), representative normalized queries (5d, literals stripped
// server-side by normalizeQuery + comment removal), and form-ratio mining
// (5d.1, port of synthesize_conventions.py's decision rules).
package discover

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Altinity/anon-discovery/internal/chclient"
)

type HotTable struct {
	Full    string
	Execs   uint64
	TotalMs uint64
	Sels    uint64
	Ins     uint64
	Users   []string
	// demotion output
	Demoted       bool
	DemoteReasons []string
	ReviewFlags   []string
}

type HotColumn struct {
	Full, Column string
	Touches      uint64
}

type RepQuery struct {
	Full, Hash, Normalized string
	Execs                  uint64
}

type Convention struct {
	Full, Metric           string
	Numerator, Denominator uint64
	Convention             string
}

const qlogSystemFilter = `splitByChar('.', t)[1] NOT IN
  ('system','information_schema','INFORMATION_SCHEMA','_temporary_and_external_tables','_table_function')`

// tokenDBPattern matches our own sandbox databases (mirror of token.ReservedRe).
const tokenDBPattern = `^(db|tbl|col|user|role|dict|cluster|disk|host|sql|field|enum)_[0-9a-f]{8,16}$`

// ownQueryFilter excludes the tool's own footprint from workload mining, two
// ways. (1) log_comment: every anond query self-tags with --log_comment, and
// in cross-cluster mode the masking SELECTs run on the SOURCE referencing
// only real tables — the token-DB filter cannot catch them, yet they carry
// the value seed and `AS col_<tok>` aliases; observed unfiltered, the next
// run's observe wave would see those aliases and the reserved-namespace guard
// would abort. (2) token-DB/meta-DB arrayExists: defense in depth and the
// single-cluster case, where a previous run's sandbox INSERTs and sandbox
// exploration touch token-pattern databases (observed live on the demo
// server) — and where queries from other tools (or pre-log_comment anond
// builds) may lack the tag.
func (r *Run) ownQueryFilter() string {
	return fmt.Sprintf(`NOT arrayExists(x -> match(splitByChar('.', x)[1], '%s')
	  OR splitByChar('.', x)[1] = '%s', tables)
	  AND log_comment != '%s'`,
		tokenDBPattern, strings.ReplaceAll(r.Cfg.MetaDB, "'", "\\'"), chclient.LogComment)
}

// mining runs all of phase 5. No-ops gracefully when the query log is empty
// (catalog-only mode, recorded in notes).
func (r *Run) mining(ctx context.Context) error {
	if r.Shape["qlog"] == "unavailable" || r.Shape["qlog_rows"] == "0" {
		r.Notes = append(r.Notes, "no query_log workload in window: catalog-only profile")
		return nil
	}
	if err := r.hotTables(ctx); err != nil {
		return err
	}
	if err := r.hotColumns(ctx); err != nil {
		return err
	}
	if err := r.repQueries(ctx); err != nil {
		return err
	}
	return r.formRatios(ctx)
}

// hotTables: 5a top-50 by execs with workload shape + users.
func (r *Run) hotTables(ctx context.Context) error {
	rows, err := r.SrcEx.Query(ctx, fmt.Sprintf(`
		SELECT t AS full_name,
		       count() AS execs,
		       sum(query_duration_ms) AS total_ms,
		       sumIf(1, query_kind = 'Select') AS sels,
		       sumIf(1, query_kind = 'Insert') AS ins,
		       arraySlice(arraySort(groupUniqArray(user)), 1, 8) AS users
		FROM system.query_log
		ARRAY JOIN tables AS t
		WHERE type = 'QueryFinish'
		  AND event_date >= today() - %d
		  AND %s
		  AND %s
		GROUP BY t ORDER BY execs DESC LIMIT 50`, r.Cfg.WindowDays, qlogSystemFilter, r.ownQueryFilter()))
	if err != nil {
		return fmt.Errorf("phase5a: %w", err)
	}
	for _, row := range rows.Data {
		full := str(row[0])
		if _, ok := r.byFull[full]; !ok {
			continue // table not in scope (or since dropped): never enters the profile
		}
		r.Hot = append(r.Hot, &HotTable{
			Full: full, Execs: u64(row[1]), TotalMs: u64(row[2]),
			Sels: u64(row[3]), Ins: u64(row[4]),
			Users: parseStrArray(str(row[5])),
		})
	}
	return nil
}

// hotColumns: 5c — column touch counts for the hot tables.
func (r *Run) hotColumns(ctx context.Context) error {
	if len(r.Hot) == 0 {
		return nil
	}
	var hotNames []string
	for _, h := range r.Hot {
		hotNames = append(hotNames, "'"+strings.ReplaceAll(h.Full, "'", "\\'")+"'")
	}
	rows, err := r.SrcEx.Query(ctx, fmt.Sprintf(`
		SELECT t, col, count() AS touches
		FROM (
		  SELECT arrayJoin(tables) AS t, arrayJoin(columns) AS col
		  FROM system.query_log
		  WHERE type = 'QueryFinish' AND event_date >= today() - %d
		    AND %s
		)
		WHERE t IN (%s) AND startsWith(col, concat(t, '.'))
		GROUP BY t, col
		ORDER BY t, touches DESC
		LIMIT 25 BY t`, r.Cfg.WindowDays, r.ownQueryFilter(), strings.Join(hotNames, ",")))
	if err != nil {
		return fmt.Errorf("phase5c: %w", err)
	}
	for _, row := range rows.Data {
		full, col := str(row[0]), str(row[1])
		col = strings.TrimPrefix(col, full+".")
		r.HotCols = append(r.HotCols, HotColumn{Full: full, Column: col, Touches: u64(row[2])})
	}
	return nil
}

// repQueries: 5d — top normalized query shapes for the top hot tables.
// normalizeQuery() strips literals server-side; comments are removed in the
// same expression, per the profiler skill's PII rule. The shapes still carry
// identifiers — tokenized later by the sqllex rewriter before storage.
func (r *Run) repQueries(ctx context.Context) error {
	top := r.Hot
	if len(top) > 15 {
		top = top[:15]
	}
	for _, h := range top {
		rows, err := r.SrcEx.Query(ctx, fmt.Sprintf(`
			SELECT toString(normalized_query_hash) AS h,
			       any(normalizeQuery(replaceRegexpAll(query, '/\\*.*?\\*/', ''))) AS clean,
			       count() AS execs
			FROM system.query_log
			WHERE type = 'QueryFinish'
			  AND event_date >= today() - %d
			  AND has(tables, '%s')
			  AND %s
			GROUP BY normalized_query_hash
			ORDER BY execs DESC LIMIT 3`, r.Cfg.WindowDays, strings.ReplaceAll(h.Full, "'", "\\'"), r.ownQueryFilter()))
		if err != nil {
			return fmt.Errorf("phase5d %s: %w", h.Full, err)
		}
		for _, row := range rows.Data {
			r.RepQueries = append(r.RepQueries, RepQuery{
				Full: h.Full, Hash: str(row[0]), Normalized: str(row[1]), Execs: u64(row[2]),
			})
		}
	}
	return nil
}

// formRatios: 5d.1 — form-level writing conventions on the top hot fact.
// Port of synthesize_conventions.py decision rules (>0.5 dominance).
func (r *Run) formRatios(ctx context.Context) error {
	if len(r.Hot) == 0 {
		return nil
	}
	top := r.Hot[0].Full
	rows, err := r.SrcEx.Query(ctx, fmt.Sprintf(`
		WITH q AS (
		  SELECT lower(replaceRegexpAll(query, '/\\*.*?\\*/', '')) AS body, count() AS execs
		  FROM system.query_log
		  WHERE type = 'QueryFinish'
		    AND event_date >= today() - %d
		    AND query_kind = 'Select'
		    AND has(tables, '%s')
		    AND %s
		  GROUP BY body
		)
		SELECT
		  sumIf(execs, match(body, '\\bprewhere\\b'))                      AS uses_prewhere,
		  sumIf(execs, match(body, '\\bwhere\\b') AND NOT match(body, '\\bprewhere\\b')) AS uses_where_only,
		  sumIf(execs, match(body, '\\bfinal\\b'))                         AS uses_final,
		  sumIf(execs, match(body, '\\bargmax\\s*\\('))                    AS uses_argmax,
		  sumIf(execs, match(body, '\\banylast\\s*\\('))                   AS uses_anylast,
		  sumIf(execs, match(body, '\\btodate\\s*\\('))                    AS uses_todate,
		  sumIf(execs, match(body, '\\btostartofday\\s*\\('))              AS uses_tostartofday,
		  sumIf(execs, match(body, '\\bselect\\s+\\*'))                    AS uses_select_star,
		  sum(execs)                                                       AS total
		FROM q`, r.Cfg.WindowDays, strings.ReplaceAll(top, "'", "\\'"), r.ownQueryFilter()))
	if err != nil {
		return fmt.Errorf("phase5d1: %w", err)
	}
	if len(rows.Data) == 0 {
		return nil
	}
	g := func(i int) uint64 { return u64(rows.Data[0][i]) }
	total := g(8)
	add := func(metric string, num, den uint64, conv string) {
		r.Conventions = append(r.Conventions, Convention{
			Full: top, Metric: metric, Numerator: num, Denominator: den, Convention: conv,
		})
	}
	prewhere, whereOnly := g(0), g(1)
	add("prewhere", prewhere, prewhere+whereOnly,
		dominant(prewhere, prewhere+whereOnly, "prewhere-idiom", "no-dominant-form"))
	argmax, anylast := g(3), g(4)
	add("anylast_vs_argmax", anylast, anylast+argmax,
		dominant(anylast, anylast+argmax, "anylast-idiom", "no-dominant-form"))
	todate, tosod := g(5), g(6)
	add("todate_vs_tostartofday", todate, todate+tosod,
		dominant(todate, todate+tosod, "todate-idiom", "no-dominant-form"))
	star := g(7)
	conv := "narrow-projections"
	if total > 0 && float64(star)/float64(total) > 0.05 {
		conv = "wide-projections-present"
	}
	add("select_star", star, total, conv)
	add("final", g(2), total, "")
	return nil
}

func dominant(num, den uint64, win, lose string) string {
	if den == 0 {
		return "no-signal"
	}
	if float64(num)/float64(den) > 0.5 {
		return win
	}
	return lose
}

// parseStrArray parses a CH Array(String) TSV cell: ['a','b'].
func parseStrArray(s string) []string {
	if len(s) < 2 || s[0] != '[' || s[len(s)-1] != ']' {
		return nil
	}
	body := s[1 : len(s)-1]
	var out []string
	i := 0
	for i < len(body) {
		for i < len(body) && (body[i] == ',' || body[i] == ' ') {
			i++
		}
		if i >= len(body) || body[i] != '\'' {
			break
		}
		i++
		var b strings.Builder
		for i < len(body) {
			if body[i] == '\\' && i+1 < len(body) {
				b.WriteByte(body[i+1])
				i += 2
				continue
			}
			if body[i] == '\'' {
				i++
				break
			}
			b.WriteByte(body[i])
			i++
		}
		out = append(out, b.String())
	}
	sort.Strings(out)
	return out
}
