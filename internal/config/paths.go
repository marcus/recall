package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ProjectFileName is the file a repository uses to configure Recall. It is
// untrusted: see the package documentation for what it may and may not say.
const ProjectFileName = "recall.toml"

// Paths locates Recall's XDG base directories.
//
// Deleting anything under StateHome or CacheHome changes latency, never
// results, so the two are kept distinct from ConfigHome, which is the only one
// holding anything a user authored.
type Paths struct {
	ConfigHome string `json:"config_home"`
	StateHome  string `json:"state_home"`
	CacheHome  string `json:"cache_home"`
}

// XDGPaths resolves the base directories from the environment.
//
// Each variable wins when set, with the documented fallbacks $HOME/.config,
// $HOME/.local/state, and $HOME/.cache. The XDG specification says a relative
// path is invalid and should be ignored; ignoring it silently would move every
// index and cache somewhere the user did not ask for, so it is an error
// instead.
func XDGPaths() (Paths, error) {
	var (
		p   Paths
		err error
	)
	if p.ConfigHome, err = xdgDir("XDG_CONFIG_HOME", ".config"); err != nil {
		return Paths{}, err
	}
	if p.StateHome, err = xdgDir("XDG_STATE_HOME", ".local", "state"); err != nil {
		return Paths{}, err
	}
	if p.CacheHome, err = xdgDir("XDG_CACHE_HOME", ".cache"); err != nil {
		return Paths{}, err
	}
	return p, nil
}

func xdgDir(env string, fallback ...string) (string, error) {
	if v := os.Getenv(env); v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("%s = %q: must be an absolute path", env, v)
		}
		return filepath.Clean(v), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%s is unset and no home directory is known: %w", env, err)
	}
	return filepath.Join(append([]string{home}, fallback...)...), nil
}

// resolved reports whether the paths were filled in.
func (p Paths) resolved() bool {
	return p.ConfigHome != "" && p.StateHome != "" && p.CacheHome != ""
}

// ConfigDir is Recall's directory inside the config home.
func (p Paths) ConfigDir() string { return filepath.Join(p.ConfigHome, "recall") }

// ConfigFile is the user configuration file. It is the only file that may
// declare an adapter command.
func (p Paths) ConfigFile() string { return filepath.Join(p.ConfigDir(), "config.toml") }

// AdaptersDir holds registered adapter definitions, one or more per file. It
// is user-level and therefore trusted.
func (p Paths) AdaptersDir() string { return filepath.Join(p.ConfigDir(), "adapters.d") }

// StateDir is where a profile's adapters keep workdirs and indexes.
func (p Paths) StateDir(profile string) string {
	return filepath.Join(p.StateHome, "recall", profile)
}

// CacheDir is where a profile's expansion and health caches live.
func (p Paths) CacheDir(profile string) string {
	return filepath.Join(p.CacheHome, "recall", profile)
}

// DiscoverProject walks up from dir looking for a project configuration and
// returns its path, or "" when there is none.
//
// The walk stops at the filesystem root. It does not stop at a repository
// boundary: a project file above the repository is still the file that governs
// this directory, and pretending otherwise would make the resolved
// configuration depend on where a checkout happens to sit.
func DiscoverProject(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("discovering %s: %w", ProjectFileName, err)
	}
	for {
		candidate := filepath.Join(abs, ProjectFileName)
		info, err := os.Stat(candidate)
		switch {
		case err == nil && info.Mode().IsRegular():
			return candidate, nil
		case err != nil && !os.IsNotExist(err):
			return "", fmt.Errorf("discovering %s: %w", ProjectFileName, err)
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", nil
		}
		abs = parent
	}
}

type locationResolution struct {
	declared     string
	resolved     string
	kind         LocationKind
	kindExplicit bool
	rewritten    bool
}

