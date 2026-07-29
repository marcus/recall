package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/marcus/recall/internal/app"
	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/eval"
	"github.com/marcus/recall/pkg/buildinfo"
)

const evalHelp = `usage: recall eval <validate|run|compare|report> [flags]

  validate [--pack <dir>]            check a pack against the schemas
  run      [--pack <dir>] [--output d]
                                      run every case and write the artifacts
  compare  [<baseline>] <candidate>  diff two runs, and refuse a regression
  report   <run>                     re-render a run's summary

A run argument is either a run directory or a run.json on its own. The
committed baseline is a run.json, because the per-case file beside it carries
excerpts and belongs outside the repository:

  recall eval run --pack eval/packs/smoke --output "$d"
  recall eval compare eval/baselines/smoke.json "$d"

flags:
  --pack <dir>     the pack directory holding pack.json
                   defaults to evaluation.development_pack from user config
  --output <dir>   where run artifacts go; defaults under the state directory
  --cold           declare this a cold-cache run, so its latency is not pooled
                   with warm runs
  --json           machine-readable output

Artifacts carry excerpts even when packs carry only references, so they inherit
the pack's sensitivity and are written outside the repository.

With no --pack, validate and run use evaluation.development_pack from user
config. With one positional argument, compare uses
evaluation.development_baseline. Neither path has a built-in default.

exit codes: 0 the run passed every gate and the comparison found no
regression, 1 the command could not run, 3 the answer is no — a gate failed,
the runs were not comparable, or a measurement moved down.`

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

// declaresReplay reports whether a source answers from a recording its adapter
// ships with.
//
// Settings are adapter-owned and this is the one key read from outside the
// adapter, because the alternative is worse: a pack's network policy judges a
// source by its location, and an adapter over an HTTP API has to name an
// endpoint even when it will never call one. Refusing those outright would put
// every network-backed adapter permanently out of reach of a deterministic
// pack. `replay` is the settled spelling across the adapters that support one,
// and an adapter that spelled it differently would be refused here rather than
// reaching the network unnoticed.
func declaresReplay(settings map[string]any) bool {
	dir, ok := settings["replay"].(string)
	return ok && dir != ""
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
		cfg, err := env.load()
		if err != nil {
			fail(env, err)
			return ExitError
		}
		*packDir = cfg.Evaluation.DevelopmentPack
		if *packDir == "" {
			return usageErr(env, evalHelp, errors.New(
				"validate needs --pack or evaluation.development_pack in user config"))
		}
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
		cfg, err := env.load()
		if err != nil {
			fail(env, err)
			return ExitError
		}
		*packDir = cfg.Evaluation.DevelopmentPack
		if *packDir == "" {
			return usageErr(env, evalHelp, errors.New(
				"run needs --pack or evaluation.development_pack in user config"))
		}
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
		locations = append(locations, eval.SourceLocation{
			SourceID: s.ID,
			Location: s.Location,
			Replayed: declaresReplay(s.Settings),
		})
	}

	started := env.now()()
	results, err := eval.NewRunner(core, pack, eval.RunOptions{
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
		// Part of the run, so --json, the rendered summary, and a committed
		// baseline all say the same thing about what is known to be broken.
		ExpectedFailures: eval.ExpectedFailuresOf(scores),
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
	var baseline, candidate string
	switch fs.NArg() {
	case 1:
		cfg, err := env.load()
		if err != nil {
			fail(env, err)
			return ExitError
		}
		baseline = cfg.Evaluation.DevelopmentBaseline
		if baseline == "" {
			return usageErr(env, evalHelp, errors.New(
				"compare needs a baseline or evaluation.development_baseline in user config"))
		}
		candidate = fs.Arg(0)
	case 2:
		baseline, candidate = fs.Arg(0), fs.Arg(1)
	default:
		return usageErr(env, evalHelp, errors.New(
			"compare takes a candidate, or an explicit baseline and candidate"))
	}

	base, baseScores, err := readRun(baseline)
	if err != nil {
		fail(env, err)
		return ExitError
	}
	cand, candScores, err := readRun(candidate)
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
	} else {
		writeTo(env.Stdout, eval.SummarizeComparison(got))
	}

	// A comparison that prints a regression and exits 0 is a report nobody
	// reads twice. CI decides on the exit status, so a metric that moved down
	// has to be the same kind of no as a failed gate — otherwise the drift this
	// command exists to catch scrolls past in a green build. The verdict is the
	// same whether the output was rendered for a person or emitted as JSON.
	if !got.Acceptable() {
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
//
// path is either a run directory or a run record on its own. The committed
// baseline is the record alone: cases.jsonl carries excerpts and locators, so
// it inherits the pack's sensitivity and never enters the repository. A compare
// that could only read directories could not be pointed at the one baseline the
// repository actually holds, which is the whole reason the baseline exists.
func readRun(path string) (eval.Run, []eval.CaseScore, error) {
	var run eval.Run
	info, err := os.Stat(path)
	if err != nil {
		return run, nil, err
	}

	runPath, casesPath := path, ""
	if info.IsDir() {
		runPath = filepath.Join(path, eval.FileRun)
		casesPath = filepath.Join(path, eval.FileCases)
	}

	raw, err := os.ReadFile(runPath)
	if err != nil {
		return run, nil, err
	}
	if err := json.Unmarshal(raw, &run); err != nil {
		return run, nil, fmt.Errorf("%s: %w", runPath, err)
	}
	if casesPath == "" {
		return run, nil, nil
	}

	scores, err := eval.ReadCaseScores(casesPath)
	if err != nil {
		return run, nil, err
	}
	return run, scores, nil
}

func environmentFor(cold bool) eval.Environment {
	env := eval.DescribeHost()
	env.RecallCommit = buildinfo.Commit
	env.Dirty = treeIsDirty()
	env.Warm = !cold
	env.CachePolicy = "cold"
	if !cold {
		env.CachePolicy = "warm"
	}
	return env
}

// treeIsDirty reports whether the working tree had uncommitted changes.
//
// A run's numbers are reproducible from its commit or they are not, and this
// is the field that says which. It was never populated, so every artifact
// claimed a clean tree — including baselines recorded mid-change, which is the
// one moment the flag exists for.
//
// Best effort by construction: outside a checkout, or with no git on PATH,
// there is no tree to be dirty and the honest answer is false. It is not
// derived from buildinfo.Version, because a binary built once and run after an
// edit would keep reporting the state of the tree at build time.
func treeIsDirty() bool {
	cmd := exec.Command("git", "status", "--porcelain")
	out, err := cmd.Output()
	return err == nil && len(bytes.TrimSpace(out)) > 0
}
