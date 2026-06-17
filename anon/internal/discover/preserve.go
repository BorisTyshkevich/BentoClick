// Schema-preserving model execution (anond --model=schema-preserving).
//
// Unlike the tokenizing pipeline, this keeps real table/column NAMES and masks
// only VALUES, so domain tools query the sandbox by its real schema. It is the
// Go home of what system-anon/gen.py did, generalized to any source DB:
//
//	shape → roster → columns → per-table {classify → CREATE real-named table with
//	masked SELECT → populate} → mint reversible value tokens into the secret
//	identifier_map → write the bentoclick.schema_guide registry → manifest.
package discover

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Altinity/anon-discovery/internal/chclient"
	"github.com/Altinity/anon-discovery/internal/classify"
	"github.com/Altinity/anon-discovery/internal/store"
)

// preservePlan is the materialization plan for one table (pure data, no IO).
type preservePlan struct {
	Table    *Table
	Cols     []string // "<expr> AS <name>" for the masking SELECT
	OrderBy  []string // real column names kept for ORDER BY
	WindowOn string   // real temporal column to window on (or "")
	Mints    []string // SELECT statements yielding (kind, original, token) for identifier_map
	RegRows  [][]*string
}

// buildPreserve computes the schema-preserving plan for one table — PURE
// (string building only), so it is fully unit-testable without a cluster.
func buildPreserve(t *Table, seed, anonDB, runID string, overrides map[string]string) preservePlan {
	p := preservePlan{Table: t}
	kept := map[string]bool{}
	for i, c := range t.Columns {
		class := classify.ClassifyPreserve(c, overrides)
		expr, include := classify.PreserveMaskExpr(c, class, seed)
		if !include {
			continue
		}
		// keep/measure exprs are the bare column; tok/redact already alias themselves.
		if class == "keep" {
			p.Cols = append(p.Cols, fmt.Sprintf("%s AS %s", quoteCol(c.Name), quoteCol(c.Name)))
		} else {
			p.Cols = append(p.Cols, expr)
		}
		kept[c.Name] = true
		if sel, mint := classify.PreserveMintSelect(c, class, seed, t.Database, t.Name); mint {
			p.Mints = append(p.Mints, sel)
		}
		regClass, usage, ok := classify.RegistryClassPreserving(class)
		if ok {
			p.RegRows = append(p.RegRows, []*string{
				s(runID), s(anonDB), s("schema-preserving"), s("real"),
				s(t.Name), s(""), u64s(0), u64s(0), u64s(uint64(i + 1)),
				s(c.Name), s(c.Type), s(regClass), s(usage),
			})
		}
	}
	// ORDER BY: kept sorting-key columns (bare names), real.
	for _, part := range strings.Split(t.SortingKey, ",") {
		n := strings.Trim(strings.TrimSpace(part), "`")
		if kept[n] {
			p.OrderBy = append(p.OrderBy, quoteCol(n))
		}
	}
	// window on a kept temporal column in the partition/sorting key.
	for _, c := range t.Columns {
		if !kept[c.Name] {
			continue
		}
		if (c.InPart || c.InSK) && isPreserveTemporal(c.Type) {
			p.WindowOn = c.Name
			break
		}
	}
	return p
}

func isPreserveTemporal(t string) bool {
	return strings.Contains(t, "Date") || strings.Contains(t, "DateTime")
}

