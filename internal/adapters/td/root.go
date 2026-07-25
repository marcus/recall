package td

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// td's own markers, named here so the mirror below reads against td's source
// rather than against magic strings.
const (
	tdRootFile = ".td-root"
	todosDir   = ".todos"
)

// resolveRoot answers the question the configured location cannot: which
// database will td open when it is pointed here.
//
// td does not resolve its database from the directory it is given. The CLI
// resolves its work directory once, then db.Open applies the same resolver to
// that result before opening .todos/issues.db. Each pass honors a local
// `.td-root` redirect, an existing `.todos/`, a recorded directory
// association, then the git root and the main worktree, so a
// subdirectory, a worktree, or a submodule path all reach the SAME database as
// the repository they sit inside. Identity taken from the configured path
// therefore names something that need not exist: two sources configured at
// `~/code/recall` and `~/code/recall/docs` are one database under two names,
// and the core counted one issue as two independent pieces of evidence for it.
//
// This is a deliberate mirror of td's internal/workdir.ResolveBaseDir, and a
// mirror is a liability: td can change its rule, and this copy would then name
// a directory td never opened. That is why nothing trusts the result on its
// own. [Adapter.Health] compares the base name of what this function returned
// against the project name `td info` reports — td's own statement about the
// database it just read — and makes the source unusable when they disagree.
// Search and Expand repeat that binding, then point every evidence command at
// the bound root through td's supported --work-dir flag. A drift in td's
// resolution therefore surfaces as a source that cannot confirm or pin its
// identity, rather than as one that quietly answers for the wrong workspace.
//
// Filesystem reads only, in the common case. A repository holding its own
// `.todos/` settles on the first check, so the ordinary configuration costs no
// process at all; git is consulted only for the paths that need it, which are
// exactly the misconfigured and worktree cases.
func resolveRoot(location string) string {
	if location == "" {
		return ""
	}
	// The configured directory itself is checked first. Do not canonicalize it
	// before lookup: td associations are keyed by the cleaned configured path,
	// including a symlink spelling. This order is
	// load-bearing: td honors a local .td-root, local .todos, or directory
	// association before consulting git. Lifting to the git root first loses
	// those redirects and can falsely bind a different same-basename store.
	first := resolveBaseDir(filepath.Clean(location))

	// td's commands pass the already-resolved CLI baseDir into db.Open, which
	// resolves it once more. Mirroring only initBaseDir identifies the project
	// td reports, but not necessarily the database db.Open actually opened.
	second := resolveBaseDir(first)
	return canonicalPath(second)
}

// resolveBaseDir mirrors td's marker search, in td's order. The order is the
// whole content of the function: a `.td-root` beats an adjacent `.todos/`, and
// both beat anything git would say, because that is the precedence td applies.
func resolveBaseDir(dir string) string {
	return resolveBaseDirUsing(dir, association)
}

func resolveBaseDirUsing(dir string, lookup func(string) (string, bool)) string {
	if root, ok := readTdRoot(dir); ok {
		return root
	}
	if hasTodosDir(dir) {
		return dir
	}
	if target, ok := lookup(dir); ok {
		return target
	}

	gitRoot := gitTopLevel(dir)
	if gitRoot == "" {
		return dir
	}
	gitRoot = filepath.Clean(gitRoot)
	if root, ok := readTdRoot(gitRoot); ok {
		return root
	}
	if hasTodosDir(gitRoot) {
		return gitRoot
	}
	if target, ok := lookup(gitRoot); ok {
		return target
	}

	// An external worktree has its own checkout root and no database of its
	// own; td falls back to the main worktree's. Missing this step is how a
	// worktree would be reported as a workspace that does not exist.
	if main := gitMainWorktree(dir, gitRoot); main != "" {
		if root, ok := readTdRoot(main); ok {
			return root
		}
		if hasTodosDir(main) {
			return main
		}
	}
	return dir
}

// readTdRoot reads a `.td-root` redirect. A relative target is resolved
// against the file's own directory, which is what makes a committed
// `.td-root` portable between checkouts.
func readTdRoot(dir string) (string, bool) {
	content, err := os.ReadFile(filepath.Join(dir, tdRootFile))
	if err != nil {
		return "", false
	}
	target := strings.TrimSpace(string(content))
	if target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(dir, target)
	}
	return filepath.Clean(target), true
}

func hasTodosDir(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, todosDir))
	return err == nil && fi.IsDir()
}

// association reads td's recorded directory associations.
//
// The path is `~/.config/td/associations.json` unconditionally, because that
// is what td hardcodes — it consults no XDG variable, so honoring one here
// would resolve differently from the binary being driven. A file this adapter
// cannot read is treated as no association rather than as an error: the answer
// is checked against td's own project name either way.
func association(dir string) (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "td", "associations.json"))
	if err != nil {
		return "", false
	}
	var assoc map[string]string
	if err := json.Unmarshal(data, &assoc); err != nil {
		return "", false
	}
	target, ok := assoc[filepath.Clean(dir)]
	if !ok || !filepath.IsAbs(target) {
		return "", false
	}
	return filepath.Clean(target), true
}

// gitTopLevel returns the checkout root containing dir, or "" when dir is not
// in a repository — including when git is not installed, which is a supported
// state rather than an error: a td workspace needs no git at all.
func gitTopLevel(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitMainWorktree returns the main worktree's root when dir sits in a linked
// worktree, and "" when it is already the main one. The main root is the
// parent of the common git directory, which is how git itself distinguishes
// the two.
func gitMainWorktree(dir, gitRoot string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	main := filepath.Dir(filepath.Clean(common))
	if main == filepath.Clean(gitRoot) {
		return ""
	}
	return main
}

// canonicalPath resolves symlinks and makes the path absolute, matching what
// td does to the root it reports. Without it two locations naming one
// directory through different links would compare as different databases, and
// the duplicate-instance check exists precisely to notice that they are not.
func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(resolved)
}
