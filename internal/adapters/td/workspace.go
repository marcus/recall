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
// Name is derived from Root and never from Location. It has to be, because
// Location can name a directory that is not a workspace at all: two sources at
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

	// Pinned records that the `workspace` setting named this workspace
	// explicitly. It changes nothing about Name — a pin that disagreed with
	// the resolved root is refused at the handshake — and exists so
	// diagnostics can say the identity was asserted as well as observed.
	Pinned bool
}

// resolveWorkspace decides a workspace's identity before any td runs.
//
// Deliberately without running td: identity has to be known before the first
// probe, because health must be able to report a MISSING workspace as
// unavailable, and a source that could not name itself until td answered could
// not report anything at all.
//
// What it does NOT do is take the identity from configuration. The name is the
// last element of the RESOLVED root — the directory td will open a database in
// — which is also what td itself reports as its project name, so the two can be
// compared against each other on every health probe. See [resolveRoot] for why
// that resolution is a mirror of td's and why the comparison is what makes the
// mirror safe.
//
// The `workspace` setting therefore ASSERTS the identity rather than
// overriding it, and a disagreement is refused here. It used to override, and
// that is how a source pointing at clara-home with `workspace = "recall"`
// answered `td:recall/td-224186` out of the wrong database: a name that came
// from configuration is a name nothing checked. Two repositories sharing a
// directory name do not need the override to stay apart — every locator
// already carries the source_id, and two instances are two source_ids — so
// what is lost by refusing is a rename, and what is gained is that no locator
// can name a workspace its database is not.
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
	if asserted != "" && !strings.EqualFold(asserted, name) {
		return workspace{}, protocol.Errorf(protocol.CodeInvalidParams,
			"td settings: workspace %q, but %s resolves to the td database at %s, whose workspace is %q. "+
				"A configured name cannot rename another workspace's database: locators built from it would "+
				"name a workspace this source does not read. Remove the setting, or point the location at %q",
			asserted, loc, root, name, asserted)
	}
	return workspace{Name: name, Location: loc, Root: root, Pinned: asserted != ""}, nil
}

// resolvedProject is the name td should report for the database this instance
// resolved to. Health compares it against `td info`'s own answer, which is the
// one check that ties this adapter's idea of its identity to the database that
// was actually opened.
func (w workspace) resolvedProject() string { return filepath.Base(w.Root) }

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
