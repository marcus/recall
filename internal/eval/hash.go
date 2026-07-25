package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// hashPrefix labels the digest so a pack hash is never mistaken for a content
// hash of some other kind.
const hashPrefix = "sha256:"

// ContentHash is a pack's content identity: its manifest, its cases, and its
// judgments.
//
// It is stable across ordering. Every record is digested on its own, the
// digests are sorted, and the sorted list is digested again — so moving a case
// to the end of the file, or reading the two files in either order, produces
// the same hash. Duplicated records still change it: sorting a multiset keeps
// duplicates, and a pack that grades one thing twice is a different pack.
//
// The hash covers the parsed values rather than the file bytes, so
// reindentation and line endings do not change a pack's identity. Nothing is
// lost by that: every schema forbids unknown properties, so a field the Go
// types do not carry cannot be in a valid pack.
//
// The pack's directory is deliberately excluded. A pack copied elsewhere is
// the same pack, and a run recording where it happened to live would compare
// unequal to itself.
func ContentHash(pack *Pack, cases []Case, judgments []Judgment) (string, error) {
	digests := make([][sha256.Size]byte, 0, 1+len(cases)+len(judgments))

	add := func(domain string, v any) error {
		// Encoding to JSON is canonical enough for the purpose: Go emits struct
		// fields in declaration order and map keys sorted, and every field here
		// is a scalar, slice, or string-keyed map.
		body, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("hash %s: %w", domain, err)
		}
		h := sha256.New()
		// The domain separator keeps a case and a judgment with identical bytes
		// from colliding, so a record cannot be moved between files unnoticed.
		h.Write([]byte(domain))
		h.Write([]byte{0})
		h.Write(body)
		digests = append(digests, [sha256.Size]byte(h.Sum(nil)))
		return nil
	}

	if pack == nil {
		return "", fmt.Errorf("hash pack: no manifest")
	}
	if err := add("pack", pack); err != nil {
		return "", err
	}
	for i := range cases {
		if err := add("case", cases[i]); err != nil {
			return "", err
		}
	}
	for i := range judgments {
		if err := add("judgment", judgments[i]); err != nil {
			return "", err
		}
	}

	sort.Slice(digests, func(i, j int) bool {
		return string(digests[i][:]) < string(digests[j][:])
	})

	h := sha256.New()
	for i := range digests {
		h.Write(digests[i][:])
	}
	return hashPrefix + hex.EncodeToString(h.Sum(nil)), nil
}
