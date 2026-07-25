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
	Skipped     []skippedDir
	Watermark   string
	GitRevision string
}

// skippedDir is one directory the walk did not enter, and the declared reason.
//
// A skipped directory is not a failure: no named record is missing from the
// generation, so counting it as one would make coverage partial for every
// corpus containing a .git/ and destroy the one signal that says records are
// genuinely absent. It is reported separately instead, because a boundary
// nobody can see is indistinguishable from a corpus that has nothing to say.
type skippedDir struct {
	Path   string // corpus-relative, slash-separated
	Reason string
}

func scanCorpus(root string, s Settings) (scan, error) {
	var files []fileRef
	var skipped []skippedDir

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
			// The root is never excluded by its own name. An operator who
			// points a source at `.claude/notes` named that directory, and a
			// pattern that swallowed the root would leave a source configured
			// to index nothing.
			if path == root {
				return nil
			}
			if reason, ok := skipReason(path, d.Name(), s); ok {
				skipped = append(skipped, skippedDir{Path: relOrBase(root, path), Reason: reason})
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
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Path < skipped[j].Path })

	out := scan{Files: files, Skipped: skipped, GitRevision: gitRevision(root)}
	out.Watermark = watermark(files, s.digest(), out.GitRevision)
	return out, nil
}

// skipReason reports why the walk does not enter a directory, if it does not.
//
// Both reasons are declared configuration, and both are reported: an exclusion
// the operator cannot see is the failure this whole mechanism exists to avoid.
// The nested-repository check is the precise one — it names the actual damage,
// a second checkout of the same documents — while the name patterns are the
// cheap one that also keeps caches, build output, and virtualenvs out.
func skipReason(path, name string, s Settings) (string, bool) {
	if pattern, ok := s.excludedDir(name); ok {
		return "exclude_dirs " + pattern, true
	}
	if s.ExcludeNestedRepos && isRepository(path) {
		return "nested repository", true
	}
	return "", false
}

// isRepository reports whether a directory holds a .git entry of its own.
//
// Lstat rather than Stat, and no check of what the entry is: a clone keeps a
// .git directory, a worktree and a submodule keep a .git FILE naming the real
// one, and all three are a separate checkout whose documents are copies of
// something already indexed under another path.
func isRepository(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// watermark identifies a corpus state.
//
// A git revision alone would be wrong: an edited working tree keeps HEAD, and
// an index that missed the edit would claim to be current. So the revision is
// recorded as evidence and the digest of the file list decides equality.
//
// The settings digest is part of it because settings decide what the corpus IS:
// changing the indexed extensions, the excluded directories, or a declared
// alias changes the generation's content without touching a single file, and an
// index that reported itself current afterwards would be answering under
// configuration nobody reviewed.
//
// The skipped directories are deliberately NOT part of it. They contribute no
// records, so a new .venv appearing next to the notes changes nothing about
// what the index contains, and letting it move the watermark would report a
// current index as stale until someone rebuilt it into the same content.
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
