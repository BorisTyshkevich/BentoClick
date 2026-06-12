// Catalog discovery: phases 0/1/3/4 of the profiler pipeline — cluster shape,
// database roster, per-table metadata, columns, and structural relations
// (Distributed targets, MV TO-chains, view FROM/JOIN references, dictionaries).
package discover

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/Altinity/anon-discovery/internal/chclient"
	"github.com/Altinity/anon-discovery/internal/classify"
	"github.com/Altinity/anon-discovery/internal/token"
)

// excludedDBs are never profiled. Our own generated objects (meta DB, token
// DBs) are excluded dynamically in roster().
var excludedDBs = []string{
	"system", "information_schema", "INFORMATION_SCHEMA", "_temporary_and_external_tables",
}

type Table struct {
	Database, Name, Engine, EngineFull string
	CreateQuery                        string
	PartitionKey, SortingKey           string
	TotalRows, TotalBytes              uint64
	Columns                            []classify.Column
}

func (t *Table) Full() string { return t.Database + "." + t.Name }

// EngineFamily buckets engines for archetype counting and sandbox eligibility.
func EngineFamily(engine string) string {
	switch {
	case strings.Contains(engine, "MergeTree"):
		return "mergetree"
	case engine == "Distributed":
		return "distributed"
	case engine == "MaterializedView":
		return "mv"
	case engine == "View", engine == "LiveView":
		return "view"
	case engine == "Dictionary":
		return "dictionary"
	case engine == "Kafka", engine == "Null", engine == "Buffer":
		return "infra"
	case engine == "MySQL", engine == "PostgreSQL", engine == "URL", engine == "S3",
		engine == "S3Queue", engine == "HDFS", engine == "MongoDB", engine == "Redis":
		return "external"
	default:
		return "other"
	}
}

type Relation struct {
	Kind          string // dist_to | mv_to | view_from | view_join | dictionary
	SrcDB, SrcTbl string
	DstDB, DstTbl string
	Detail        string // shard key, dict layout, ...
}

func inList(dbs []string) string {
	parts := make([]string, len(dbs))
	for i, d := range dbs {
		parts[i] = "'" + strings.ReplaceAll(d, "'", "\\'") + "'"
	}
	return strings.Join(parts, ",")
}

// shape: phase 0 — version + query_log presence.
func (r *Run) shape(ctx context.Context) error {
	rows, err := r.Ex.Query(ctx, "SELECT version()")
	if err != nil {
		return fmt.Errorf("phase0: %w", err)
	}
	r.Version = cell(rows, 0, 0)
	r.Shape["version"] = r.Version

	qr, err := r.Ex.Query(ctx, fmt.Sprintf(
		"SELECT count() FROM system.query_log WHERE type = 'QueryFinish' AND event_date >= today() - %d", r.Cfg.WindowDays))
	if err != nil {
		r.Shape["qlog"] = "unavailable"
		r.Notes = append(r.Notes, "system.query_log unavailable: catalog-only mode")
	} else {
		r.Shape["qlog_rows"] = cell(qr, 0, 0)
	}
	return nil
}

// roster: phase 1 — pick scope databases and load table metadata.
func (r *Run) roster(ctx context.Context) error {
	rows, err := r.Ex.Query(ctx, fmt.Sprintf(`
		SELECT database, name, engine, engine_full, create_table_query,
		       partition_key, sorting_key,
		       coalesce(total_rows, 0), coalesce(total_bytes, 0)
		FROM system.tables
		WHERE database NOT IN (%s)`, inList(excludedDBs)))
	if err != nil {
		return fmt.Errorf("phase1 roster: %w", err)
	}
	wantDB := map[string]bool{}
	for _, d := range r.Cfg.Databases {
		wantDB[d] = true
	}
	for _, row := range rows.Data {
		db := str(row[0])
		if db == r.Cfg.MetaDB || db == "" {
			continue
		}
		if token.ReservedRe.MatchString(db) {
			continue // our own (or a previous run's) sandbox DB
		}
		if len(wantDB) > 0 && !wantDB[db] {
			continue
		}
		t := &Table{
			Database: db, Name: str(row[1]), Engine: str(row[2]), EngineFull: str(row[3]),
			CreateQuery: str(row[4]), PartitionKey: str(row[5]), SortingKey: str(row[6]),
			TotalRows: u64(row[7]), TotalBytes: u64(row[8]),
		}
		r.Tables = append(r.Tables, t)
		r.byFull[t.Full()] = t
	}
	dbset := map[string]bool{}
	for _, t := range r.Tables {
		dbset[t.Database] = true
	}
	for d := range dbset {
		r.ScopeDBs = append(r.ScopeDBs, d)
	}
	if len(r.ScopeDBs) == 0 {
		return fmt.Errorf("phase1: no business databases in scope")
	}
	return nil
}

