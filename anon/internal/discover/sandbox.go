// Sandbox materialization: ONE operator-named database on the DEST cluster
// holding physical token-named tables of masked data, populated by streaming
// masked TSV from the source cluster.
//
// Leak model: a materialized table's SHOW CREATE exposes only token columns +
// engine. The masking expressions — which embed the value seed and real
// table/column names — appear only in SELECTs executed on the SOURCE cluster
// (its query_log is trusted), never in any object or log on the dest cluster.
// (This is why the sandbox replaced live masking views: view bodies are
// readable by any SELECT-granted user, verified on CH 26.3.)
//
// Safety: an object name is only ever created after (a) registering it in the
// dest registry (generated_objects) and (b) confirming any existing object
// with that name is ours. We never DROP or replace a foreign object.
package discover

import (
	"context"
	"fmt"
	"strings"

	"github.com/Altinity/anon-discovery/internal/classify"
)

type colPlan struct {
	Col      classify.Column
	Class    classify.Class
	Expr     string // trusted-side masking expression (contains seed + real name)
	OutType  string
	ColToken string
	Include  bool
}

type tablePlan struct {
	T       *Table
	DBTok   string
	TblTok  string
	Cols    []colPlan
	OrderBy []string // token names for the sandbox ORDER BY
}

// sandboxEligible: physical/queryable data surfaces only. Infra engines, MVs
// and external engines are never materialized; plain views are skipped in v1
// (materializing one executes arbitrary SQL — revisit deliberately).
func sandboxEligible(t *Table) bool {
	switch EngineFamily(t.Engine) {
	case "mergetree", "distributed":
		return true
	}
	return false
}

// writeMaskingPlan computes the per-column plan for every scope table, stores
// it (REAL names — trusted side), and returns the plans for materialization.
func (r *Run) writeMaskingPlan(ctx context.Context) ([]*tablePlan, error) {
	seed := r.Minter.ValueSeed()
	var plans []*tablePlan
	var rows [][]*string
	for _, t := range r.Tables {
		dbTok, err := r.tok("db", t.Database)
		if err != nil {
			return nil, err
		}
		tblTok, err := r.tok("tbl", t.Name)
		if err != nil {
			return nil, err
		}
		p := &tablePlan{T: t, DBTok: dbTok, TblTok: tblTok}
		for _, c := range t.Columns {
			class := classify.Classify(c)
			// Per attrmap column: classify each key (PII denylist + cardinality)
			// into a role, then build a role-gated mask spec — vocabulary values
			// kept real, measure values kept iff numeric, identity/sensitive
			// masked (P1). Custom (non-semconv) key NAMES are tokenized to a
			// field_<hex> token minted into the identifier map (P3). KeepAttrKeys
			// is an operator force-keep (treated as vocabulary). On query failure,
			// the spec stays empty so every value masks (fail-closed).
			var spec *classify.AttrMaskSpec
			if class == classify.ClassAttrMap {
				ms := classify.AttrMaskSpec{}
				infos, qerr := r.attrRolesFor(ctx, t.Database, t.Name, c.Name, tableTimeCol(t))
				if qerr != nil {
					r.Notes = append(r.Notes, fmt.Sprintf("attr-key roles failed for %s.%s.%s: %s (masking all non-override values)", t.Database, t.Name, c.Name, firstLine(qerr.Error())))
				} else {
					for i := range infos {
						switch classify.AttrRole(infos[i].Role) {
						case classify.RoleVocabulary:
							ms.VocabKeys = append(ms.VocabKeys, infos[i].Key)
						case classify.RoleMeasure:
							ms.MeasureKeys = append(ms.MeasureKeys, infos[i].Key)
						}
						if classify.IsSemconvKey(infos[i].Key) {
							infos[i].KeyOut = infos[i].Key // standard key name kept real
						} else {
							ftok, terr := r.tok("field", infos[i].Key)
							if terr != nil {
								return nil, terr
							}
							infos[i].KeyOut = ftok
							ms.CustomKeys = append(ms.CustomKeys, infos[i].Key)
							ms.CustomToks = append(ms.CustomToks, ftok)
						}
					}
					r.AttrKeyRoles = append(r.AttrKeyRoles, infos...)
				}
				ms.VocabKeys = append(ms.VocabKeys, r.Cfg.KeepAttrKeys...)
				spec = &ms
			}
			expr, outType, include := classify.MaskExpr(c, class, seed, spec)
			colTok, err := r.tok("col", c.Name)
			if err != nil {
				return nil, err
			}
			p.Cols = append(p.Cols, colPlan{
				Col: c, Class: class, Expr: expr, OutType: outType,
				ColToken: colTok, Include: include,
			})
			rows = append(rows, []*string{
				s(r.RunID), s(t.Database), s(t.Name), s(c.Name),
				s(string(class)), s(expr), b8x(include),
			})
		}
		p.OrderBy = sandboxOrderBy(p)
		plans = append(plans, p)
	}
	// de-anon secret: the plan carries real names + masking expressions →
	// SOURCE secret DB (never the meta/registry DB the LLM can reach)
	if err := r.SecretStore.Insert(ctx, "masking_plan",
		[]string{"run_id", "database", "table", "column", "class", "transform", "included"}, rows); err != nil {
		return nil, err
	}
	return plans, nil
}

