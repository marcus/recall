package docs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// fileRef is one candidate record in the corpus, before it is read.
type fileRef struct {
	Path    string // corpus-relative, slash-separated
	Size    int64
	ModTime time.Time
}

// scan is a complete corpus boundary: every file the settings admit, in a
// deterministic order, plus the watermark that identifies this state of the
// corpus.
//
// It either enumerates the whole corpus or fails. A directory that cannot be
// listed is not a partial result that can be published: its records would be
// missing from the next generation and therefore indistinguishable from records
// deleted upstream, which is precisely the confusion invariant 5 forbids. A
// single unreadable FILE is different — it is one named record that failed, and
// that is counted rather than fatal.
type scan struct {
	Files       []fileRef
	Watermark   string
	GitRevision string
}

func scanCorpus(root string, s Settings) (scan, error) {
	var files []fileRef

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if d == nil || d.IsDir() {
				return fmt.Errorf("cannot list %s: %w", relOrBase(root, path), walkErr)
			}
			// One entry that vanished between the listing and the visit is a
			// deletion the next boundary reports. It does not make this one
			// unknown the way an unlistable directory does.
			return nil
		}
		if d.IsDir() {
			if path != root && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		// Only regular files: a symlink loop, device, or FIFO in a notes
		// directory is not a document, and opening one can block forever.
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !s.indexes(rel) {
			return nil
		}
		// Same race as above: a file listed and then removed is simply not part
		// of this boundary.
		if info, err := d.Info(); err == nil {
			files = append(files, fileRef{Path: rel, Size: info.Size(), ModTime: info.ModTime().UTC()})
		}
		return nil
	})
	if err != nil {
		return scan{}, err
	}

	// WalkDir already visits in lexical order; sorting says so explicitly,
	// because chunk order, locator order, and the watermark all depend on it.
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	out := scan{Files: files, GitRevision: gitRevision(root)}
	out.Watermark = watermark(files, s.digest(), out.GitRevision)
	return out, nil
}

// skipDir keeps the walk out of directories that are never corpus content.
//
// Dot-directories are excluded as a class, not by name. They hold tool state —
// caches, build output, virtualenvs, agent worktrees — and the failure they
// cause is worse than indexing junk: a directory like `.claude/worktrees/`
// contains whole checkouts of the corpus, so the same document is indexed
// several times under distinct paths. Lineage groups on source_record_id, and
// those copies carry different ones, so a single document arrives as several
// independent lineage roots and corroborates itself. Nothing downstream can
// detect that, because at that point the copies genuinely are distinct records.
//
// The cost is `.github/`, which is the one dot-directory people write prose in.
// A corpus that needs it can point an instance's location or Root straight at
// it, which is explicit and cannot silently pull in a checkout.
func skipDir(name string) bool {
	if len(name) > 1 && strings.HasPrefix(name, ".") {
		return true
	}
	return name == "node_modules"
}

// watermark identifies a corpus state.
//
// A git revision alone would be wrong: an edited working tree keeps HEAD, and
// an index that missed the edit would claim to be current. So the revision is
// recorded as evidence and the digest of the file list decides equality.
//
// The settings digest is part of it because settings decide what the corpus IS:
// changing the indexed extensions or a declared alias changes the generation's
// content without touching a single file, and an index that reported itself
// current afterwards would be answering under configuration nobody reviewed.
func watermark(files []fileRef, settingsDigest, gitRev string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "settings\x00%s\n", settingsDigest)
	for _, f := range files {
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00%d\n", f.Path, f.Size, f.ModTime.UnixNano())
	}
	digest := hex.EncodeToString(h.Sum(nil))[:12]
	if gitRev != "" {
		return "git:" + gitRev[:min(12, len(gitRev))] + "+fs:" + digest
	}
	return "fs:" + digest
}

func relOrBase(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.Base(path)
}

// gitRevision reads HEAD from the repository containing root, or returns empty
// when there is none.
//
// It reads the ref files directly rather than running git: an adapter that
// shells out inherits another program's exit codes, PATH, and prompts, and the
// three files below are a stable on-disk format. Anything unexpected returns
// empty, because a missing revision is normal and never an error.
func gitRevision(root string) string {
	dir := gitDir(root)
	if dir == "" {
		return ""
	}
	head, err := os.ReadFile(filepath.Join(dir, "HEAD"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(head))
	ref, ok := strings.CutPrefix(line, "ref: ")
	if !ok {
		return validRev(line)
	}
	if body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ref))); err == nil {
		return validRev(strings.TrimSpace(string(body)))
	}
	packed, err := os.ReadFile(filepath.Join(dir, "packed-refs"))
	if err != nil {
		return ""
	}
	for _, l := range strings.Split(string(packed), "\n") {
		sha, name, found := strings.Cut(strings.TrimSpace(l), " ")
		if found && name == ref {
			return validRev(sha)
		}
	}
	return ""
}

func gitDir(root string) string {
	dir, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, ".git")
		info, err := os.Stat(candidate)
		switch {
		case err == nil && info.IsDir():
			return candidate
		case err == nil:
			// A worktree or submodule: ".git" is a file naming the real one.
			body, err := os.ReadFile(candidate)
			if err != nil {
				return ""
			}
			path, ok := strings.CutPrefix(strings.TrimSpace(string(body)), "gitdir: ")
			if !ok {
				return ""
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			return path
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func validRev(s string) string {
	if len(s) < 7 {
		return ""
	}
	for _, r := range s {
		if _, err := strconv.ParseUint(string(r), 16, 8); err != nil {
			return ""
		}
	}
	return s
}