// columns: phase 3 — per-column metadata for every scope table.
func (r *Run) columns(ctx context.Context) error {
	rows, err := r.Ex.Query(ctx, fmt.Sprintf(`
		SELECT database, table, name, type, position,
		       is_in_partition_key, is_in_sorting_key, is_in_primary_key
		FROM system.columns
		WHERE database IN (%s)
		ORDER BY database, table, position`, inList(r.ScopeDBs)))
	if err != nil {
		return fmt.Errorf("phase3 columns: %w", err)
	}
	for _, row := range rows.Data {
		full := str(row[0]) + "." + str(row[1])
		t, ok := r.byFull[full]
		if !ok {
			continue
		}
		t.Columns = append(t.Columns, classify.Column{
			Name: str(row[2]), Type: str(row[3]),
			InPart: str(row[5]) == "1", InSK: str(row[6]) == "1", InPK: str(row[7]) == "1",
		})
	}
	return nil
}

var (
	distRe = regexp.MustCompile(`Distributed\('([^']*)',\s*'([^']*)',\s*'([^']*)'(?:,\s*(.+))?\)\s*$`)
	mvToRe = regexp.MustCompile(`\bTO\s+(?:` + "`" + `([^` + "`" + `]+)` + "`" + `|(\w+))\.(?:` + "`" + `([^` + "`" + `]+)` + "`" + `|(\w+))`)
	fromRe = regexp.MustCompile(`(?i)\bFROM\s+(?:(\w+)\.)?(\w+)`)
	joinRe = regexp.MustCompile(`(?i)\bJOIN\s+(?:(\w+)\.)?(\w+)`)
)

func pick(m []string, a, b int) string {
	if m[a] != "" {
		return m[a]
	}
	return m[b]
}

// relations: phase 4 — structural relations from engine_full / DDL / system.dictionaries.
func (r *Run) relations(ctx context.Context) error {
	for _, t := range r.Tables {
		switch t.Engine {
		case "Distributed":
			if m := distRe.FindStringSubmatch(t.EngineFull); m != nil {
				r.Relations = append(r.Relations, Relation{
					Kind: "dist_to", SrcDB: t.Database, SrcTbl: t.Name,
					DstDB: m[2], DstTbl: m[3], Detail: strings.TrimSpace(m[4]),
				})
			}
		case "MaterializedView":
			if m := mvToRe.FindStringSubmatch(t.CreateQuery); m != nil {
				r.Relations = append(r.Relations, Relation{
					Kind: "mv_to", SrcDB: t.Database, SrcTbl: t.Name,
					DstDB: pick(m, 1, 2), DstTbl: pick(m, 3, 4),
				})
			}
			r.viewRefs(t)
		case "View", "LiveView":
			r.viewRefs(t)
		}
	}
	// dictionaries
	rows, err := r.Ex.Query(ctx, fmt.Sprintf(
		"SELECT database, name, type, status FROM system.dictionaries WHERE database IN (%s)", inList(r.ScopeDBs)))
	if err == nil {
		for _, row := range rows.Data {
			r.Relations = append(r.Relations, Relation{
				Kind: "dictionary", SrcDB: str(row[0]), SrcTbl: str(row[1]),
				Detail: str(row[2]) + "/" + str(row[3]),
			})
		}
	}
	return nil
}

// viewRefs extracts FROM/JOIN table references from a view body. Identifier
// resolution is heuristic (regex over DDL, per the profiler skill phase 4d);
// referents are verified for existence before they reach the profile.
func (r *Run) viewRefs(t *Table) {
	body := t.CreateQuery
	if i := strings.Index(strings.ToUpper(body), " AS SELECT"); i >= 0 {
		body = body[i:]
	}
	seen := map[string]bool{}
	add := func(kind string, m []string) {
		db := m[1]
		if db == "" {
			db = t.Database
		}
		full := db + "." + m[2]
		if full == t.Full() || seen[full] {
			return
		}
		if _, exists := r.byFull[full]; !exists {
			return // existence check: unresolvable referent never enters the profile
		}
		seen[full] = true
		r.Relations = append(r.Relations, Relation{
			Kind: kind, SrcDB: t.Database, SrcTbl: t.Name, DstDB: db, DstTbl: m[2],
		})
	}
	for _, m := range fromRe.FindAllStringSubmatch(body, -1) {
		add("view_from", m)
	}
	for _, m := range joinRe.FindAllStringSubmatch(body, -1) {
		add("view_join", m)
	}
}

func cell(r *chclient.Rows, row, col int) string {
	if r == nil || len(r.Data) <= row || r.Data[row][col] == nil {
		return ""
	}
	return *r.Data[row][col]
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func u64(p *string) uint64 {
	if p == nil {
		return 0
	}
	var v uint64
	fmt.Sscanf(*p, "%d", &v)
	return v
}
