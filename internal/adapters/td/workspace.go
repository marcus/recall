package td

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/marcus/recall/internal/protocol"
)

// workspaceSep separates the workspace from the issue id inside a locator's
// local part. It is "/" because that is what a person reads as "in": the
// locator `td:recall/td-369eef` says issue td-369eef in the recall workspace.
const workspaceSep = "/"

// workspace is one td database and the name Recall knows it by.
//
// Root is what td is pointed at; Name is what locators, metadata, and
// provenance carry. They are separate values because they answer different
// questions and change at different rates: a repository can be moved, cloned
// to a second checkout, or reached through a worktree without becoming a
// different workspace, and a locator recorded a month ago must still resolve
// after any of that. Deriving the name from a path hash would fail every one
// of those cases; deriving it from the path's last element survives them and
// stays readable.
type workspace struct {
	Name string
	Root string
}

// resolveWorkspace decides a workspace's identity from configuration alone.
//
// Deliberately without running td: identity has to be known before the first
// probe, because health must be able to report a MISSING workspace as
// unavailable, and a source that could not name itself until td answered could
// not report anything at all. It also has to be identical on every run, and a
// value read from the source would change the day the source changes.
//
// The name defaults to the last element of the configured location, which is
// the repository directory and therefore what td itself reports as the project
// name. Two workspaces can share that name — `~/work/api` and `~/oss/api` —
// so the name is configurable, and that is the whole fix: they are separate
// instances already, and one of them says `workspace = "work-api"`.
func resolveWorkspace(location, configured string) (workspace, error) {
	root := expandHome(strings.TrimSpace(location))
	if root == "" {
		return workspace{}, protocol.Errorf(protocol.CodeInvalidParams,
			"td settings: this source names no location; a td source is one workspace directory")
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	root = filepath.Clean(root)

	name := strings.TrimSpace(configured)
	if name == "" {
		name = filepath.Base(root)
	}
	if err := checkWorkspaceName(name); err != nil {
		return workspace{}, err
	}
	return workspace{Name: name, Root: root}, nil
}

// checkWorkspaceName rejects names that would not survive a round trip through
// a locator. A name carrying the locator separator, the workspace separator,
// or whitespace would parse back as something else, which is the one way a
// locator can quietly start naming another record.
func checkWorkspaceName(name string) error {
	switch {
	case name == "", name == "." || name == "..":
		return protocol.Errorf(protocol.CodeInvalidParams,
			"td settings: %q is not a workspace name", name)
	case strings.ContainsAny(name, ":/ \t\n"):
		return protocol.Errorf(protocol.CodeInvalidParams,
			"td settings: workspace %q may not contain ':', '/', or whitespace; they would not survive a locator", name)
	}
	return nil
}

// locator renders the local part of a locator: workspace identity plus issue
// id, which is what docs/profile-example.md specifies for this source.
func (w workspace) locator(issueID string) string {
	return w.Name + workspaceSep + issueID
}

// parse reads an issue id back out of a locator's local part.
//
// The qualified form is what this adapter emits. The bare form is accepted too
// because a person typing `recall expand td:td-369eef` is naming this
// instance's workspace by naming this instance, and refusing that would be
// pedantry rather than safety.
//
// A locator naming a DIFFERENT workspace is refused, and that is the boundary
// this function exists for. Answering it from this workspace would silently
// return an unrelated issue that happens to share a six-hex id, which is
// exactly the confusion one adapter serving many workspaces has to prevent.
func (w workspace) parse(local string) (string, error) {
	name, id, qualified := strings.Cut(local, workspaceSep)
	if !qualified {
		name, id = w.Name, local
	}
	switch {
	case name != w.Name:
		return "", fmt.Errorf("%w: %q names workspace %q; this source serves %q",
			protocol.ErrLocatorUnknown, local, name, w.Name)
	case !idPattern.MatchString(id):
		return "", fmt.Errorf("%w: %q is not a td issue id", protocol.ErrLocatorUnknown, id)
	}
	return id, nil
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~"))
}
