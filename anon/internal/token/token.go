// Package token mints HMAC-derived anonymization tokens.
//
// Tokens are COMPUTED, never allocated: HMAC-SHA256(key, kind||0x00||original)
// truncated to hex. Any process holding the key derives the same token for the
// same identifier — no cross-replica coordination, idempotent re-runs, stable
// under schema drift. The token shape <kind>_<hex> is lexically reserved (see
// ReservedRe): a run aborts if any real identifier matches it, which keeps
// word-level de-tokenization of composed SQL unambiguous.
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
)

// Kinds in use. db/tbl/col/user/dict/cluster/host come from structured system
// tables; sql is minted by the lexer for free-text identifiers it cannot
// attribute; field/enum come from data-type strings.
var Kinds = []string{"db", "tbl", "col", "user", "role", "dict", "cluster", "disk", "host", "sql", "field", "enum"}

// HexLen is the default token suffix length (8 hex = 32 bits). Collisions are
// detected at map-build time and the whole colliding group is re-minted at
// HexLenWide.
const (
	HexLen     = 8
	HexLenWide = 16
)

// ReservedRe matches the token namespace. Real identifiers must never match;
// the discovery run aborts if one does.
var ReservedRe = regexp.MustCompile(`^(db|tbl|col|user|role|dict|cluster|disk|host|sql|field|enum)_[0-9a-f]{8,16}$`)

type Minter struct{ key []byte }

func NewMinter(key []byte) (*Minter, error) {
	if len(key) < 16 {
		return nil, fmt.Errorf("token: HMAC key must be at least 16 bytes (got %d)", len(key))
	}
	return &Minter{key: key}, nil
}

func (m *Minter) mac(parts ...string) []byte {
	h := hmac.New(sha256.New, m.key)
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(p))
	}
	return h.Sum(nil)
}

// Token derives the token for (kind, original) at the given hex length.
func (m *Minter) Token(kind, original string, hexLen int) string {
	sum := m.mac(kind, original)
	return kind + "_" + hex.EncodeToString(sum)[:hexLen]
}

// ValueSeed derives the UInt64 seed embedded in trusted-side masking
// expressions (sipHash64(seed, v)). The seed is independent of every
// identifier token (separate HMAC input domain).
func (m *Minter) ValueSeed() uint64 {
	return binary.BigEndian.Uint64(m.mac("value-seed", "v1"))
}
