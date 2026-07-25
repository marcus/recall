package eval

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// hashPrefix labels the digest so a pack hash is never mistaken for a content
// hash of some other kind.
const hashPrefix = "sha256:"

// ErrUnsafeFixtureTree reports a declared fixture tree that cannot be hashed
// reproducibly without following filesystem state outside the pack.
var ErrUnsafeFixtureTree = errors.New("unsafe fixture tree")

// ContentHash is a pack's content identity: its manifest, its cases, and its
// judgments, plus every regular file under its declared sources and transcript
// fixture trees.
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
// Fixture trees hash slash-separated paths relative to their declared root and
// raw file bytes. Paths are sorted before hashing. File mode, ownership, mtime,
// and empty directories are ignored because they neither survive a portable
// copy nor affect the adapters. Symlinks and non-regular entries are rejected
// rather than followed; a declared but missing tree is an error. An omitted
// tree contributes no fixture digest, and its omission is already represented
// in the manifest.
//
// The pack's absolute directory is deliberately excluded. A pack copied
// elsewhere is the same pack, and a run recording where it happened to live
// would compare unequal to itself.
func ContentHash(pack *Pack, cases []Case, judgments []Judgment) (string, error) {
	digests := make([][sha256.Size]byte, 0, 3+len(cases)+len(judgments))

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
	for _, tree := range []struct {
		domain string
		rel    string
	}{
		{"sources", pack.Sources},
		{"transcripts", pack.Transcripts},
	} {
		if tree.rel == "" {
			continue
		}
		digest, err := fixtureTreeDigest(pack, tree.domain, tree.rel)
		if err != nil {
			return "", err
		}
		digests = append(digests, digest)
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

type fixtureFile struct {
	path string
	body []byte
}

func fixtureTreeDigest(pack *Pack, domain, rel string) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	root, err := pack.resolve(rel)
	if err != nil {
		return zero, fmt.Errorf("hash %s fixtures: %w", domain, err)
	}

	var files []fixtureFile
	seen := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s contains symlink %q",
				ErrUnsafeFixtureTree, domain, path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s contains non-regular entry %q",
				ErrUnsafeFixtureTree, domain, path)
		}

		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
			return fmt.Errorf("%w: %s path %q escapes its root",
				ErrUnsafeFixtureTree, domain, relative)
		}
		if seen[relative] {
			return fmt.Errorf("%w: %s contains duplicate normalized path %q",
				ErrUnsafeFixtureTree, domain, relative)
		}
		seen[relative] = true

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, fixtureFile{path: relative, body: body})
		return nil
	})
	if err != nil {
		return zero, fmt.Errorf("hash %s fixtures at %q: %w", domain, rel, err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })

	h := sha256.New()
	writeHashFrame(h, []byte("fixture-tree:"+domain))
	for _, file := range files {
		writeHashFrame(h, []byte(file.path))
		writeHashFrame(h, file.body)
	}
	return [sha256.Size]byte(h.Sum(nil)), nil
}

// writeHashFrame length-prefixes every value so path and content boundaries
// cannot collide (for example, ["ab", "c"] with ["a", "bc"]).
func writeHashFrame(h hash.Hash, body []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(body)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(body)
}
