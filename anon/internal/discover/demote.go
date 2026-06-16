// Demotion (phase 6) and role classification (phase 7) — ports of the
// profiler skill's pareto_cut.py and the phase-7 case statement. Review flags
// are mechanical detections emitted as data; they are never auto-resolved.
package discover

import (
	"fmt"
	"regexp"
	"strings"
)

var DefaultServiceUsers = []string{"default", "clickhouse", "airflow", "bot", "monitor", "oncall", ""}

var serviceUserPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^airflow`),
	regexp.MustCompile(`^bot`),
	regexp.MustCompile(`^monitor`),
	regexp.MustCompile(`^oncall`),
}

var infraEngines = map[string]bool{"Kafka": true, "Null": true, "Buffer": true, "MaterializedView": true}

var stagingSuffixes = []string{"_new", "_tmp", "_staging", "_old", "_backup"}
var innerPrefixes = []string{".inner.mv_", ".inner_id."}

var (
	perTenantRe     = regexp.MustCompile(`_[0-9a-f]{8,}_`)
	shadowTrafficRe = regexp.MustCompile(`_test_v\d+$`)
)

const (
	shadowRivalRatio       = 0.5
	bHugeEnterpriseBizTabs = 5000
	bHugeServiceFraction   = 0.9
)

func isServiceUser(u string, service map[string]bool) bool {
	if service[u] {
		return true
	}
	for _, p := range serviceUserPatterns {
		if p.MatchString(u) {
			return true
		}
	}
	return false
}

func allServiceUsers(users []string, service map[string]bool) bool {
	if len(users) == 0 {
		return false // no signal, don't demote
	}
	for _, u := range users {
		if !isServiceUser(u, service) {
			return false
		}
	}
	return true
}

func mostlyServiceUsers(users []string, service map[string]bool, fraction float64) bool {
	if len(users) == 0 {
		return false
	}
	n := 0
	for _, u := range users {
		if isServiceUser(u, service) {
			n++
		}
	}
	return float64(n)/float64(len(users)) >= fraction
}

func shortName(full string) string {
	if i := strings.Index(full, "."); i >= 0 {
		return full[i+1:]
	}
	return full
}

// Demote applies the phase-6 rules in place on r.Hot.
func (r *Run) demote(archetype string, bizTabs int) {
	service := map[string]bool{}
	users := r.Cfg.ServiceUsers
	if len(users) == 0 {
		users = DefaultServiceUsers
	}
	for _, u := range users {
		service[u] = true
	}
	hugeB := archetype == "B" && bizTabs > bHugeEnterpriseBizTabs

	baseExecs := map[string]uint64{}
	for _, h := range r.Hot {
		if !shadowTrafficRe.MatchString(shortName(h.Full)) {
			baseExecs[h.Full] = h.Execs
		}
	}

	for _, h := range r.Hot {
		short := shortName(h.Full)
		var reasons, review []string

		// Rule 1: insert-dominated
		total := h.Sels + h.Ins
		if h.Ins > 0 && total > 0 && float64(h.Ins)/float64(total) > 0.9 {
			reasons = append(reasons, "insert-dominated")
		}

		// Rule 2: service-users-only (or mostly-service for huge B)
		if hugeB {
			if mostlyServiceUsers(h.Users, service, bHugeServiceFraction) {
				reasons = append(reasons, "mostly-service-users (archetype B huge-enterprise)")
			}
		} else if allServiceUsers(h.Users, service) {
			reasons = append(reasons, "service-users-only")
		}

		// Rule 4: engine-by-nature
		if t, ok := r.byFull[h.Full]; ok && infraEngines[t.Engine] {
			reasons = append(reasons, "engine-is-infra:"+t.Engine)
		}
		for _, p := range innerPrefixes {
			if strings.HasPrefix(short, p) {
				reasons = append(reasons, "inner-mv-storage")
				break
			}
		}

		// Rule 5: staging naming, with the misleading-name caveat
		for _, s := range stagingSuffixes {
			if strings.HasSuffix(short, s) {
				selectDominated := total > 0 && float64(h.Sels)/float64(total) > 0.7
				humanSignal := len(h.Users) > 0 && !allServiceUsers(h.Users, service)
				if selectDominated && humanSignal {
					review = append(review, "misleading-staging-name: select-dominated by human users; kept hot — confirm with catalog owner")
				} else {
					reasons = append(reasons, "staging-name")
				}
				break
			}
		}

		// Caveat: shadow-traffic test sibling
		if m := shadowTrafficRe.FindString(short); m != "" {
			baseName := h.Full[:len(h.Full)-len(m)]
			if base, ok := baseExecs[baseName]; ok && base > 0 && float64(h.Execs) >= float64(base)*shadowRivalRatio {
				kept := reasons[:0]
				for _, x := range reasons {
					if x != "staging-name" {
						kept = append(kept, x)
					}
				}
				reasons = kept
				review = append(review, fmt.Sprintf("shadow-traffic-vs-%s: kept hot (execs=%d vs base=%d)", baseName, h.Execs, base))
			} else {
				review = append(review, "looks-like-test-sibling")
			}
		}

		// Caveat: per-tenant hash pattern
		if perTenantRe.MatchString(short) {
			review = append(review, "per-tenant-hash-pattern: summarize once, do not enumerate")
		}

		h.DemoteReasons, h.ReviewFlags = reasons, review
		h.Demoted = len(reasons) > 0
	}
}

// ClassifyRole assigns the phase-7 role + confidence for one table.
func ClassifyRole(t *Table) (role, confidence string) {
	switch t.Engine {
	case "Dictionary":
		return "Dim", "Confident"
	case "View", "LiveView":
		return "Mart", "Confident"
	case "MaterializedView", "Kafka", "Null", "Buffer":
		return "Pipeline", "Confident"
	}
	if strings.Contains(t.Engine, "MergeTree") || t.Engine == "Distributed" {
		hasTime := false
		for _, c := range t.Columns {
			ty := c.Type
			if strings.Contains(ty, "DateTime") || strings.Contains(ty, "Date") {
				hasTime = true
				break
			}
		}
		switch {
		case hasTime && t.TotalRows >= 10_000_000:
			return "Fact", "Confident"
		case hasTime && t.TotalRows >= 1_000_000:
			return "Fact", "Likely"
		case t.TotalRows > 0 && t.TotalRows < 100_000 && lookupShaped(t):
			return "Dim", "Likely"
		}
	}
	return "Other", "Other"
}

// lookupShaped: small table with an id-ish column and a name/title column.
func lookupShaped(t *Table) bool {
	hasID, hasName := false, false
	for _, c := range t.Columns {
		n := strings.ToLower(c.Name)
		if idName.MatchString(n) || c.InPK {
			hasID = true
		}
		if strings.Contains(n, "name") || strings.Contains(n, "title") || strings.Contains(n, "label") {
			hasName = true
		}
	}
	return hasID && hasName
}

var idName = regexp.MustCompile(`(?i)(^|_)(id|uuid|guid|key)s?$`)
