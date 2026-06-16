// Package discover orchestrates the full v1 pipeline:
//
//	shape → roster → columns → relations → mining → archetype → demotion →
//	roles → OBSERVE identifiers → build map → write profile (tokens only) →
//	masking plan → sandbox materialization → verification → manifest (LAST)
//
// The two-phase observe→build discipline (from the collector) guarantees the
// same identifier maps to the same token in structured profile columns and
// inside tokenized SQL shapes.
package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Altinity/anon-discovery/internal/chclient"
	"github.com/Altinity/anon-discovery/internal/classify"
	"github.com/Altinity/anon-discovery/internal/idmap"
	"github.com/Altinity/anon-discovery/internal/sqllex"
	"github.com/Altinity/anon-discovery/internal/store"
	"github.com/Altinity/anon-discovery/internal/token"
)

type Config struct {
	// Source and Dest are whitespace-split command prefixes that accept
	// clickhouse-client flags, e.g. "cl otel" / "clickhouse-client
	// --connection demo". Equal strings mean single-cluster mode.
	Source string
	Dest   string
	// SourceDB is the ONE database to mirror (required).
	SourceDB string
	// DestDB names the sandbox database on the dest cluster. Default =
	// SourceDB ("mirror" semantics) — which discloses the DB name (see the
	// disclosure rule in observe). Tables/columns inside are always
	// token-named regardless.
	DestDB       string
	MetaDB       string
	WindowDays   int
	SampleRows   uint64
	ServiceUsers []string
	// KeepAttrKeys is an operator allowlist of attrmap KEYS whose String values
	// are kept verbatim instead of hashed (low-card categorical vocabulary like
	// event.name/model that the LLM filters on and the human must de-anonymize).
	// Empty = mask all non-numeric attrmap values (original behavior).
	KeepAttrKeys []string
	HMACKey      []byte
	DryRun       bool
	Log          func(format string, args ...any)
}

type Run struct {
	Cfg   Config
	SrcEx chclient.Executor // discovery, mining, masking SELECTs (real names stay here)
	DstEx chclient.Executor // sandbox objects + tokens-only profile

	SrcStore *store.Store // trusted tables (identifier_map, masking_plan) on the source meta DB
	DstStore *store.Store // profile tables + registry + manifest on the dest meta DB

	RunID    string
	Version  string
	Shape    map[string]string
	ScopeDBs []string
	Tables   []*Table
	byFull   map[string]*Table

	Relations   []Relation
	Hot         []*HotTable
	HotCols     []HotColumn
	RepQueries  []RepQuery
	Conventions []Convention
	Notes       []string

	Minter *token.Minter
	IdMap  *idmap.IdMap
	Rw     *sqllex.Rewriter

	// per-table sandbox row counts (filled by sandbox phase)
	SandboxRows map[string]uint64

	started time.Time
}

