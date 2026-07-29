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

func runGo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
