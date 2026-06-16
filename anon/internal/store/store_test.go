package store

import "testing"

// The trusted/profile split must stay total and disjoint: every meta table
// belongs to exactly one group and has DDL — a table silently missing from
// both groups would never be created (or worse, a trusted table slipping
// into ProfileTables would land on the LLM-exposed cluster).
func TestTableGroupsPartitionDDL(t *testing.T) {
	seen := map[string]string{}
	for _, n := range TrustedTables {
		seen[n] = "trusted"
	}
	for _, n := range ProfileTables {
		if g, dup := seen[n]; dup {
			t.Errorf("table %q in both %s and profile groups", n, g)
		}
		seen[n] = "profile"
	}
	if len(seen) != len(ddl) {
		t.Errorf("groups cover %d tables, ddl defines %d", len(seen), len(ddl))
	}
	for n := range seen {
		if _, ok := ddl[n]; !ok {
			t.Errorf("no DDL for grouped table %q", n)
		}
	}
	for n := range ddl {
		if _, ok := seen[n]; !ok {
			t.Errorf("DDL table %q not assigned to a group", n)
		}
	}
	if len(MetaTables) != len(ddl) {
		t.Errorf("MetaTables has %d entries, ddl %d", len(MetaTables), len(ddl))
	}
	for _, trusted := range TrustedTables {
		for tbl := range ProfileContentColumns {
			if tbl == trusted {
				t.Errorf("trusted table %q listed as LLM-readable profile content", trusted)
			}
		}
	}
}