// sandboxOrderBy maps the original sorting-key COLUMNS (bare names only;
// expressions are skipped) to their token names, keeping only included
// columns. Hashed keys still serve point lookups and joins; kept time columns
// preserve range locality.
func sandboxOrderBy(p *tablePlan) []string {
	included := map[string]string{}
	for _, cp := range p.Cols {
		if cp.Include {
			included[cp.Col.Name] = cp.ColToken
		}
	}
	var out []string
	for _, part := range strings.Split(p.T.SortingKey, ",") {
		name := strings.TrimSpace(part)
		name = strings.Trim(name, "`")
		if tok, ok := included[name]; ok {
			out = append(out, tok)
		}
	}
	return out
}

// timeFilterCol picks a Date/DateTime column from the partition or sorting
// key for window-bounded sampling, if any survives masking.
func timeFilterCol(p *tablePlan) string {
	for _, cp := range p.Cols {
		if cp.Class == classify.ClassTime && (cp.Col.InPart || cp.Col.InSK) {
			return cp.Col.Name
		}
	}
	return ""
}

// tableTimeCol is the raw-Table analog of timeFilterCol: the partition/sorting-key
// time column to window the attr-key cardinality scan to (same window the sandbox
// uses), or "" if none. Usable before the masking plan exists.
func tableTimeCol(t *Table) string {
	for _, c := range t.Columns {
		if classify.Classify(c) == classify.ClassTime && (c.InPart || c.InSK) {
			return c.Name
		}
	}
	return ""
}

// ensureOurs aborts if an object with this name exists on the DEST cluster
// but is not registered in the dest registry. Returns whether the object
// already exists.
func (r *Run) ensureOurs(ctx context.Context, kind, name, existsQuery string) (bool, error) {
	rows, err := r.DstEx.Query(ctx, existsQuery)
	if err != nil {
		return false, err
	}
	exists := len(rows.Data) > 0 && str(rows.Data[0][0]) != "0"
	if !exists {
		return false, nil
	}
	ours, err := r.DstStore.IsRegistered(ctx, kind, name)
	if err != nil {
		return true, err
	}
	if !ours {
		return true, fmt.Errorf("safety: %s %q already exists and was not created by this tool; aborting (never touch foreign objects)", kind, name)
	}
	return true, nil
}

func qident(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

func sqlStr(v string) string {
	return strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), `'`, `\'`)
}