func quoteCol(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

// registrySchemaCols is the bentoclick.schema_guide column list both models write.
var registrySchemaCols = []string{
	"run_id", "anon_database", "model", "naming", "table_name", "table_role",
	"total_rows", "sandbox_rows", "position", "column_name", "type", "class", "usage",
}

// executePreserve runs the schema-preserving pipeline end to end.
func (r *Run) executePreserve(ctx context.Context) error {
	r.started = time.Now().UTC()
	r.RunID = fmt.Sprintf("run-%s", r.started.Format("20060102-150405"))
	log := r.Cfg.Log
	log("schema-preserving model: source-db=%s dest-db=%s", r.Cfg.SourceDB, r.Cfg.DestDB)

	// reads (no writes): shape → roster → columns
	if err := r.shape(ctx); err != nil {
		return err
	}
	if err := r.roster(ctx); err != nil {
		return err
	}
	if err := r.columns(ctx); err != nil {
		return err
	}
	log("schema-preserving: %d tables in scope", len(r.Tables))
	if r.Cfg.DryRun {
		log("dry-run: stopping before writes")
		return nil
	}

	// secret store (identifier_map + masking_plan) and the LLM-facing registry.
	secret := store.New(r.SrcEx, r.Cfg.SecretDB)
	if err := secret.InitTrusted(ctx); err != nil {
		return err
	}
	reg := store.New(r.DstEx, r.Cfg.RegistryDB)
	seed := strconv.FormatUint(r.Minter.ValueSeed(), 10)
	sbDB := r.Cfg.DestDB

	dbReady := false
	var allReg [][]*string
	var allMints, maskRows [][]*string
	for _, t := range r.Tables {
		if !sandboxEligible(t) {
			continue
		}
		ov := r.Cfg.ColumnOverrides[t.Name]
		p := buildPreserve(t, seed, sbDB, r.RunID, ov)
		if len(p.Cols) == 0 {
			r.Notes = append(r.Notes, fmt.Sprintf("table %s: all columns dropped, no sandbox table", t.Name))
			continue
		}
		if !dbReady {
			if err := r.ensureSandboxDB(ctx, sbDB); err != nil {
				return err
			}
			dbReady = true
		}
		if err := r.materializePreserve(ctx, sbDB, t, p); err != nil {
			r.Notes = append(r.Notes, fmt.Sprintf("schema-preserving %s.%s failed: %s", sbDB, t.Name, firstLine(err.Error())))
			continue
		}
		allReg = append(allReg, p.RegRows...)
		// masking_plan rows (trusted side): real names + the run's class.
		for _, c := range t.Columns {
			cl := classify.ClassifyPreserve(c, ov)
			_, inc := classify.PreserveMaskExpr(c, cl, seed)
			maskRows = append(maskRows, []*string{
				s(r.RunID), s(t.Database), s(t.Name), s(c.Name), s(string(cl)), s(""), b8x(inc),
			})
		}
	}
	// mint reversible value tokens into the secret identifier_map.
	for _, t := range r.Tables {
		ov := r.Cfg.ColumnOverrides[t.Name]
		for _, c := range t.Columns {
			cl := classify.ClassifyPreserve(c, ov)
			if sel, mint := classify.PreserveMintSelect(c, cl, seed, t.Database, t.Name); mint {
				rows, err := r.SrcEx.Query(ctx, sel)
				if err != nil {
					r.Notes = append(r.Notes, fmt.Sprintf("mint %s.%s failed: %s", t.Name, c.Name, firstLine(err.Error())))
					continue
				}
				for _, row := range rows.Data {
					if len(row) >= 3 {
						allMints = append(allMints, []*string{s(r.RunID), row[0], row[1], row[2]})
					}
				}
			}
		}
	}
	if len(allMints) > 0 {
		if err := secret.Insert(ctx, "identifier_map", []string{"run_id", "kind", "original", "token"}, allMints); err != nil {
			return err
		}
	}
	if len(maskRows) > 0 {
		if err := secret.Insert(ctx, "masking_plan",
			[]string{"run_id", "database", "table", "column", "class", "transform", "included"}, maskRows); err != nil {
			return err
		}
	}
	if len(allReg) > 0 {
		if err := reg.Insert(ctx, "schema_guide_data", registrySchemaCols, allReg); err != nil {
			return err
		}
	}
	log("schema-preserving complete: %d sandbox tables, %d registry rows, %d token mints",
		len(r.SandboxRows), len(allReg), len(allMints))
	return nil
}

func (r *Run) ensureSandboxDB(ctx context.Context, sbDB string) error {
	dbExists := fmt.Sprintf("SELECT count() FROM system.databases WHERE name = '%s'", sqlStr(sbDB))
	if _, err := r.ensureOurs(ctx, "database", sbDB, dbExists); err != nil {
		return err
	}
	if err := r.DstStore.RegisterObject(ctx, r.RunID, "database", sbDB); err != nil {
		return err
	}
	return r.DstEx.Exec(ctx, "CREATE DATABASE IF NOT EXISTS "+qident(sbDB))
}

// materializePreserve creates the real-named table and populates it with masked
// values, reusing the same-cluster INSERT...SELECT / cross-cluster TSV stream.
func (r *Run) materializePreserve(ctx context.Context, sbDB string, t *Table, p preservePlan) error {
	full := sbDB + "." + t.Name
	tblExists := fmt.Sprintf("SELECT count() FROM system.tables WHERE database = '%s' AND name = '%s'",
		sqlStr(sbDB), sqlStr(t.Name))
	exists, err := r.ensureOurs(ctx, "table", full, tblExists)
	if err != nil {
		return err
	}
	if err := r.DstStore.RegisterObject(ctx, r.RunID, "table", full); err != nil {
		return err
	}
	if exists {
		if err := r.DstEx.Exec(ctx, fmt.Sprintf("DROP TABLE %s.%s", qident(sbDB), qident(t.Name))); err != nil {
			return err
		}
	}
	orderBy := "tuple()"
	if len(p.OrderBy) > 0 {
		orderBy = strings.Join(p.OrderBy, ", ")
	}
	// CREATE ... AS SELECT infers real types from the masked SELECT (CASTs keep them).
	where := ""
	if p.WindowOn != "" {
		where = fmt.Sprintf(" WHERE %s >= now() - INTERVAL %d DAY", qident(p.WindowOn), r.Cfg.WindowDays)
	}
	sel := fmt.Sprintf("SELECT %s FROM %s.%s%s LIMIT %d",
		strings.Join(p.Cols, ", "), qident(t.Database), qident(t.Name), where, r.Cfg.SampleRows)
	create := fmt.Sprintf("CREATE TABLE %s.%s ENGINE = MergeTree ORDER BY (%s) AS %s",
		qident(sbDB), qident(t.Name), orderBy, sel)
	if err := r.DstEx.Exec(ctx, create); err != nil {
		return fmt.Errorf("create %s: %w", full, err)
	}
	cnt, err := r.DstEx.Query(ctx, fmt.Sprintf("SELECT count() FROM %s.%s", qident(sbDB), qident(t.Name)))
	if err != nil {
		return err
	}
	r.SandboxRows[t.Full()] = u64(cnt.Data[0][0])
	return nil
}

// registryAttrCols is the bentoclick.attr_guide column list.
var registryAttrCols = []string{"run_id", "anon_database", "table_name", "column_name", "attr_key", "role", "usage"}

// writeSchemaGuideTokenizing writes registry rows for the tokenizing model
// (token table/column names, naming='tokens') into the RegistryDB.schema_guide_data
// backing table and the attrmap key roles into RegistryDB.attr_guide_data (the
// schema_guide/attr_guide views read these *_data tables FINAL).
func (r *Run) writeSchemaGuideTokenizing(ctx context.Context) error {
	reg := store.New(r.DstEx, r.Cfg.RegistryDB)
	anonDB := r.Cfg.DestDB
	var rows [][]*string
	for _, t := range r.Tables {
		if !sandboxEligible(t) {
			continue
		}
		tblTok, err := r.tok("tbl", t.Name)
		if err != nil {
			return err
		}
		role, _ := ClassifyRole(t)
		sbRows := r.SandboxRows[t.Full()]
		for i, c := range t.Columns {
			class := classify.Classify(c)
			regClass, usage, ok := classify.RegistryClassTokenizing(class)
			if !ok {
				continue
			}
			colTok, err := r.tok("col", c.Name)
			if err != nil {
				return err
			}
			rows = append(rows, []*string{
				s(r.RunID), s(anonDB), s("tokenizing"), s("tokens"),
				s(tblTok), s(role), u64s(t.TotalRows), u64s(sbRows), u64s(uint64(i + 1)),
				s(colTok), s(r.Rw.Rewrite(c.Type, false)), s(regClass), s(usage),
			})
		}
	}
	if len(rows) > 0 {
		if err := reg.Insert(ctx, "schema_guide_data", registrySchemaCols, rows); err != nil {
			return err
		}
	}
	// attr_guide: token db/table/col, sandbox attr key (semconv real / custom
	// tokenized), role + usage.
	var aRows [][]*string
	for _, a := range r.AttrKeyRoles {
		tblTok, err := r.tok("tbl", a.Table)
		if err != nil {
			return err
		}
		colTok, err := r.tok("col", a.Column)
		if err != nil {
			return err
		}
		aRows = append(aRows, []*string{
			s(r.RunID), s(anonDB), s(tblTok), s(colTok), s(a.outKey()), s(a.Role), s(attrUsage(a.Role)),
		})
	}
	if len(aRows) > 0 {
		return reg.Insert(ctx, "attr_guide_data", registryAttrCols, aRows)
	}
	return nil
}

func attrUsage(role string) string {
	switch role {
	case "vocabulary":
		return "real value: filter and group"
	case "measure":
		return "real number: aggregate"
	case "identity":
		return "masked: GROUP BY only, relabels to real for the human"
	case "sensitive":
		return "masked free text: avoid"
	}
	return role
}

var _ = chclient.S // chclient used for S() helper parity across the package