// NewRun wires a run over injected executors (testability: fakes go here).
// The same meta-DB name is used on both clusters; the store split decides
// which tables exist where.
func NewRun(cfg Config, src, dst chclient.Executor) (*Run, error) {
	if cfg.SourceDB == "" {
		return nil, fmt.Errorf("discover: SourceDB is required")
	}
	if cfg.DestDB == "" {
		cfg.DestDB = cfg.SourceDB
	}
	if cfg.WindowDays <= 0 {
		cfg.WindowDays = 7
	}
	if cfg.SampleRows == 0 {
		cfg.SampleRows = 1_000_000
	}
	if cfg.MetaDB == "" {
		cfg.MetaDB = "altinity"
	}
	if cfg.DestDB == cfg.MetaDB {
		return nil, fmt.Errorf("discover: DestDB %q collides with the meta DB", cfg.DestDB)
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	m, err := token.NewMinter(cfg.HMACKey)
	if err != nil {
		return nil, err
	}
	return &Run{
		Cfg: cfg, SrcEx: src, DstEx: dst,
		SrcStore:    store.New(src, cfg.MetaDB),
		DstStore:    store.New(dst, cfg.MetaDB),
		Shape:       map[string]string{},
		byFull:      map[string]*Table{},
		Minter:      m,
		IdMap:       idmap.New(m),
		SandboxRows: map[string]uint64{},
	}, nil
}

// Execute runs the whole pipeline.
func (r *Run) Execute(ctx context.Context) error {
	r.started = time.Now().UTC()
	r.RunID = fmt.Sprintf("run-%s", r.started.Format("20060102-150405"))
	log := r.Cfg.Log

	if !r.Cfg.DryRun {
		if err := r.SrcStore.InitTrusted(ctx); err != nil {
			return err
		}
		if err := r.DstStore.InitProfile(ctx); err != nil {
			return err
		}
	}
	log("phase 0: shape")
	if err := r.shape(ctx); err != nil {
		return err
	}
	log("phase 1: roster")
	if err := r.roster(ctx); err != nil {
		return err
	}
	log("phase 1: %d databases, %d tables in scope", len(r.ScopeDBs), len(r.Tables))
	log("phase 3: columns")
	if err := r.columns(ctx); err != nil {
		return err
	}
	log("phase 4: relations")
	if err := r.relations(ctx); err != nil {
		return err
	}
	counts := CountTables(r.Tables)
	primary, rule := DetectPrimary(counts)
	secondaries := DetectSecondaries(counts, primary)
	r.Shape["archetype_primary"] = primary
	r.Shape["archetype_rule"] = rule
	r.Shape["archetype_secondaries"] = strings.Join(secondaries, ",")
	log("phase 1.5: archetype %s (%s) secondaries=[%s]", primary, rule, strings.Join(secondaries, ","))

	log("phase 5: mining (window %dd)", r.Cfg.WindowDays)
	if err := r.mining(ctx); err != nil {
		return err
	}
	log("phase 5: %d hot tables, %d hot columns, %d query shapes", len(r.Hot), len(r.HotCols), len(r.RepQueries))
	log("phase 6: demotion")
	r.demote(primary, counts.BizTabs)

	log("map: observe identifiers")
	if err := r.observe(ctx); err != nil {
		return err
	}
	log("map: build")
	if err := r.IdMap.Build(); err != nil {
		return err
	}
	if r.IdMap.Collisions > 0 {
		r.Notes = append(r.Notes, fmt.Sprintf("%d 8-hex token collision groups widened to 16 hex", r.IdMap.Collisions))
	}

	if r.Cfg.DryRun {
		log("dry-run: stopping before writes (map has %d entries)", len(r.IdMap.Pairs()))
		return nil
	}

	log("write: masking plan")
	plans, err := r.writeMaskingPlan(ctx)
	if err != nil {
		return err
	}
	log("sandbox: materialize")
	if err := r.sandbox(ctx, plans); err != nil {
		return err
	}
	log("write: profile tables")
	if err := r.writeProfile(ctx); err != nil {
		return err
	}
	log("verify")
	if err := r.verify(ctx); err != nil {
		return err
	}
	log("manifest")
	return r.manifest(ctx)
}

// observe registers every identifier with the map. Wave 1: structured names.
// Wave 2: free-text SQL identifiers (DDL, view bodies, normalized queries) —
// anything not already structured is minted under the "sql" kind.
func (r *Run) observe(ctx context.Context) error {
	im := r.IdMap
	im.KeepVerbatim("db", "default")
	im.KeepVerbatim("db", "system")

	// DB-name disclosure rule: naming the dest sandbox database after the
	// source database is an operator decision to disclose that one name, so
	// the profile keeps it verbatim and agrees with the sandbox. Any other
	// dest name keeps the source DB tokenized. Tables and columns are
	// token-named either way.
	if r.Cfg.DestDB == r.Cfg.SourceDB {
		im.KeepVerbatim("db", r.Cfg.SourceDB)
		r.Shape["db_disclosure"] = "disclosed"
		r.Notes = append(r.Notes, "source DB name disclosed by operator choice (dest db = source db)")
	} else {
		r.Shape["db_disclosure"] = "tokenized"
	}

	for _, t := range r.Tables {
		im.Observe("db", t.Database)
		im.Observe("tbl", t.Name)
		for _, c := range t.Columns {
			im.Observe("col", c.Name)
		}
	}
	for _, rel := range r.Relations {
		im.Observe("db", rel.SrcDB)
		im.Observe("tbl", rel.SrcTbl)
		if rel.DstDB != "" {
			im.Observe("db", rel.DstDB)
		}
		if rel.DstTbl != "" {
			im.Observe("tbl", rel.DstTbl)
		}
		if rel.Kind == "dictionary" {
			im.Observe("dict", rel.SrcTbl)
		}
	}
	for _, h := range r.Hot {
		for _, u := range h.Users {
			im.Observe("user", u)
		}
	}
	for _, hc := range r.HotCols {
		im.Observe("col", hc.Column)
	}
	// cluster names
	if rows, err := r.SrcEx.Query(ctx, "SELECT DISTINCT cluster FROM system.clusters"); err == nil {
		for _, row := range rows.Data {
			im.Observe("cluster", str(row[0]))
		}
	}

	// wave 2: SQL identifiers. The keep registry carries the cluster's own
	// function/setting/engine/type/format vocabulary so those words are never
	// tokenized; combinators make sumIf/uniqArrayIf-style names vocabulary too.
	vocab, combinators, err := r.clusterVocab(ctx)
	if err != nil {
		return err
	}
	r.Rw = sqllex.NewRewriter(im, sqllex.NewKeepRegistry(vocab))
	r.Rw.Combinators = combinators
	known := im.Known(sqllex.Probe)
	observeSQL := func(text string) {
		for _, w := range r.Rw.IdentifierWords(text) {
			if !known[w] {
				im.Observe("sql", w)
			}
		}
	}
	for _, t := range r.Tables {
		observeSQL(t.CreateQuery)
		observeSQL(t.SortingKey)
		observeSQL(t.PartitionKey)
	}
	for _, q := range r.RepQueries {
		observeSQL(q.Normalized)
	}
	return nil
}

func (r *Run) clusterVocab(ctx context.Context) (vocab, combinators []string, err error) {
	for _, q := range []string{
		"SELECT name FROM system.functions",
		"SELECT alias_to FROM system.functions WHERE alias_to != ''",
		"SELECT name FROM system.settings",
		"SELECT name FROM system.merge_tree_settings",
		"SELECT name FROM system.table_engines",
		"SELECT name FROM system.data_type_families",
		"SELECT alias_to FROM system.data_type_families WHERE alias_to != ''",
		"SELECT name FROM system.formats",
	} {
		rows, err := r.SrcEx.Query(ctx, q)
		if err != nil {
			return nil, nil, fmt.Errorf("keep registry: %w", err)
		}
		for _, row := range rows.Data {
			vocab = append(vocab, str(row[0]))
		}
	}
	rows, err := r.SrcEx.Query(ctx, "SELECT name FROM system.aggregate_function_combinators")
	if err != nil {
		return nil, nil, fmt.Errorf("keep registry: %w", err)
	}
	for _, row := range rows.Data {
		if c := strings.ToUpper(str(row[0])); c != "" {
			combinators = append(combinators, c)
		}
	}
	return vocab, combinators, nil
}

// tok maps with fail-closed error wrapping.
func (r *Run) tok(kind, v string) (string, error) {
	t, err := r.IdMap.Map(kind, v)
	if err != nil {
		return "", fmt.Errorf("profile write: %w", err)
	}
	return t, nil
}

// rewriteKeyExpr tokenizes a key expression (sorting/partition key) — these
// are SQL fragments over column names.
func (r *Run) rewriteKeyExpr(expr string) string {
	if expr == "" {
		return ""
	}
	return r.Rw.Rewrite(expr, false)
}

func u64s(v uint64) *string { s := strconv.FormatUint(v, 10); return &s }
func b8(v bool) *string {
	s := "0"
	if v {
		s = "1"
	}
	return &s
}

// writeProfile emits all tokenized profile rows (dest) plus the identifier
// map (source — trusted side only).
func (r *Run) writeProfile(ctx context.Context) error {
	// identifier_map (trusted side: SOURCE meta DB only)
	var mapRows [][]*string
	for _, p := range r.IdMap.Pairs() {
		mapRows = append(mapRows, []*string{chclient.S(r.RunID), chclient.S(p[0]), chclient.S(p[1]), chclient.S(p[2])})
	}
	if err := r.SrcStore.Insert(ctx, "identifier_map", []string{"run_id", "kind", "original", "token"}, mapRows); err != nil {
		return err
	}

	// profile_shape
	var shapeRows [][]*string
	keys := make([]string, 0, len(r.Shape))
	for k := range r.Shape {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		shapeRows = append(shapeRows, []*string{chclient.S(r.RunID), chclient.S(k), chclient.S(r.Shape[k])})
	}
	if err := r.DstStore.Insert(ctx, "profile_shape", []string{"run_id", "key", "value"}, shapeRows); err != nil {
		return err
	}

	hotByFull := map[string]*HotTable{}
	for _, h := range r.Hot {
		hotByFull[h.Full] = h
	}

	// profile_catalog + profile_columns
	var catRows, colRows [][]*string
	for _, t := range r.Tables {
		dbTok, err := r.tok("db", t.Database)
		if err != nil {
			return err
		}
		tblTok, err := r.tok("tbl", t.Name)
		if err != nil {
			return err
		}
		role, conf := ClassifyRole(t)
		demoted, reasons, flags := false, []string{}, []string{}
		if h, ok := hotByFull[t.Full()]; ok {
			demoted, reasons = h.Demoted, h.DemoteReasons
			for _, fl := range h.ReviewFlags {
				tf, err := r.tokenizeFlag(fl)
				if err != nil {
					return err
				}
				flags = append(flags, tf)
			}
		}
		sbRows, sandboxed := r.SandboxRows[t.Full()]
		catRows = append(catRows, []*string{
			chclient.S(r.RunID), chclient.S(dbTok), chclient.S(tblTok),
			chclient.S(t.Engine), chclient.S(EngineFamily(t.Engine)),
			chclient.S(r.rewriteKeyExpr(t.SortingKey)), chclient.S(r.rewriteKeyExpr(t.PartitionKey)),
			u64s(t.TotalRows), u64s(t.TotalBytes),
			chclient.S(role), chclient.S(conf),
			b8(demoted), chclient.S(store.ChArray(reasons)), chclient.S(store.ChArray(flags)),
			b8(sandboxed), u64s(sbRows),
		})
		for i, c := range t.Columns {
			colTok, err := r.tok("col", c.Name)
			if err != nil {
				return err
			}
			class := classify.Classify(c)
			_, _, include := classify.MaskExpr(c, class, 0)
			colRows = append(colRows, []*string{
				chclient.S(r.RunID), chclient.S(dbTok), chclient.S(tblTok), chclient.S(colTok),
				u64s(uint64(i + 1)), chclient.S(r.Rw.Rewrite(c.Type, false)), chclient.S(string(class)),
				b8(c.InPK), b8(c.InSK), b8(c.InPart), b8(include),
			})
		}
	}
	if err := r.DstStore.Insert(ctx, "profile_catalog",
		[]string{"run_id", "db_token", "table_token", "engine", "engine_family",
			"sorting_key_tok", "partition_key_tok", "total_rows", "total_bytes",
			"role", "role_confidence", "demoted", "demote_reasons", "review_flags",
			"sandboxed", "sandbox_rows"}, catRows); err != nil {
		return err
	}
	if err := r.DstStore.Insert(ctx, "profile_columns",
		[]string{"run_id", "db_token", "table_token", "col_token", "position",
			"type_tok", "class", "in_pk", "in_sk", "in_part", "included"}, colRows); err != nil {
		return err
	}

	// profile_relations
	var relRows [][]*string
	for _, rel := range r.Relations {
		srcDB, err := r.tok("db", rel.SrcDB)
		if err != nil {
			return err
		}
		srcTbl, err := r.tok("tbl", rel.SrcTbl)
		if err != nil {
			return err
		}
		dstDB, dstTbl := "", ""
		if rel.DstDB != "" {
			if dstDB, err = r.tok("db", rel.DstDB); err != nil {
				return err
			}
		}
		if rel.DstTbl != "" {
			if dstTbl, err = r.tok("tbl", rel.DstTbl); err != nil {
				return err
			}
		}
		detail := ""
		if rel.Detail != "" {
			detail = r.Rw.Rewrite(rel.Detail, false)
		}
		relRows = append(relRows, []*string{
			chclient.S(r.RunID), chclient.S(rel.Kind),
			chclient.S(srcDB), chclient.S(srcTbl), chclient.S(dstDB), chclient.S(dstTbl),
			chclient.S(detail),
		})
	}
	if err := r.DstStore.Insert(ctx, "profile_relations",
		[]string{"run_id", "rel_kind", "src_db_tok", "src_tbl_tok", "dst_db_tok", "dst_tbl_tok", "detail"},
		relRows); err != nil {
		return err
	}

	// workload tables
	splitFull := func(full string) (string, string, error) {
		i := strings.Index(full, ".")
		if i < 0 {
			return "", "", fmt.Errorf("unqualified table %q in workload", full)
		}
		dbTok, err := r.tok("db", full[:i])
		if err != nil {
			return "", "", err
		}
		tblTok, err := r.tok("tbl", full[i+1:])
		if err != nil {
			return "", "", err
		}
		return dbTok, tblTok, nil
	}
	var wRows [][]*string
	for _, h := range r.Hot {
		dbTok, tblTok, err := splitFull(h.Full)
		if err != nil {
			return err
		}
		var usersTok []string
		for _, u := range h.Users {
			ut, err := r.tok("user", u)
			if err != nil {
				return err
			}
			usersTok = append(usersTok, ut)
		}
		wRows = append(wRows, []*string{
			chclient.S(r.RunID), chclient.S(dbTok), chclient.S(tblTok),
			u64s(h.Execs), u64s(h.TotalMs), u64s(h.Sels), u64s(h.Ins),
			chclient.S(store.ChArray(usersTok)),
		})
	}
	if err := r.DstStore.Insert(ctx, "profile_workload",
		[]string{"run_id", "db_token", "table_token", "execs", "total_ms", "sels", "ins", "users_tok"},
		wRows); err != nil {
		return err
	}
	var hcRows [][]*string
	for _, hc := range r.HotCols {
		dbTok, tblTok, err := splitFull(hc.Full)
		if err != nil {
			return err
		}
		colTok, err := r.tok("col", hc.Column)
		if err != nil {
			return err
		}
		hcRows = append(hcRows, []*string{
			chclient.S(r.RunID), chclient.S(dbTok), chclient.S(tblTok), chclient.S(colTok), u64s(hc.Touches),
		})
	}
	if err := r.DstStore.Insert(ctx, "profile_hot_columns",
		[]string{"run_id", "db_token", "table_token", "col_token", "touches"}, hcRows); err != nil {
		return err
	}
	var qRows [][]*string
	for _, q := range r.RepQueries {
		dbTok, tblTok, err := splitFull(q.Full)
		if err != nil {
			return err
		}
		qRows = append(qRows, []*string{
			chclient.S(r.RunID), chclient.S(dbTok), chclient.S(tblTok),
			chclient.S(q.Hash), chclient.S(r.Rw.Rewrite(q.Normalized, false)), u64s(q.Execs),
		})
	}
	if err := r.DstStore.Insert(ctx, "profile_queries",
		[]string{"run_id", "db_token", "table_token", "query_hash", "query_tok", "execs"}, qRows); err != nil {
		return err
	}
	var cvRows [][]*string
	for _, cv := range r.Conventions {
		dbTok, tblTok, err := splitFull(cv.Full)
		if err != nil {
			return err
		}
		cvRows = append(cvRows, []*string{
			chclient.S(r.RunID), chclient.S(dbTok), chclient.S(tblTok),
			chclient.S(cv.Metric), u64s(cv.Numerator), u64s(cv.Denominator), chclient.S(cv.Convention),
		})
	}
	return r.DstStore.Insert(ctx, "profile_conventions",
		[]string{"run_id", "db_token", "table_token", "metric", "numerator", "denominator", "convention"}, cvRows)
}

// shadowFlagRe extracts the real base-table name a shadow-traffic review flag
// references, so it can be tokenized before entering the LLM-readable profile.
var shadowFlagRe = regexp.MustCompile(`^shadow-traffic-vs-([^:\s]+\.[^:\s]+)`)

func (r *Run) tokenizeFlag(flag string) (string, error) {
	m := shadowFlagRe.FindStringSubmatch(flag)
	if m == nil {
		return flag, nil
	}
	db, tbl, _ := strings.Cut(m[1], ".")
	dbTok, err := r.tok("db", db)
	if err != nil {
		return "", err
	}
	tblTok, err := r.tok("tbl", tbl)
	if err != nil {
		return "", err
	}
	return strings.Replace(flag, m[1], dbTok+"."+tblTok, 1), nil
}

// manifest writes the completion marker — always LAST. It lands on the DEST
// meta DB, which the LLM-facing read path can see, so everything in it must
// be tokens or operator-disclosed values: scope_databases carries the DB
// TOKEN (== the real name only under the disclosure rule), the stats JSON
// records dest_db (the operator-named sandbox DB, which `anond verify` reads
// back to locate the sandbox), and connection records only the dest command —
// the source command could name the customer cluster.
func (r *Run) manifest(ctx context.Context) error {
	stats := map[string]any{
		"tables":         len(r.Tables),
		"relations":      len(r.Relations),
		"hot_tables":     len(r.Hot),
		"map_entries":    len(r.IdMap.Pairs()),
		"map_collisions": r.IdMap.Collisions,
		"sandboxed":      len(r.SandboxRows),
		"version":        r.Version,
		"dest_db":        r.Cfg.DestDB,
		"db_disclosure":  r.Shape["db_disclosure"],
	}
	js, _ := json.Marshal(stats)
	scopeToks := make([]string, 0, len(r.ScopeDBs))
	for _, d := range r.ScopeDBs {
		tok, err := r.tok("db", d)
		if err != nil {
			return err
		}
		scopeToks = append(scopeToks, tok)
	}
	return r.DstStore.WriteManifest(ctx, r.RunID, map[string]string{
		"started":     r.started.Format("2006-01-02 15:04:05"),
		"finished":    time.Now().UTC().Format("2006-01-02 15:04:05"),
		"connection":  r.Cfg.Dest,
		"window_days": strconv.Itoa(r.Cfg.WindowDays),
		"sample_rows": strconv.FormatUint(r.Cfg.SampleRows, 10),
		"stats":       string(js),
	}, scopeToks, r.Notes)
}
