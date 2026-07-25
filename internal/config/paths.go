package config

import (
	"fmt"
	"os"
	"path/filepath"
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

// resolveLocation turns a declared location into the form an adapter receives.
//
// A relative path resolves against the directory of the file that declared it,
// never the working directory: a project file means the path it wrote, wherever
// Recall is invoked from. A location carrying a scheme is an endpoint or
// connection reference and is left exactly as written.
func resolveLocation(location, dir string) (string, error) {
	switch {
	case location == "":
		return "", nil
	case strings.Contains(location, "://"):
		return location, nil
	case location == "~" || strings.HasPrefix(location, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding %q: %w", location, err)
		}
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(location, "~"), "/")), nil
	case filepath.IsAbs(location):
		return filepath.Clean(location), nil
	default:
		return filepath.Join(dir, location), nil
	}
}
