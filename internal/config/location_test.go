package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestClassifyLocationUsesDeclaredSyntax(t *testing.T) {
	tests := []struct {
		name     string
		location string
		want     LocationKind
	}{
		{"empty", "", LocationEmpty},
		{"email account", "marcus@vorwaller.net", LocationOpaque},
		{"mailbox name", "Archive 2026", LocationOpaque},
		{"device name", "Marcus MacBook Pro", LocationOpaque},
		{"bare identifier", "notes", LocationOpaque},
		{"dot-prefixed identifier", ".index", LocationOpaque},
		{"https endpoint", "https://notes.example/api", LocationScheme},
		{"adapter scheme", "google://marcus@vorwaller.net", LocationScheme},
		{"scheme without slashes", "mailto:marcus@example.net", LocationScheme},
		{"opaque urn", "urn:uuid:1234", LocationScheme},
		{"explicit relative", "./notes", LocationPath},
		{"parent relative", "../notes", LocationPath},
		{"nested POSIX", "archive/notes", LocationPath},
		{"explicit Windows relative", `.\notes`, LocationPath},
		{"nested Windows", `archive\notes`, LocationPath},
		{"POSIX absolute", "/srv/notes", LocationPath},
		{"home", "~", LocationPath},
		{"home child", "~/notes", LocationPath},
		{"Windows drive absolute", `C:\Users\Marcus\Mail`, LocationPath},
		{"Windows drive relative", `C:Mail`, LocationPath},
		{"Windows UNC", `\\server\share\mail`, LocationPath},
		{"traversal is still a path", "../../../../etc/passwd", LocationPath},
		{"scheme traversal is opaque", "https://example.test/../../admin", LocationScheme},
		{"scheme colon beats separators", "google:mail/archive", LocationScheme},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyLocation(tc.location); got != tc.want {
				t.Errorf("classifyLocation(%q) = %q, want %q", tc.location, got, tc.want)
			}
		})
	}
}

func TestResolveLocationRewritesOnlyPaths(t *testing.T) {
	base := filepath.Join(t.TempDir(), "config")
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)

	tests := []struct {
		name, location, want string
		rewritten            bool
	}{
		{"email", "marcus@vorwaller.net", "marcus@vorwaller.net", false},
		{"device", "Marcus MacBook Pro", "Marcus MacBook Pro", false},
		{"URL", "https://example.test/a/../b", "https://example.test/a/../b", false},
		{"scheme", "google://marcus@vorwaller.net", "google://marcus@vorwaller.net", false},
		{"relative", "./notes", filepath.Join(base, "notes"), true},
		{"parent", "../notes", filepath.Join(filepath.Dir(base), "notes"), true},
		{"nested", "archive/notes", filepath.Join(base, "archive", "notes"), true},
		{"Windows separators", `archive\notes`, filepath.Join(base, "archive", "notes"), true},
		{"home", "~/notes", filepath.Join(home, "notes"), true},
		{"absolute clean", "/srv/../data", "/data", true},
		{"malicious traversal", "../../../../etc/passwd", filepath.Clean(filepath.Join(base, "../../../../etc/passwd")), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveLocation(tc.location, base)
			if err != nil {
				t.Fatal(err)
			}
			if got.declared != tc.location {
				t.Errorf("declared = %q, want exact input %q", got.declared, tc.location)
			}
			if got.resolved != tc.want {
				t.Errorf("resolved = %q, want %q", got.resolved, tc.want)
			}
			if got.rewritten != tc.rewritten {
				t.Errorf("rewritten = %v, want %v", got.rewritten, tc.rewritten)
			}
		})
	}
}

func TestForeignWindowsPathsAreNotCorrupted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("this property is about preserving foreign path syntax")
	}
	for _, location := range []string{`C:\Users\Marcus\Mail`, `C:Mail`, `\\server\share\mail`} {
		got, err := resolveLocation(location, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if got.kind != LocationPath || got.resolved != location || got.rewritten {
			t.Errorf("resolveLocation(%q) = %+v; foreign path must be classified but preserved", location, got)
		}
	}
}

func TestLocationClassificationDoesNotDependOnExistence(t *testing.T) {
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := resolveLocation("notes", base)
	if err != nil {
		t.Fatal(err)
	}
	if got.kind != LocationOpaque || got.resolved != "notes" || got.rewritten {
		t.Fatalf("existing bare name changed the syntax decision: %+v", got)
	}
}

// Seeded fuzz tests are ordinary property tests under `go test`, and become
// full fuzz targets when run with -fuzz. No opaque identifier or URI scheme may
// ever be changed merely because of its spelling or the contents of base.
func FuzzResolveNonPathIsIdentity(f *testing.F) {
	for _, seed := range []string{
		"", "marcus@example.net", "Archive 2026", "device-01", ".index",
		"https://example.test/a/../b", "google://marcus@example.net",
		"mailto:marcus@example.net", "urn:uuid:1234",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, location string) {
		kind := classifyLocation(location)
		if kind != LocationEmpty && kind != LocationOpaque && kind != LocationScheme {
			t.Skip()
		}
		got, err := resolveLocation(location, filepath.Join(t.TempDir(), "base"))
		if err != nil {
			t.Fatal(err)
		}
		if got.resolved != location || got.rewritten {
			t.Fatalf("non-path %q (%s) resolved as %+v", location, kind, got)
		}
	})
}
