// Verification (phase 7.5, v1 subset): existence re-checks for every
// identifier the profile references, and leak checks on the emitted sandbox
// DDL. Behavior probes (EXPLAIN, query_log round-trips) and inferred-join
// cardinality probes are deferred — v1 relations are structural, and the
// deferral is recorded in the manifest notes.
package discover

import (
	"context"
	"fmt"
	"strings"

	"github.com/Altinity/anon-discovery/internal/chclient"
)

func (r *Run) verify(ctx context.Context) error {
	var rows [][]*string
	add := func(claim, subject, status, detail string) {
		rows = append(rows, []*string{
			chclient.S(r.RunID), chclient.S(claim), chclient.S(subject),
			chclient.S(status), chclient.S(detail),
		})
	}

	// 7.5a existence: every table the profile references still exists.
	missing := 0
	res, err := r.Ex.Query(ctx, fmt.Sprintf(
		"SELECT concat(database, '.', name) FROM system.tables WHERE database IN (%s)", inList(r.ScopeDBs)))
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, row := range res.Data {
		live[str(row[0])] = true
	}
	for _, t := range r.Tables {
		if !live[t.Full()] {
			missing++
			// tokenized subject: profile_verification is LLM-readable
			dbTok, _ := r.IdMap.Lookup("db", t.Database)
			tblTok, _ := r.IdMap.Lookup("tbl", t.Name)
			add("existence", dbTok+"."+tblTok, "dropped-since-discovery", "table vanished between catalog and verification")
		}
	}
	add("existence", "scope", "verified", fmt.Sprintf("%d tables checked, %d vanished", len(r.Tables), missing))

	// Sandbox leak check: SHOW CREATE of every sandbox table must contain no
	// real identifier and no seed digits. This is the structural property the
	// sandbox was chosen for — assert it, don't assume it.
	leaks := 0
	for full := range r.SandboxRows {
		t := r.byFull[full]
		dbTok, _ := r.IdMap.Lookup("db", t.Database)
		tblTok, _ := r.IdMap.Lookup("tbl", t.Name)
		sc, err := r.Ex.Query(ctx, fmt.Sprintf("SHOW CREATE TABLE %s.%s", qident(dbTok), qident(tblTok)))
		if err != nil {
			return err
		}
		ddl := cell(sc, 0, 0)
		if leak := findLeak(ddl, t, r.Minter.ValueSeed()); leak != "" {
			leaks++
			add("sandbox-leak", dbTok+"."+tblTok, "LEAK", leak)
		}
	}
	if leaks > 0 {
		// fail the run loudly — a leaking sandbox is worse than no sandbox
		return fmt.Errorf("verify: %d sandbox tables leak real identifiers or the seed in SHOW CREATE; run aborted before manifest", leaks)
	}
	add("sandbox-leak", "all", "verified", fmt.Sprintf("%d sandbox tables checked", len(r.SandboxRows)))

	r.Notes = append(r.Notes,
		"behavior probes (EXPLAIN/query_log) and join-cardinality probes deferred: v1 relations are structural only")
	return r.St.Insert(ctx, "profile_verification",
		[]string{"run_id", "claim_type", "subject", "status", "detail"}, rows)
}

// findLeak scans emitted DDL for the table's real identifiers or the seed.
func findLeak(ddl string, t *Table, seed uint64) string {
	if strings.Contains(ddl, fmt.Sprintf("%d", seed)) {
		return "value seed present"
	}
	for _, name := range append([]string{t.Database, t.Name}, colNames(t)...) {
		if len(name) < 4 {
			continue // single-letter names would false-positive on tokens
		}
		if strings.Contains(ddl, name) {
			return "real identifier present (len " + fmt.Sprint(len(name)) + ")"
		}
	}
	return ""
}

func colNames(t *Table) []string {
	out := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		out[i] = c.Name
	}
	return out
}
