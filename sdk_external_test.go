package recall_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSDKFromExternalModule proves the public SDK is usable from a module that
// is not beneath Recall's import path. The copied fixture implements an
// adapter, serves it over the protocol, and replays a recorded transcript.
func TestSDKFromExternalModule(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	if err := os.CopyFS(scratch, os.DirFS(filepath.Join("testdata", "external-sdk"))); err != nil {
		t.Fatalf("copy scratch module: %v", err)
	}

	runGo(t, scratch, "mod", "edit",
		"-replace=github.com/marcus/recall="+root)
	runGo(t, scratch, "get", "github.com/marcus/recall@v0.0.0")
	runGo(t, scratch, "mod", "tidy")
	runGo(t, scratch, "test", "./...")
}

// TestSDKPackagesDoNotDependOnRecallInternalPackages guards the dependency
// boundary directly. An external module can compile a public package that
// itself imports a sibling internal package, so the scratch-module test above
// is necessary but not sufficient to prove the SDK is self-contained.
func TestSDKPackagesDoNotDependOnRecallInternalPackages(t *testing.T) {
	output := runGo(t, ".", "list", "-deps", "-f={{.ImportPath}}", "./pkg/...")
	for _, importPath := range strings.Fields(output) {
		if strings.HasPrefix(importPath, "github.com/marcus/recall/internal/") {
			t.Errorf("public SDK depends on private package %s", importPath)
		}
	}
}

func runGo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
