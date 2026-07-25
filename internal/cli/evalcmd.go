package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/marcus/recall/internal/app"
	"github.com/marcus/recall/internal/buildinfo"
	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/eval"
)

const evalHelp = `usage: recall eval <validate|run|compare|report> [flags]

  validate --pack <dir>              check a pack against the schemas
  run      --pack <dir> [--output d] run every case and write the artifacts
  compare  <baseline> <candidate>    diff two runs
  report   <run>                     re-render a run's summary

flags:
  --pack <dir>     the pack directory holding pack.json
  --output <dir>   where run artifacts go; defaults under the state directory
  --cold           declare this a cold-cache run, so its latency is not pooled
                   with warm runs
  --json           machine-readable output

Artifacts carry excerpts even when packs carry only references, so they inherit
the pack's sensitivity and are written outside the repository.

exit codes: 0 the run passed every gate, 1 the command could not run,
3 the run is invalid because a gate failed.`

// ExitInvalid means the run completed and is not admissible evidence.
//
// It is distinct from ExitError, which means the command could not run at all:
// a script deciding whether to promote a change needs to tell "the gates say no"
// from "the harness broke".
const ExitInvalid = 3

func evalCmd(ctx context.Context, env Env, args []string) int {
	if len(args) == 0 {
		writeTo(env.Stderr, evalHelp)
		return ExitError
	}
	sub, rest := args[0], args[1:]

	switch sub {
	case "validate":
		return evalValidate(env, rest)
	case "run":
		return evalRun(ctx, env, rest)
	case "compare":
		return evalCompare(env, rest)
	case "report":
		return evalReport(env, rest)
	default:
		fail(env, fmt.Errorf("unknown eval command %q", sub))
		writeTo(env.Stderr, evalHelp)
		return ExitError
	}
}

// packConfig loads the configuration a pack brings with it.
//
// A pack that measured whatever the operator happened to have configured would
// not be measuring the same thing twice, so the pack's own sources/config.toml
// is the user layer for the run, and no project file is discovered.
//
// ${PACK} is substituted for the pack directory, because a committed
// configuration cannot name absolute paths on a machine it has never seen.
func packConfig(env Env, pack *eval.Pack) (*config.Config, error) {
	src := filepath.Join(pack.Dir(), "sources", "config.toml")
	raw, err := os.ReadFile(src)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(pack.Dir())
	if err != nil {
		return nil, err
	}

	home, err := os.MkdirTemp("", "recall-eval-config-")
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, "recall")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	body := strings.ReplaceAll(string(raw), "${PACK}", abs)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o600); err != nil {
		return nil, err
	}

	builtins := make([]config.Builtin, 0, len(env.adapters()))
	for _, a := range env.adapters() {
		builtins = append(builtins, config.Builtin{Name: a.Name, FreshnessModes: a.FreshnessModes})
	}
	return config.Load(config.Options{
		Paths: config.Paths{
			ConfigHome: home,
			StateHome:  filepath.Join(home, "state"),
			CacheHome:  filepath.Join(home, "cache"),
		},
		Builtins: builtins,
	})
}

// loadPack reads a pack and its two files. Cases and judgments load separately
// so a runner can hand out the queries without the answers.
func loadPack(dir string) (*eval.Pack, []eval.Case, []eval.Judgment, error) {
	pack, err := eval.LoadPack(dir)
	if err != nil {
		return nil, nil, nil, err
	}
	cases, err := pack.LoadCases()
	if err != nil {
		return nil, nil, nil, err
	}
	judgments, err := pack.LoadJudgments()
	if err != nil {
		return nil, nil, nil, err
	}
	return pack, cases, judgments, nil
}

func evalValidate(env Env, args []string) int {
	fs, jsonOut := evalFlags(env, "validate")
	packDir := fs.String("pack", "", "pack directory")
	if ok, code := parse(env, fs, evalHelp, args); !ok {
		return code
	}
	if *packDir == "" {
		return usageErr(env, evalHelp, errors.New("validate needs --pack"))
	}

	pack, cases, judgments, err := loadPack(*packDir)
	if err != nil {
		fail(env, err)
		return ExitError
	}
	if err := eval.Validate(pack, cases, judgments); err != nil {
		fail(env, err)
		return ExitError
	}
	hash, err := eval.ContentHash(pack, cases, judgments)
	if err != nil {
		fail(env, err)
		return ExitError
	}

	if *jsonOut {
		if err := emitJSON(env.Stdout, map[string]any{
			"pack_id": pack.PackID, "version": pack.Version,
			"cases": len(cases), "judgments": len(judgments), "content_hash": hash,
		}); err != nil {
			fail(env, err)
			return ExitError
		}
		return ExitOK
	}
	writeTo(env.Stdout, fmt.Sprintf("%s %s valid — %d cases, %d judgments, content %s\n",
		pack.PackID, pack.Version, len(cases), len(judgments), hash[:12]))
	return ExitOK
}

