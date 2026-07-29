package cli

import (
	"context"
	"fmt"

	"github.com/marcus/recall/pkg/recall"
)

const expandHelp = `usage: recall expand [flags] <locator>

Retrieve the evidence behind a locator, in the form "<source_id>:<local>" as
printed by recall query. Expansion is stateless with respect to the query that
produced the locator and re-checks permissions.

flags:
  --profile NAME    profile to resolve; the ceiling is applied again here
  --json            emit the response as JSON
  --detail LEVEL    summary | excerpt | full | context   (default excerpt)
  --budget BYTES    hard output limit
  --server URL      dispatch to a running recall serve instance
  --auth-token-env ENV
                    read the server bearer token from ENV

` + exitCodes

func runExpand(ctx context.Context, env Env, args []string) int {
	fs := newFlagSet("expand")
	var (
		profile = fs.String("profile", "", "profile to resolve")
		asJSON  = fs.Bool("json", false, "emit JSON")
		detail  = fs.String("detail", string(recall.DetailExcerpt), "summary|excerpt|full|context")
		budget  = fs.Int64("budget", 0, "hard output limit in bytes")
	)
	remote := addRemoteFlags(fs)
	if ok, code := parse(env, fs, expandHelp, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return usageErr(env, expandHelp, fmt.Errorf("expand takes exactly one locator"))
	}
	level, err := parseDetail(*detail)
	if err != nil {
		return usageErr(env, expandHelp, err)
	}
	locator, err := recall.ParseLocator(fs.Arg(0))
	if err != nil {
		return usageErr(env, expandHelp, err)
	}

	core, closeCore, err := openCore(env, *profile, 0, remote)
	if err != nil {
		fail(env, err)
		return ExitError
	}
	defer func() { _ = closeCore() }()

	resp, err := core.Expand(ctx, recall.ExpandRequest{
		Locator: locator,
		Detail:  level,
		Budget:  *budget,
	})
	if err != nil {
		// A locator that could not be expanded — unconfigured source, denied,
		// expired, unreachable — is a source failure and never an empty
		// document. It exits failed rather than error so a script sees the same
		// vocabulary it gets from query: the request was well formed and the
		// source could not serve it.
		fail(env, err)
		return ExitFailed
	}

	if *asJSON {
		return report(env, emitJSON(env.Stdout, resp))
	}
	var o out
	renderEvidence(&o, resp)
	return report(env, o.flush(env.Stdout))
}

// renderEvidence prints the provenance first and the content last, so a reader
// sees where text came from before reading it, and so piping the tail of the
// output is piping the evidence.
func renderEvidence(o *out, resp recall.ExpandResponse) {
	var f fields
	f.text("provenance", resp.Provenance)
	f.text("revision", resp.SourceRevision)
	f.flag("truncated", resp.Truncated)
	f.text("boundary", resp.TruncationBoundary)
	if !f.empty() {
		o.line(f.String())
		o.blank()
	}
	o.line(resp.Content)
}

func parseDetail(v string) (recall.DetailLevel, error) {
	switch level := recall.DetailLevel(v); level {
	case recall.DetailSummary, recall.DetailExcerpt, recall.DetailFull, recall.DetailContext:
		return level, nil
	default:
		return "", fmt.Errorf("detail %q: want summary, excerpt, full, or context", v)
	}
}
