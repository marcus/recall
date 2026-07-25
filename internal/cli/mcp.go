package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/marcus/recall/internal/api"
)

const mcpHelp = `usage: recall mcp [flags]

Run Recall as a Model Context Protocol server over stdio. Standard output is
reserved for protocol messages; diagnostics go to standard error.

flags:
  --profile NAME  profile served for this process lifetime
`

func runMCP(ctx context.Context, env Env, args []string) int {
	fs := newFlagSet("mcp")
	profile := fs.String("profile", "", "profile served for this process lifetime")
	if ok, code := parse(env, fs, mcpHelp, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageErr(env, mcpHelp, errors.New("mcp takes no arguments"))
	}

	core, closeCore, err := openCore(env, *profile, 0, remoteFlags{})
	if err != nil {
		fail(env, err)
		return ExitError
	}
	defer func() { _ = closeCore() }()

	err = api.ServeMCP(ctx, env.stdin(), env.Stdout, api.MCPOptions{
		Core: core,
		Log:  func(line string) { _, _ = fmt.Fprintln(env.Stderr, "recall mcp:", line) },
	})
	if err != nil {
		fail(env, err)
		return ExitError
	}
	return ExitOK
}
