// Archetype detection — direct port of the profiler skill's Phase 1.5
// first-match-wins decision rules (primary) and halved-threshold secondaries.
package discover

import "strings"

type Counts struct {
	BizTabs                                 int
	Dist, View, MV, Summ, Agg, Repl         int
	Dict, Buffer, Kafka, Null, External, MT int
}

func (c Counts) share(n int) float64 {
	if c.BizTabs == 0 {
		return 0
	}
	return float64(n) / float64(c.BizTabs)
}

func CountTables(tables []*Table) Counts {
	var c Counts
	for _, t := range tables {
		c.BizTabs++
		switch t.Engine {
		case "Distributed":
			c.Dist++
		case "View", "LiveView":
			c.View++
		case "MaterializedView":
			c.MV++
		case "Dictionary":
			c.Dict++
		case "Buffer":
			c.Buffer++
		case "Kafka":
			c.Kafka++
		case "Null":
			c.Null++
		case "MergeTree":
			c.MT++
		}
		switch {
		case strings.Contains(t.Engine, "SummingMergeTree"):
			c.Summ++
		case strings.Contains(t.Engine, "AggregatingMergeTree"):
			c.Agg++
		case strings.Contains(t.Engine, "ReplacingMergeTree"):
			c.Repl++
		}
		if EngineFamily(t.Engine) == "external" {
			c.External++
		}
	}
	return c
}

// DetectPrimary applies the rules in order; first match wins.
func DetectPrimary(c Counts) (string, string) {
	viewShare, mvShare := c.share(c.View), c.share(c.MV)
	summShare, aggShare := c.share(c.Summ), c.share(c.Agg)
	dictShare, distShare, replShare, mtShare := c.share(c.Dict), c.share(c.Dist), c.share(c.Repl), c.share(c.MT)
	switch {
	case c.BizTabs > 5000:
		return "B", "rule-1 huge-enterprise"
	case viewShare > 0.70 && c.BizTabs > 500:
		return "C", "rule-2 view-warehouse"
	case c.Kafka > 15 || (c.Null > 20 && mvShare > 0.10):
		return "E", "rule-3 kafka-streaming"
	case c.External > 30 || (c.BizTabs > 0 && float64(c.External)/float64(c.BizTabs) > 0.30):
		return "E", "rule-4 federation"
	case dictShare > 0.03 && mvShare > 0.10:
		return "D", "rule-5 star-dict"
	case mvShare > 0.12 && (summShare > 0.02 || aggShare > 0.05):
		return "C", "rule-6 cube-mv"
	case distShare > 0.20 && replShare > 0.15:
		return "B", "rule-7 sharded-replacing"
	case distShare > 0.20:
		return "B", "rule-8 sharded-plain"
	case mvShare > 0.08 && (c.Buffer > 5 || mtShare > 0.40):
		return "C", "rule-9 realtime-mv"
	case c.BizTabs < 100:
		return "A", "rule-10 small-simple"
	default:
		return "A", "rule-11 plain-mt-fallback"
	}
}

// DetectSecondaries evaluates the other rules with thresholds halved.
func DetectSecondaries(c Counts, primary string) []string {
	viewShare, mvShare := c.share(c.View), c.share(c.MV)
	summShare, aggShare := c.share(c.Summ), c.share(c.Agg)
	dictShare, distShare, replShare, mtShare := c.share(c.Dict), c.share(c.Dist), c.share(c.Repl), c.share(c.MT)
	sec := map[string]bool{}
	if c.BizTabs > 2500 {
		sec["B"] = true
	}
	if viewShare > 0.35 && c.BizTabs > 250 {
		sec["C"] = true
	}
	if c.Kafka > 7 {
		sec["E"] = true
	}
	if c.Null > 10 && mvShare > 0.05 {
		sec["E"] = true
	}
	if c.External > 15 {
		sec["E"] = true
	}
	if c.BizTabs > 0 && float64(c.External)/float64(c.BizTabs) > 0.15 {
		sec["E"] = true
	}
	if dictShare > 0.015 && mvShare > 0.05 {
		sec["D"] = true
	}
	if mvShare > 0.06 && (summShare > 0.01 || aggShare > 0.025) {
		sec["C"] = true
	}
	if distShare > 0.10 && replShare > 0.075 {
		sec["B"] = true
	}
	if distShare > 0.10 {
		sec["B"] = true
	}
	if mvShare > 0.04 && (c.Buffer > 2 || mtShare > 0.20) {
		sec["C"] = true
	}
	delete(sec, primary)
	var out []string
	for _, k := range []string{"A", "B", "C", "D", "E"} {
		if sec[k] {
			out = append(out, k)
		}
	}
	return out
}