// sandbox materializes all eligible tables into the single operator-named
// dest database, streaming masked rows source→dest.
func (r *Run) sandbox(ctx context.Context, plans []*tablePlan) error {
	sbDB := r.Cfg.DestDB
	dbReady := false
	for _, p := range plans {
		if !sandboxEligible(p.T) {
			continue
		}
		var cols, exprs []string
		for _, cp := range p.Cols {
			if !cp.Include {
				continue
			}
			cols = append(cols, fmt.Sprintf("%s %s", cp.ColToken, cp.OutType))
			exprs = append(exprs, fmt.Sprintf("%s AS %s", cp.Expr, cp.ColToken))
		}
		if len(cols) == 0 {
			r.Notes = append(r.Notes, fmt.Sprintf("table %s.%s: all columns excluded, no sandbox table", p.DBTok, p.TblTok))
			continue
		}
		orderBy := "tuple()"
		if len(p.OrderBy) > 0 {
			orderBy = strings.Join(p.OrderBy, ", ")
		}

		// database (once; operator-named, so the registry — not a token
		// pattern — is what marks it ours)
		if !dbReady {
			dbExists := fmt.Sprintf("SELECT count() FROM system.databases WHERE name = '%s'", sqlStr(sbDB))
			if _, err := r.ensureOurs(ctx, "database", sbDB, dbExists); err != nil {
				return err
			}
			if err := r.DstStore.RegisterObject(ctx, r.RunID, "database", sbDB); err != nil {
				return err
			}
			if err := r.DstEx.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+qident(sbDB)); err != nil {
				return err
			}
			dbReady = true
		}

		// table (recreate OUR OWN table for refresh; foreign names abort above)
		fullTok := sbDB + "." + p.TblTok
		tblExists := fmt.Sprintf("SELECT count() FROM system.tables WHERE database = '%s' AND name = '%s'",
			sqlStr(sbDB), sqlStr(p.TblTok))
		exists, err := r.ensureOurs(ctx, "table", fullTok, tblExists)
		if err != nil {
			return err
		}
		if err := r.DstStore.RegisterObject(ctx, r.RunID, "table", fullTok); err != nil {
			return err
		}
		if exists {
			if err := r.DstEx.Exec(ctx, fmt.Sprintf("DROP TABLE %s.%s", qident(sbDB), qident(p.TblTok))); err != nil {
				return err
			}
		}
		create := fmt.Sprintf("CREATE TABLE %s.%s (%s) ENGINE = MergeTree ORDER BY (%s)",
			qident(sbDB), qident(p.TblTok), strings.Join(cols, ", "), orderBy)
		if err := r.DstEx.Exec(ctx, create); err != nil {
			return fmt.Errorf("sandbox create %s: %w", fullTok, err)
		}

		// populate — the masked SELECT (seed + real names) is evaluated inside
		// ClickHouse; real values never reach this process. Same-cluster uses a
		// server-side INSERT ... SELECT; cross-cluster streams masked TSV.
		populate := func(where string) error {
			sel := fmt.Sprintf("SELECT %s FROM %s.%s%s LIMIT %d",
				strings.Join(exprs, ", "),
				qident(p.T.Database), qident(p.T.Name), where, r.Cfg.SampleRows)
			dst := qident(sbDB) + "." + qident(p.TblTok)
			if r.Cfg.Source == r.Cfg.Dest {
				// Same cluster: copy entirely server-side via INSERT ... SELECT —
				// no Go round-trip, no TSV. The masked SELECT (seed + real names)
				// runs inside ClickHouse only. An empty result inserts 0 rows
				// (no error), so the count()==0 fallback below still applies.
				return r.DstEx.Exec(ctx, "INSERT INTO "+dst+" "+sel)
			}
			// Cross-cluster: stream masked TSV from source to dest; only masked
			// rows cross the wire. Positional insert (SELECT emits columns in
			// table-definition order; types parse back from TSV by construction).
			rc, err := r.SrcEx.QueryStream(ctx, sel)
			if err != nil {
				return err
			}
			insErr := r.DstEx.InsertStream(ctx, dst, nil, rc)
			if cerr := rc.Close(); insErr == nil {
				insErr = cerr // surface a source-side failure too
			}
			// A zero-row source stream makes the dest client error with
			// NO_DATA_TO_INSERT (Code 108). That is "0 rows", not a failure —
			// returning nil here lets the count()==0 fallback below decide
			// (unwindowed retry, or a legitimately empty source table).
			if insErr != nil && strings.Contains(insErr.Error(), "NO_DATA_TO_INSERT") {
				return nil
			}
			return insErr
		}
		count := func() (uint64, error) {
			cnt, err := r.DstEx.Query(ctx, fmt.Sprintf("SELECT count() FROM %s.%s", qident(sbDB), qident(p.TblTok)))
			if err != nil {
				return 0, err
			}
			return u64(cnt.Data[0][0]), nil
		}
		where := ""
		if tc := timeFilterCol(p); tc != "" {
			where = fmt.Sprintf(" WHERE %s >= now() - INTERVAL %d DAY", qident(tc), r.Cfg.WindowDays)
		}
		if err := populate(where); err != nil {
			// Per-table failure (exotic type, permissions) must not kill the
			// run: record and continue; the table simply isn't sandboxed.
			r.Notes = append(r.Notes, fmt.Sprintf("sandbox populate failed for %s: %s", fullTok, firstLine(err.Error())))
			continue
		}
		n, err := count()
		if err != nil {
			return err
		}
		if n == 0 && where != "" && p.T.TotalRows > 0 {
			// static/old dataset: the recency window emptied the sample.
			// Fall back to unwindowed first-N.
			if err := populate(""); err != nil {
				r.Notes = append(r.Notes, fmt.Sprintf("sandbox unwindowed fallback failed for %s: %s", fullTok, firstLine(err.Error())))
				continue
			}
			if n, err = count(); err != nil {
				return err
			}
			r.Notes = append(r.Notes, fmt.Sprintf("sandbox %s: no rows in the %d-day window, sampled unwindowed first-N instead", fullTok, r.Cfg.WindowDays))
		}
		r.SandboxRows[p.T.Full()] = n
	}
	if r.Cfg.SampleRows > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("sandbox sampled at most %d rows per table (time-window filter when a key time column exists, else first-N): distributions of larger tables are biased", r.Cfg.SampleRows))
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func s(v string) *string { return &v }

func b8x(v bool) *string {
	out := "0"
	if v {
		out = "1"
	}
	return &out
}
