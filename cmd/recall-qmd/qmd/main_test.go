package qmd_test

import (
	"os"
	"testing"

	"github.com/marcus/recall/cmd/recall-qmd/qmd"
)

// TestMain keeps index maintenance locks out of the user's cache directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "recall-qmd-locks-")
	if err != nil {
		panic(err)
	}
	os.Setenv(qmd.LockDirEnv, dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
