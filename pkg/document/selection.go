package document

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Selection is the shared corpus boundary for document and derived-text views.
// Paths passed to it are root-relative and slash-separated. Sibling preference
// considers only regular, extension-eligible files admitted by the walk.
type Selection struct {
	ExcludeDirs        []string
	ExcludeGlobs       []string
	SiblingPreference  []string
	ExcludeNestedRepos bool
}

type Exclusion struct{ Path, Reason string }

func (s Selection) Validate() error {
	for _, p := range s.ExcludeGlobs {
		if p == "" || path.IsAbs(p) || strings.Contains(p, "\\") {
			return fmt.Errorf("exclude_globs: invalid root-relative glob %q", p)
		}
		for _, segment := range strings.Split(p, "/") {
			if segment == ".." || segment == "." || segment == "" {
				return fmt.Errorf("exclude_globs: invalid root-relative glob %q", p)
			}
			if _, err := path.Match(segment, "probe"); err != nil {
				return fmt.Errorf("exclude_globs: %q: %w", p, err)
			}
		}
	}
	for _, ext := range s.SiblingPreference {
		if len(ext) < 2 || ext[0] != '.' || strings.ContainsAny(ext, "/\\*?[]") {
			return fmt.Errorf("sibling_preference: want extensions like .md, got %q", ext)
		}
	}
	return nil
}

// MatchGlob supports path.Match syntax plus a whole-segment ** matching zero or
// more directories. A slashless pattern matches a file at the corpus root;
// use **/*.html to match that suffix at every depth.
func MatchGlob(pattern, rel string) bool {
	p, r := strings.Split(pattern, "/"), strings.Split(rel, "/")
	// Dynamic programming bounds repeated ** patterns to O(pattern * path).
	memo := map[[2]int]bool{}
	seen := map[[2]int]bool{}
	var match func(int, int) bool
	match = func(i, j int) bool {
		key := [2]int{i, j}
		if seen[key] {
			return memo[key]
		}
		seen[key] = true
		ok := false
		switch {
		case i == len(p):
			ok = j == len(r)
		case p[i] == "**":
			ok = match(i+1, j) || j < len(r) && match(i, j+1)
		case j < len(r):
			hit, err := path.Match(p[i], r[j])
			ok = err == nil && hit && match(i+1, j+1)
		}
		memo[key] = ok
		return ok
	}
	return match(0, 0)
}

func (s Selection) GlobReason(rel string) string {
	for _, pattern := range s.ExcludeGlobs {
		if MatchGlob(pattern, rel) {
			return "exclude_globs " + pattern
		}
	}
	return ""
}

// DirectoryReason explains why a walk should prune this directory. The caller
// leaves the explicitly configured root unpruned.
func (s Selection) DirectoryReason(root, rel string) string {
	for _, pattern := range s.ExcludeDirs {
		if ok, err := path.Match(pattern, path.Base(rel)); err == nil && ok {
			return "exclude_dirs " + pattern
		}
	}
	if reason := s.GlobReason(rel); reason != "" {
		return reason
	}
	if s.ExcludeNestedRepos {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel), ".git")); err == nil {
			return "nested repository"
		}
	}
	return ""
}

// Select applies file globs and sibling selection to an eligible file list.
// Preference order is significant; extensions absent from the list remain
// eligible when no preferred sibling exists. Inputs are never mutated.
func (s Selection) Select(paths []string) ([]string, []Exclusion) {
	var candidates []string
	var excluded []Exclusion
	preferred := map[string]string{}
	priority := map[string]int{}
	for i, ext := range s.SiblingPreference {
		ext = strings.ToLower(ext)
		if _, ok := priority[ext]; !ok {
			priority[ext] = i
		}
	}
	rank := func(rel string) int {
		if i, ok := priority[strings.ToLower(path.Ext(rel))]; ok {
			return i
		}
		return len(s.SiblingPreference)
	}
	stem := func(rel string) string { return strings.TrimSuffix(rel, path.Ext(rel)) }
	ordered := append([]string(nil), paths...)
	sort.Strings(ordered)
	for _, rel := range ordered {
		if reason := s.GlobReason(rel); reason != "" {
			excluded = append(excluded, Exclusion{rel, reason})
			continue
		}
		candidates = append(candidates, rel)
		if rank(rel) == len(s.SiblingPreference) {
			continue
		}
		key := stem(rel)
		old, exists := preferred[key]
		if !exists || rank(rel) < rank(old) {
			preferred[key] = rel
		}
	}
	var selected []string
	for _, rel := range candidates {
		if winner, ok := preferred[stem(rel)]; ok && winner != rel {
			excluded = append(excluded, Exclusion{rel, "sibling_preference " + winner})
			continue
		}
		selected = append(selected, rel)
	}
	sort.Slice(excluded, func(i, j int) bool { return excluded[i].Path < excluded[j].Path })
	return selected, excluded
}