// resolveLocation turns a declared location into the form an adapter receives.
//
// Location is deliberately a sum type. location_kind is the authoritative
// discriminator when declared. Legacy entries without it retain syntax-based
// inference: URI syntax first, then explicit filesystem syntax, then opaque.
// Ambiguous values (slash-bearing identifiers and Windows drive syntax in
// particular) should declare their kind instead of relying on inference.
//
// Relative paths resolve against the directory of the file that declared them,
// never the working directory.
func resolveLocation(location, dir string, declaredKind *string) (locationResolution, error) {
	kind, explicit, err := resolveLocationKind(location, declaredKind)
	if err != nil {
		return locationResolution{}, err
	}
	result := locationResolution{
		declared:     location,
		resolved:     location,
		kind:         kind,
		kindExplicit: explicit,
	}

	switch result.kind {
	case LocationEmpty, LocationOpaque, LocationScheme:
		return result, nil
	case LocationPath:
		// Drive-relative paths (C:mail) are resolved by Windows against that
		// drive's working directory, not against an ordinary directory. Joining
		// one to the declaring file would change its native meaning.
		if isWindowsDriveRelativePath(location) {
			return result, nil
		}
		// A Windows-qualified path is path syntax on every platform, but only
		// Windows can resolve its drive or UNC namespace correctly. Preserve it
		// elsewhere instead of corrupting it with a POSIX base directory.
		if isWindowsQualifiedPath(location) && runtime.GOOS != "windows" {
			return result, nil
		}

		path := filepath.FromSlash(strings.ReplaceAll(location, `\`, "/"))
		switch {
		case path == "~" || strings.HasPrefix(path, "~/"):
			home, err := os.UserHomeDir()
			if err != nil {
				return locationResolution{}, fmt.Errorf("expanding %q: %w", location, err)
			}
			result.resolved = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		case filepath.IsAbs(path):
			result.resolved = filepath.Clean(path)
		default:
			result.resolved = filepath.Join(dir, path)
		}
		result.rewritten = result.resolved != result.declared
		return result, nil
	default:
		panic("unknown location kind " + result.kind)
	}
}

func resolveLocationKind(location string, declared *string) (LocationKind, bool, error) {
	if declared == nil {
		return classifyLocation(location), false, nil
	}
	if location == "" {
		return "", true, fmt.Errorf("location_kind %q requires a non-empty location", *declared)
	}
	switch *declared {
	case string(LocationPath):
		return LocationPath, true, nil
	case string(LocationOpaque):
		return LocationOpaque, true, nil
	case string(LocationScheme):
		if !hasURIScheme(location) {
			return "", true, fmt.Errorf("location_kind %q requires a URI scheme in %q", *declared, location)
		}
		return LocationScheme, true, nil
	default:
		return "", true, fmt.Errorf("location_kind %q is invalid; want path, opaque, or uri", *declared)
	}
}

func classifyLocation(location string) LocationKind {
	switch {
	case location == "":
		return LocationEmpty
	case hasURIScheme(location):
		return LocationScheme
	case location == ".", location == "..", location == "~":
		return LocationPath
	case filepath.IsAbs(location):
		return LocationPath
	case strings.HasPrefix(location, "./"), strings.HasPrefix(location, "../"),
		strings.HasPrefix(location, "~/"), strings.HasPrefix(location, `.\`),
		strings.HasPrefix(location, `..\`), strings.HasPrefix(location, `~\`):
		return LocationPath
	case strings.ContainsAny(location, `/\`):
		return LocationPath
	default:
		return LocationOpaque
	}
}

func hasURIScheme(location string) bool {
	colon := strings.IndexByte(location, ':')
	if colon <= 0 || !isASCIIAlpha(location[0]) {
		return false
	}
	for i := 1; i < colon; i++ {
		c := location[i]
		if !isASCIIAlpha(c) && (c < '0' || c > '9') && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

func isWindowsQualifiedPath(location string) bool {
	return isWindowsDrivePath(location) ||
		strings.HasPrefix(location, `\\`) ||
		strings.HasPrefix(location, "//")
}

func isWindowsDrivePath(location string) bool {
	return len(location) >= 2 && isASCIIAlpha(location[0]) && location[1] == ':'
}

func isWindowsDriveRelativePath(location string) bool {
	return isWindowsDrivePath(location) &&
		(len(location) == 2 || location[2] != '/' && location[2] != '\\')
}

func isASCIIAlpha(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
