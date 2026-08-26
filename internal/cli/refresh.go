package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/marcus/recall/pkg/recall"
)

const refreshHelp = `usage: recall refresh [flags]

Update one source, or every eligible checkpoint-capable source in the active
profile when --source is omitted. A named live source that cannot checkpoint
is probed for health rather than skipped. The response reports every source
considered and the post-refresh health of each attempted source.

flags:
  --source ID       refresh this source id; live sources are health-probed
  --full            request a complete rebuild rather than an incremental pass
  --profile NAME    profile to resolve; default is the configured one
  --json            emit the typed result as JSON
  --server URL      dispatch to a running recall serve instance
  --auth-token-env ENV
                    read the server bearer token from ENV

` + exitCodes

func runRefresh(ctx context.Context, env Env, args []string) int {
	fs := newFlagSet("refresh")
	var (
		sourceID = fs.String("source", "", "source id to refresh")
		full     = fs.Bool("full", false, "request a complete rebuild")
		profile  = fs.String("profile", "", "profile to resolve")
		asJSON   = fs.Bool("json", false, "emit JSON")
	)
	remote := addRemoteFlags(fs)
	if ok, code := parse(env, fs, refreshHelp, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageErr(env, refreshHelp, fmt.Errorf("refresh takes no arguments; use --source ID"))
	}
	normalizedSource := strings.TrimSpace(*sourceID)
	if *sourceID != "" && normalizedSource == "" {
		return usageErr(env, refreshHelp, fmt.Errorf("--source must not be blank; omit it to refresh all eligible sources"))
	}

	core, closeCore, err := openCore(env, *profile, 0, remote)
	if err != nil {
		fail(env, err)
		return ExitError
	}
	defer func() { _ = closeCore() }()

	resp, err := core.Refresh(ctx, recall.RefreshRequest{
		Profile:  *profile,
		SourceID: normalizedSource,
		Full:     *full,
	})
	if err != nil {
		fail(env, err)
		return ExitError
	}
	if *asJSON {
		if code := report(env, emitJSON(env.Stdout, resp)); code != ExitOK {
			return code
		}
	} else {
		var o out
		renderRefresh(&o, resp)
		if code := report(env, o.flush(env.Stdout)); code != ExitOK {
			return code
		}
	}
	return refreshExit(resp.Outcome)
}

func refreshExit(outcome recall.RefreshOutcome) int {
	switch outcome {
	case recall.RefreshSucceeded:
		return ExitOK
	case recall.RefreshDegraded:
		return ExitDegraded
	default:
		return ExitFailed
	}
}

func renderRefresh(o *out, resp recall.RefreshResponse) {
	var head fields
	head.text("outcome", string(resp.Outcome))
	head.dur("elapsed", resp.Elapsed)
	head.count("sources", len(resp.Sources))
	o.line(head.String())
	for _, source := range resp.Sources {
		var row fields
		row.text("source", source.SourceID)
		row.text("status", string(source.Status))
		row.text("reason", string(source.Reason))
		row.text("detail", source.DiagnosticDetail)
		row.dur("elapsed", source.Elapsed)
		if source.Health != nil {
			row.text("health", string(source.Health.Status))
			row.text("coverage", string(source.Health.Coverage))
			row.text("generation", source.Health.IndexGeneration)
			row.text("source watermark", source.Health.SourceWatermark)
			row.text("index watermark", source.Health.IndexWatermark)
		}
		o.line(row.String())
	}
}