func evalRun(ctx context.Context, env Env, args []string) int {
	fs, jsonOut := evalFlags(env, "run")
	packDir := fs.String("pack", "", "pack directory")
	output := fs.String("output", "", "artifact directory")
	cold := fs.Bool("cold", false, "declare a cold-cache run")
	if ok, code := parse(env, fs, evalHelp, args); !ok {
		return code
	}
	if *packDir == "" {
		return usageErr(env, evalHelp, errors.New("run needs --pack"))
	}

	pack, cases, judgments, err := loadPack(*packDir)
	if err != nil {
		fail(env, err)
		return ExitError
	}
	if err := eval.Validate(pack, cases, judgments); err != nil {
		fail(env, err)
		return ExitError
	}

	// The pack brings its own configuration: a pack that measured whatever the
	// operator happened to have configured would not be measuring the same
	// thing twice.
	cfg, err := packConfig(env, pack)
	if err != nil {
		fail(env, err)
		return ExitError
	}

	core, registry, err := app.Build(app.BuildOptions{
		Config:   cfg,
		Builtins: factories(env),
		StateDir: cfg.Paths.StateDir(pack.Profile),
		Now:      env.now(),
	})
	if err != nil {
		fail(env, err)
		return ExitError
	}
	defer func() { _ = registry.Close() }()

	locations := make([]eval.SourceLocation, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		locations = append(locations, eval.SourceLocation{SourceID: s.ID, Location: s.Location})
	}

	started := env.now()()
	results, err := eval.NewRunner(core, pack, eval.RunOptions{
		Clock:     env.now(),
		Locations: locations,
		Cold:      *cold,
	}).Run(ctx, cases)
	if err != nil {
		fail(env, err)
		return ExitError
	}

	scores, err := eval.Scores(cases, judgments, results)
	if err != nil {
		fail(env, err)
		return ExitError
	}
	report := eval.ReportOf(scores)
	gates := eval.EvaluateGates(pack, scores, report, nil)

	hash, err := eval.ContentHash(pack, cases, judgments)
	if err != nil {
		fail(env, err)
		return ExitError
	}
	run := eval.Run{
		SchemaVersion: 1,
		RunID:         fmt.Sprintf("%s-%d", pack.PackID, started.UnixNano()),
		Status:        eval.StatusOf(gates),
		StartedAt:     started,
		FinishedAt:    env.now()(),
		Pack:          eval.PackRef{PackID: pack.PackID, Version: pack.Version, ContentHash: hash},
		Environment:   environmentFor(*cold),
		Metrics:       report,
		Gates:         gates,
	}

	dir := *output
	if dir == "" {
		dir = filepath.Join(cfg.Paths.StateDir(pack.Profile), "eval", run.RunID)
	}
	if err := eval.WriteRun(dir, run, results, scores); err != nil {
		fail(env, err)
		return ExitError
	}

	if *jsonOut {
		if err := emitJSON(env.Stdout, run); err != nil {
			fail(env, err)
			return ExitError
		}
	} else {
		writeTo(env.Stdout, eval.Summarize(run, scores))
		writeTo(env.Stdout, fmt.Sprintf("\nartifacts: %s\n", dir))
	}
	if run.Status != eval.StatusPass {
		return ExitInvalid
	}
	return ExitOK
}

func evalCompare(env Env, args []string) int {
	fs, jsonOut := evalFlags(env, "compare")
	if ok, code := parse(env, fs, evalHelp, args); !ok {
		return code
	}
	if fs.NArg() != 2 {
		return usageErr(env, evalHelp, errors.New("compare takes a baseline and a candidate run directory"))
	}

	base, baseScores, err := readRun(fs.Arg(0))
	if err != nil {
		fail(env, err)
		return ExitError
	}
	cand, candScores, err := readRun(fs.Arg(1))
	if err != nil {
		fail(env, err)
		return ExitError
	}

	got := eval.Compare(base, cand, baseScores, candScores)
	if *jsonOut {
		if err := emitJSON(env.Stdout, got); err != nil {
			fail(env, err)
			return ExitError
		}
		return ExitOK
	}
	writeTo(env.Stdout, eval.SummarizeComparison(got))
	if !got.Comparable {
		return ExitInvalid
	}
	return ExitOK
}

func evalReport(env Env, args []string) int {
	fs, jsonOut := evalFlags(env, "report")
	if ok, code := parse(env, fs, evalHelp, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		return usageErr(env, evalHelp, errors.New("report takes one run directory"))
	}

	run, scores, err := readRun(fs.Arg(0))
	if err != nil {
		fail(env, err)
		return ExitError
	}
	if *jsonOut {
		if err := emitJSON(env.Stdout, run); err != nil {
			fail(env, err)
			return ExitError
		}
		return ExitOK
	}
	writeTo(env.Stdout, eval.Summarize(run, scores))
	if run.Status != eval.StatusPass {
		return ExitInvalid
	}
	return ExitOK
}

func evalFlags(env Env, name string) (*flag.FlagSet, *bool) {
	fs := newFlagSet("eval " + name)
	return fs, fs.Bool("json", false, "machine-readable output")
}

// readRun reads a run and its per-case scores back off disk.
func readRun(dir string) (eval.Run, []eval.CaseScore, error) {
	var run eval.Run
	raw, err := os.ReadFile(filepath.Join(dir, eval.FileRun))
	if err != nil {
		return run, nil, err
	}
	if err := json.Unmarshal(raw, &run); err != nil {
		return run, nil, fmt.Errorf("%s: %w", dir, err)
	}

	scores, err := eval.ReadCaseScores(filepath.Join(dir, eval.FileCases))
	if err != nil {
		return run, nil, err
	}
	return run, scores, nil
}

func environmentFor(cold bool) eval.Environment {
	env := eval.DescribeHost()
	env.RecallCommit = buildinfo.Commit
	env.Warm = !cold
	env.CachePolicy = "cold"
	if !cold {
		env.CachePolicy = "warm"
	}
	return env
}
