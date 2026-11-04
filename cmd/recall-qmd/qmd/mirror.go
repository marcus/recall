package qmd

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/marcus/recall/pkg/document"
	"github.com/marcus/recall/pkg/protocol"
)

// The protocol workdir is <state>/adapters/<source_uid>. No display name or
// corpus path participates in mirror identity. Standalone protocol clients may
// supply another workdir; their mirror stays within that explicit directory.
type derivedPaths struct{ Mirror, Receipt string }

func newDerivedPaths(workdir, corpus string) (*derivedPaths, error) {
	if workdir == "" {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd: derive_extensions requires a workdir")
	}
	dir, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	mirror := filepath.Join(dir, "qmd-derived")
	if filepath.Base(filepath.Dir(dir)) == "adapters" {
		mirror = filepath.Join(filepath.Dir(filepath.Dir(dir)), "qmd-derived", filepath.Base(dir))
	}
	if within(canonical(corpus), canonicalFuture(mirror)) || within(canonicalFuture(mirror), canonical(corpus)) {
		return nil, protocol.Errorf(protocol.CodeInvalidParams, "qmd: derived mirror must be outside the corpus")
	}
	return &derivedPaths{Mirror: mirror, Receipt: filepath.Join(dir, "qmd-derived.json")}, nil
}
func within(root, file string) bool {
	rel, err := filepath.Rel(root, file)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type mirrorEntry struct {
	SourceHash string `json:"source_hash"`
	MirrorHash string `json:"mirror_hash"`
	QMDPath    string `json:"qmd_path"`
}
type mirrorReceipt struct {
	Version      int                    `json:"version"`
	Corpus       string                 `json:"corpus"`
	Config       string                 `json:"config"`
	Ready        bool                   `json:"ready"`
	Failure      string                 `json:"failure,omitempty"`
	Entries      map[string]mirrorEntry `json:"entries"`
	Excluded     int                    `json:"excluded"`
	ExcludedDirs int                    `json:"excluded_dirs"`
}
type derivedDocument struct {
	HTML document.HTML
	Data []byte
}
type mirrorScan struct {
	Docs                   map[string]derivedDocument
	Entries                map[string]mirrorEntry
	Excluded, ExcludedDirs int
	Failures               []string
}

func (a *Adapter) hasDerived() bool    { a.mu.RLock(); defer a.mu.RUnlock(); return a.derived != nil }
func (a *Adapter) paths() derivedPaths { a.mu.RLock(); defer a.mu.RUnlock(); return *a.derived }
func hashBytes(data []byte) string     { return fmt.Sprintf("%x", sha256.Sum256(data)) }

// Every read/write rechecks that the artifact tree and the corpus do not
// overlap. Reject symlinks inside the artifact tree rather than following them
// during incremental writes or deletion. Configured ancestor symlinks (e.g.
// /var on macOS) are resolved by canonical, just as for collection locations.
func (a *Adapter) validateMirror() error {
	p := a.paths()
	_, _, root, _, err := a.session()
	if err != nil {
		return err
	}
	if within(canonical(root), canonicalFuture(p.Mirror)) || within(canonicalFuture(p.Mirror), canonical(root)) {
		return fmt.Errorf("%w: derived mirror overlaps corpus", protocol.ErrSourceUnavailable)
	}
	base := filepath.Dir(p.Mirror)
	if info, err := os.Lstat(base); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: derived parent is a symlink", protocol.ErrSourceUnavailable)
	}
	return filepath.WalkDir(p.Mirror, func(_ string, d fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink in derived mirror", protocol.ErrSourceUnavailable)
		}
		return nil
	})
}

