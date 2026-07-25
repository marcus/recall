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
// Location is what configuration pointed this instance at. Root is the
// directory td resolves that location to, which is not the same thing and is
// the distinction this type exists to keep: td walks upward to find its
// database, so a subdirectory, a worktree, or a submodule path all reach the
// database of the repository they sit inside. Name is what locators,
// metadata, and provenance carry.
//
// Name is derived from Root and never from Location. Before td runs both are
// provisional results of the filesystem mirror; Health, Search, and Expand
// bind them against `td info`. It has to be this way because Location can name
// a directory that is not a workspace at all: two sources at
// `~/code/recall` and `~/code/recall/docs` are ONE database, and naming the
// second one `docs` made the core count one issue as two independent pieces of
// evidence and score it up for the corroboration. Name and Root stay separate
// values because they still change at different rates — a repository can be
// moved, cloned, or reached through a worktree without becoming a different
// workspace, and a locator recorded a month ago must still resolve after any
// of that.
type workspace struct {
	Name     string
	Location string
	Root     string
	// StoreIdentity is the verified physical store used in fingerprints and
	// duplicate detection. A replay has no physical store and leaves it empty.
	StoreIdentity string

	// Asserted is the optional `workspace` setting. It is an assertion, not
	// identity: the filesystem mirror cannot decide whether it agrees. The
	// assertion is checked only after `td info` identifies the database td
	// actually opened.
	Asserted string
}

// resolveWorkspace decides a workspace's identity before any td runs.
//
// Deliberately without running td: identity has to be known before the first
// probe, because health must be able to report a MISSING workspace as
// unavailable, and a source that could not name itself until td answered could
// not report anything at all.
//
// What it does NOT do is take the identity from configuration. The provisional
// name is the last element of the RESOLVED root — the directory td is expected
// to open a database in — which is also what td reports as its project name,
// so the two can be compared on every probe and operation. See [resolveRoot]
// for why that resolution is a mirror and why binding it against td's answer
// is what makes the mirror safe.
//
// The `workspace` setting therefore ASSERTS the identity rather than
// overriding it. The assertion is syntax-checked here, then compared with the
// opened database by [Adapter.Health] (and by direct Search/Expand calls)
// before a locator can be emitted or expanded. Comparing it here would make
// the mirror authoritative and can falsely refuse a sound source when td's
// resolution changes.
func resolveWorkspace(location, configured string, replaying bool) (workspace, error) {
	loc := expandHome(strings.TrimSpace(location))
	if loc == "" {
		return workspace{}, protocol.Errorf(protocol.CodeInvalidParams,
			"td settings: this source names no location; a td source is one workspace directory")
	}
	if abs, err := filepath.Abs(loc); err == nil {
		loc = abs
	}
	loc = filepath.Clean(loc)

	// A replaying instance has no database to resolve. The recording IS the
	// workspace, so walking the filesystem from it would climb into whatever
	// repository happens to contain the recording and name that instead —
	// which is how a committed transcript would start depending on where the
	// checkout lives.
	root := loc
	if !replaying {
		if resolved := resolveRoot(loc); resolved != "" {
			root = resolved
		}
	}

	name := filepath.Base(root)
	if err := checkWorkspaceName(name); err != nil {
		return workspace{}, err
	}

	asserted := strings.TrimSpace(configured)
	if asserted != "" {
		if err := checkWorkspaceName(asserted); err != nil {
			return workspace{}, err
		}
	}
	return workspace{Name: name, Location: loc, Root: root, Asserted: asserted}, nil
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
//
// The name compared against is the resolved root's, never a configured one, so
// the refusal is a statement about the database this instance opened rather
// than about what its configuration claimed.
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
