package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/config"
)

// The XDG variables are how a user relocates state, and Recall's state layout
// is a documented contract: deleting cache or state may change latency, never
// results, which only holds if both live where the user said.
func TestXDGPathsHonorTheEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))

	p, err := config.XDGPaths()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, got, want string
	}{
		{"config file", p.ConfigFile(), filepath.Join(root, "cfg", "recall", "config.toml")},
		{"adapters dir", p.AdaptersDir(), filepath.Join(root, "cfg", "recall", "adapters.d")},
		{"state dir", p.StateDir("work"), filepath.Join(root, "state", "recall", "work")},
		{"cache dir", p.CacheDir("work"), filepath.Join(root, "cache", "recall", "work")},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// The documented fallbacks. A machine with no XDG variables set is the common
// case, not an error case.
func TestXDGPathsFallBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")

	p, err := config.XDGPaths()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, got, want string
	}{
		{"config home", p.ConfigHome, filepath.Join(home, ".config")},
		{"state home", p.StateHome, filepath.Join(home, ".local", "state")},
		{"cache home", p.CacheHome, filepath.Join(home, ".cache")},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
}

// The XDG specification says a relative base directory is invalid and should
// be ignored. Ignoring it silently would put every index somewhere the user
// did not ask for, and they would find out by watching a rebuild.
func TestRelativeXDGDirectoryIsAnError(t *testing.T) {
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv("XDG_STATE_HOME", "")
			t.Setenv("XDG_CACHE_HOME", "")
			t.Setenv(name, "relative/path")

			_, err := config.XDGPaths()
			if err == nil {
				t.Fatalf("%s accepted a relative path", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("message %q does not name the variable", err)
			}
		})
	}
}

// Load resolves the environment itself when no paths are supplied, which is
// what the CLI does.
func TestLoadResolvesPathsFromTheEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	writeFile(t, filepath.Join(home, "cfg", "recall", "config.toml"), `
[[sources]]
source_uid = "01J8ZKQ4MHENVAA"
source_id = "from-env"
adapter = "documents"
freshness_mode = "indexed"
`)
	cfg, err := config.Load(config.Options{Builtins: builtins})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Source("from-env"); !ok {
		t.Error("configuration under XDG_CONFIG_HOME was not loaded")
	}
}