func (a *Adapter) scanMirror(ctx context.Context) (mirrorScan, error) {
	set, _, _, _, err := a.session()
	if err != nil {
		return mirrorScan{}, err
	}
	root, err := a.corpusRoot()
	if err != nil {
		return mirrorScan{}, err
	}
	out := mirrorScan{Docs: map[string]derivedDocument{}, Entries: map[string]mirrorEntry{}}
	var paths []string
	err = filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		rel, e := filepath.Rel(root, full)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if full != root && set.Selection.DirectoryReason(root, rel) != "" {
				out.ExcludedDirs++
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.Contains(rel, "#") {
			return nil
		}
		ext := strings.ToLower(path.Ext(rel))
		eligible := ext == ".md" || ext == ".markdown"
		for _, v := range set.DeriveExtensions {
			eligible = eligible || ext == strings.ToLower(v)
		}
		if eligible {
			paths = append(paths, rel)
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	selected, excluded := set.Selection.Select(paths)
	for _, e := range excluded {
		if derives(set, e.Path) {
			out.Excluded++
		}
	}
	for _, rel := range selected {
		if !derives(set, rel) {
			continue
		}
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		data, err := readOriginal(root, rel, set.MaxRawBytes)
		if err != nil {
			out.Failures = append(out.Failures, rel+": "+safeErr(err))
			continue
		}
		doc, err := document.ExtractHTML(data, rel, set.MaxRawBytes, set.MaxTextBytes)
		if errors.Is(err, document.ErrNoIndex) {
			out.Excluded++
			continue
		}
		if err != nil {
			out.Failures = append(out.Failures, rel+": "+safeErr(err))
			continue
		}
		out.Docs[rel] = derivedDocument{doc, data}
		out.Entries[rel] = mirrorEntry{SourceHash: hashBytes(data), MirrorHash: hashBytes([]byte(doc.Markdown())), QMDPath: qmdVirtualPath(rel + ".md")}
	}
	// Qmd folds punctuation into handles. Two physical paths mapping to one
	// handle would overwrite each other in its index and cannot be cited safely.
	owners := map[string]string{}
	collisions := map[string]bool{}
	for rel, entry := range out.Entries {
		if old, ok := owners[entry.QMDPath]; ok && old != rel {
			collisions[old] = true
			collisions[rel] = true
		}
		owners[entry.QMDPath] = rel
	}
	for rel := range collisions {
		delete(out.Docs, rel)
		delete(out.Entries, rel)
		out.Failures = append(out.Failures, rel+": qmd virtual path collision")
	}
	sort.Strings(out.Failures)
	return out, nil
}
func derives(set Settings, rel string) bool {
	for _, ext := range set.DeriveExtensions {
		if strings.EqualFold(path.Ext(rel), ext) {
			return true
		}
	}
	return false
}

// Never read through a symlink, FIFO, device, or directory, including a
// symlinked parent replaced since the boundary walk.
func readOriginal(root, rel string, limit int64) ([]byte, error) {
	if _, err := parseLocal(rel + "#L1-L1"); err != nil {
		return nil, err
	}
	full := root
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		full = filepath.Join(full, part)
		info, err := os.Lstat(full)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || (i == len(parts)-1 && !info.Mode().IsRegular()) {
			return nil, fmt.Errorf("not a regular corpus file")
		}
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only close
	return document.Read(f, limit)
}
func (a *Adapter) loadReceipt() (mirrorReceipt, error) {
	data, err := os.ReadFile(a.paths().Receipt)
	if err != nil {
		return mirrorReceipt{}, err
	}
	var r mirrorReceipt
	if err = json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	if r.Version != 1 {
		return r, fmt.Errorf("unknown derived receipt version")
	}
	return r, nil
}
func (a *Adapter) saveReceipt(r mirrorReceipt) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(a.paths().Receipt, append(data, '\n'))
}
func atomicWrite(file string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".recall-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name) //nolint:errcheck // best-effort temporary cleanup
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, file)
}
func (a *Adapter) syncMirror(ctx context.Context, scan mirrorScan) error {
	if err := a.validateMirror(); err != nil {
		return err
	}
	p := a.paths()
	if err := os.MkdirAll(p.Mirror, 0o700); err != nil {
		return err
	}
	// Compare content hashes, not mtimes. Unchanged sources and unchanged
	// rendered bytes retain their inode/mtime across refreshes.
	for rel, doc := range scan.Docs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		file := filepath.Join(p.Mirror, filepath.FromSlash(rel)+".md")
		if old, err := os.ReadFile(file); err == nil && hashBytes(old) == scan.Entries[rel].MirrorHash {
			continue
		}
		if err := atomicWrite(file, []byte(doc.HTML.Markdown())); err != nil {
			return err
		}
	}
	return filepath.WalkDir(p.Mirror, func(file string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(p.Mirror, file)
		if err != nil {
			return err
		}
		source := strings.TrimSuffix(filepath.ToSlash(rel), ".md")
		if _, ok := scan.Entries[source]; !ok || !strings.HasSuffix(rel, ".md") {
			return os.Remove(file)
		}
		return nil
	})
}

// inspectMirror is read-only. A receipt is not proof that today's source or
// artifact bytes are still the ones whose embedding completed.
func (a *Adapter) inspectMirror(ctx context.Context) (mirrorReceipt, mirrorScan, string) {
	r, err := a.loadReceipt()
	if err != nil {
		return r, mirrorScan{}, "derived mirror has no valid refresh receipt; run recall refresh"
	}
	if err = a.validateMirror(); err != nil {
		return r, mirrorScan{}, safeErr(err)
	}
	scan, err := a.scanMirror(ctx)
	if err != nil {
		return r, scan, safeErr(err)
	}
	set, _, root, _, err := a.session()
	if err != nil {
		return r, scan, safeErr(err)
	}
	reason := ""
	switch {
	case !r.Ready:
		reason = "derived refresh incomplete: " + r.Failure
	case r.Corpus != canonical(root) || r.Config != set.settingsDigest():
		reason = "derived configuration changed; run recall refresh"
	case len(scan.Failures) > 0:
		reason = "derived files could not be extracted"
	case len(scan.Entries) != len(r.Entries):
		reason = "derived file boundary changed; run recall refresh"
	}
	for rel, entry := range scan.Entries {
		data, err := os.ReadFile(filepath.Join(a.paths().Mirror, filepath.FromSlash(rel)+".md"))
		if entry != r.Entries[rel] || err != nil || hashBytes(data) != entry.MirrorHash {
			reason = "derived source or mirror changed; run recall refresh"
			break
		}
	}
	return r, scan, reason
}

// EvalSymlinks alone cannot resolve a not-yet-created artifact path. Resolve
// its nearest existing ancestor so a missing leaf under a symlink into the
// corpus is refused before MkdirAll or collection creation can touch it.
func canonicalFuture(name string) string {
	current := filepath.Clean(name)
	var tail []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved := canonical(current)
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved
		}
		parent := filepath.Dir(current)
		if parent == current {
			return canonical(name)
		}
		tail = append(tail, filepath.Base(current))
		current = parent
	}
}
