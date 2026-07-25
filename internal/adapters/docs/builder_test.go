package docs_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/adapters/docs"
	"github.com/marcus/recall/internal/protocol"
)

// builderEnv re-executes this test binary as a real index builder.
//
// A build that is interrupted rather than failed can only be produced by
// killing a process: SIGKILL runs no deferred cleanup and no error path, so
// what survives is what the write ordering guaranteed by itself. Re-executing
// the test binary keeps that hermetic — no build step, no fixture program, and
// the builder under test is the exported one.
const builderEnv = "RECALL_DOCS_TEST_BUILDER"

func TestMain(m *testing.M) {
	if spec := os.Getenv(builderEnv); spec != "" {
		os.Exit(runBuilder(spec))
	}
	os.Exit(m.Run())
}

// runBuilder handshakes against an existing workdir and rebuilds. It announces
// itself on stdout first, so the parent kills a build that has certainly
// started rather than racing the process spawn.
func runBuilder(spec string) int {
	root, workdir, ok := strings.Cut(spec, string(os.PathListSeparator))
	if !ok {
		fmt.Fprintln(os.Stderr, "builder: want <root>"+string(os.PathListSeparator)+"<workdir>")
		return 2
	}

	a := docs.New()
	if _, err := a.Initialize(context.Background(), config(root, workdir, nil)); err != nil {
		fmt.Fprintln(os.Stderr, "builder: handshake:", err)
		return 1
	}
	fmt.Println("building")
	if _, err := a.Refresh(context.Background(), protocol.RefreshParams{}); err != nil {
		fmt.Fprintln(os.Stderr, "builder: build:", err)
		return 1
	}
	fmt.Println("published")
	return 0
}
