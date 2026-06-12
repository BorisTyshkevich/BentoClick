// Package idmap is the per-kind injective identifier map (Go port of the
// collector's idmap.py, with HMAC tokens instead of sequential ones).
//
// Two phases: Observe() every identifier value across all sources, then
// Build() derives tokens and verifies injectivity (8-hex collisions re-mint
// the whole colliding group at 16 hex; a 16-hex collision aborts). Identity
// ("keep verbatim") entries cover universal vocabulary — the `system` and
// `default` databases — so structured columns and free-text SQL stay
// consistent without tokenizing names every cluster shares.
package idmap

import (
	"fmt"
	"sort"

	"github.com/Altinity/anon-discovery/internal/token"
)

type IdMap struct {
	minter     *token.Minter
	values     map[string]map[string]bool // kind -> set(original)
	keep       map[string]map[string]bool // kind -> set(identity-kept)
	fwd        map[string]map[string]string
	Collisions int // 8-hex collision groups widened (recorded in manifest)
}

func New(m *token.Minter) *IdMap {
	return &IdMap{
		minter: m,
		values: map[string]map[string]bool{},
		keep:   map[string]map[string]bool{},
		fwd:    map[string]map[string]string{},
	}
}

// KeepVerbatim maps a value to itself: universal vocabulary identical on
// every cluster (system/default DBs). Never tokenized even if observed.
func (im *IdMap) KeepVerbatim(kind, value string) {
	if value == "" {
		return
	}
	if im.keep[kind] == nil {
		im.keep[kind] = map[string]bool{}
	}
	im.keep[kind][value] = true
}

func (im *IdMap) Observe(kind, value string) {
	if value == "" || im.keep[kind][value] {
		return
	}
	if im.values[kind] == nil {
		im.values[kind] = map[string]bool{}
	}
	im.values[kind][value] = true
}

// Build derives all tokens. Errors on: a real identifier inside the reserved
// token namespace (would break word-level de-tokenization), or an
// unresolvable 16-hex collision.
func (im *IdMap) Build() error {
	im.fwd = map[string]map[string]string{}
	for kind, set := range im.values {
		m := map[string]string{}
		byTok := map[string][]string{}
		for v := range set {
			if token.ReservedRe.MatchString(v) {
				return fmt.Errorf("idmap: real identifier %q (kind %s) collides with the reserved token namespace; aborting", v, kind)
			}
			tok := im.minter.Token(kind, v, token.HexLen)
			byTok[tok] = append(byTok[tok], v)
		}
		for tok, vs := range byTok {
			if len(vs) == 1 {
				m[vs[0]] = tok
				continue
			}
			im.Collisions++
			wide := map[string]bool{}
			for _, v := range vs {
				wt := im.minter.Token(kind, v, token.HexLenWide)
				if wide[wt] {
					return fmt.Errorf("idmap: 16-hex token collision in kind %s; aborting", kind)
				}
				wide[wt] = true
				m[v] = wt
			}
		}
		for v := range im.keep[kind] {
			m[v] = v
		}
		im.fwd[kind] = m
	}
	// kinds that only have keep entries
	for kind, set := range im.keep {
		if im.fwd[kind] == nil {
			m := map[string]string{}
			for v := range set {
				m[v] = v
			}
			im.fwd[kind] = m
		}
	}
	return nil
}

// Map returns the token for (kind, value). Fail-closed: unknown values error
// (the caller observed incompletely — a bug, not a runtime condition).
func (im *IdMap) Map(kind, value string) (string, error) {
	if value == "" {
		return value, nil
	}
	tok, ok := im.fwd[kind][value]
	if !ok {
		return "", fmt.Errorf("idmap: unobserved %s identifier (len %d)", kind, len(value))
	}
	return tok, nil
}

// Lookup is Map without the error: ok=false for unknown values.
func (im *IdMap) Lookup(kind, value string) (string, bool) {
	tok, ok := im.fwd[kind][value]
	return tok, ok
}

// Known returns every observed value across probeKinds plus all identity-kept
// values — the structured identifiers already minted, so the SQL pass won't
// re-mint them under the "sql" kind. Called between the two observe waves.
func (im *IdMap) Known(probeKinds []string) map[string]bool {
	known := map[string]bool{}
	for _, k := range probeKinds {
		for v := range im.values[k] {
			known[v] = true
		}
	}
	for _, set := range im.keep {
		for v := range set {
			known[v] = true
		}
	}
	return known
}

// Pairs returns all (kind, original, token) triples sorted, for the
// identifier_map table (trusted side only).
func (im *IdMap) Pairs() [][3]string {
	var out [][3]string
	for kind, m := range im.fwd {
		for orig, tok := range m {
			out = append(out, [3]string{kind, orig, tok})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}
