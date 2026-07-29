// Package cli is the command-line transport over the application layer.
//
// Boundary: it parses arguments, renders, and chooses an exit code. It owns no
// ranking, permission, or expansion behavior. Every retrieval decision is made
// in internal/app, every eligibility decision in internal/source, and every
// score explanation is rendered by internal/explain. A rule that lived here and
// nowhere else would be a rule the HTTP and MCP transports do not have, and the
// spec requires every surface to behave identically.
//
// Human and JSON output carry the same facts. A zero value is not a fact:
// human output omits empty fields the way internal/explain does, so the rule is
// "every value the JSON carries appears in the text", not "every key does".
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/marcus/recall/internal/adapters/docs"
	"github.com/marcus/recall/internal/adapters/tasks"
	"github.com/marcus/recall/internal/adapters/td"
	"github.com/marcus/recall/internal/api"
	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/source"
	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/buildinfo"
	"github.com/marcus/recall/pkg/recall"
)

// Exit codes. A script has to be able to tell "nothing matched" from "the
// sources were down", so the outcome of a query is in the exit status and not
// only in the output. They are ordered by severity, and the most severe
// applicable code wins: a degraded run that also abstained exits degraded,
// because the abstention is not trustworthy.
const (
	// ExitOK means results were returned and every eligible source answered.
	ExitOK = 0

	// ExitError means the command could not run at all: bad usage, invalid
	// configuration, an unreadable locator, a failed expansion, a doctor check
	// that failed. It is never a statement about the corpus.
	ExitError = 1

	// ExitAbstained means nothing matched and at least one source answered.
	// That is a claim about the corpus, and it is supported.
	ExitAbstained = 2

	// ExitDegraded means a source that was eligible could not answer, so
	// whatever came back came from an incomplete set of sources.
	ExitDegraded = 3

	// ExitFailed means every source that was asked failed. "No results" would
	// be a claim nothing looked hard enough to support.
	ExitFailed = 4
)

// Adapter is one built-in adapter this binary can serve: what configuration
// must validate against, and how to construct it.
type Adapter struct {
	Name           string
	FreshnessModes []recall.FreshnessMode
	New            source.Factory
}

// Builtins are the adapters compiled into recall.
//
// It is a function rather than a variable so a caller cannot mutate the set the
// next invocation sees, and so tests can pass their own without the real Tasks
// binary or a real corpus ever being reachable.
func Builtins() []Adapter {
	return []Adapter{
		{
			// "documents" rather than "docs": the name appears in every
			// user's configuration and in every evaluation pack, and it names
			// the source class the spec defines. The package is docs; the
			// configured adapter is documents.
			Name:           "documents",
			FreshnessModes: []recall.FreshnessMode{recall.FreshnessIndexed},
			New:            func() adapter.Adapter { return docs.New() },
		},
		{
			Name:           "tasks",
			FreshnessModes: []recall.FreshnessMode{recall.FreshnessLive},
			New:            func() adapter.Adapter { return tasks.New(tasks.Options{}) },
		},
		{
			// One adapter, one configured source per td workspace. It is
			// built in rather than external because the same workspaces are
			// read from both the home and the work install, and an adapter
			// shared across installs is one this binary should carry.
			Name:           "td",
			FreshnessModes: []recall.FreshnessMode{recall.FreshnessLive},
			New:            func() adapter.Adapter { return td.New(td.Options{}) },
		},
	}
}

// Env is everything the CLI touches outside its arguments. Nothing here reads
// a global: a test builds a whole machine — config home, state, corpus,
// adapters, clock — and the command cannot reach past it.
type Env struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// Dir is where project configuration discovery starts. Empty means the
	// process working directory.
	Dir string

	// Paths overrides XDG resolution. A zero value resolves from the
	// environment.
	Paths config.Paths

	// Adapters replaces the compiled-in set. Nil means [Builtins].
	Adapters []Adapter

	// Core replaces the in-process application core. It is a test and embedding
	// seam; normal commands build one from configuration.
	Core api.Core

	// LookupEnv resolves a named environment variable. Nil uses os.LookupEnv.
	LookupEnv func(string) (string, bool)

	// Now is injectable so a test can pin the clock and a golden file can be a
	// diff of formatting rather than of timing.
	Now func() time.Time
}

func (e Env) adapters() []Adapter {
	if e.Adapters == nil {
		return Builtins()
	}
	return e.Adapters
}

func (e Env) now() func() time.Time {
	if e.Now == nil {
		return time.Now
	}
	return e.Now
}

func (e Env) stdin() io.Reader {
	if e.Stdin == nil {
		return os.Stdin
	}
	return e.Stdin
}

func (e Env) lookupEnv(name string) (string, bool) {
	if e.LookupEnv == nil {
		return os.LookupEnv(name)
	}
	return e.LookupEnv(name)
}

// Run executes one command and returns the process exit code.
func Run(ctx context.Context, env Env) int {
	if len(env.Args) == 0 {
		writeTo(env.Stdout, usage)
		return ExitError
	}

	cmd, args := env.Args[0], env.Args[1:]
	switch cmd {
	case "init":
		return runInit(env, args)
	case "query":
		return runQuery(ctx, env, args)
	case "expand":
		return runExpand(ctx, env, args)
	case "refresh":
		return runRefresh(ctx, env, args)
	case "sources":
		return runSources(ctx, env, args)
	case "doctor":
		return runDoctor(ctx, env, args)
	case "config":
		return runConfig(ctx, env, args)
	case "eval":
		return evalCmd(ctx, env, args)
	case "serve":
		return runServe(ctx, env, args)
	case "mcp":
		return runMCP(ctx, env, args)
	case "version", "--version":
		_, err := fmt.Fprintf(env.Stdout, "recall %s (%s)\n", buildinfo.Version, buildinfo.Commit)
		return report(env, err)
	case "help", "-h", "--help":
		writeTo(env.Stdout, usage)
		return ExitOK
	default:
		fail(env, fmt.Errorf("unknown command %q", cmd))
		writeTo(env.Stderr, usage)
		return ExitError
	}
}

const exitCodes = `exit codes:
  0  answered    results were returned and every eligible source answered
  1  error       the command could not run: bad usage, invalid configuration,
                 an unreadable locator, or a doctor check that failed
  2  abstained   nothing matched, and at least one source answered
  3  degraded    a source that was eligible could not answer, so any results
                 came from an incomplete set of sources
  4  failed      every source that was asked failed, so "no results" would be a
                 claim nothing supports; for expand, the source could not
                 produce the evidence
`

const usage = `recall — portable retrieval over user-controlled sources

usage: recall <command> [flags] [arguments]

commands:
  init --docs DIR   create a first user configuration
  query <text>      search and fuse configured sources
  expand <locator>  retrieve evidence from a locator
  refresh           update one or every eligible checkpoint-capable source
  sources           list source instances, capabilities, health, and freshness
  doctor            validate configuration, trust boundary, access, health,
                    freshness, identity, and lineage
  config explain    print the resolved configuration and each value's origin
  eval              validate, run, compare, and report an evaluation pack
  serve             run the versioned HTTP API
  mcp               run the Model Context Protocol server over stdio
  version           print the build identity

Every read command supports --json alongside its human output, and both carry
the same facts.

` + exitCodes + `
Run "recall <command> --help" for a command's flags.
`

// newFlagSet builds a flag set that reports nothing itself. Usage and errors
// are printed by the command, once, so a parse error and a bad argument look
// the same to a reader.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// parse handles the two outcomes a flag set has besides success: an explicit
// help request, which is not an error, and a bad flag, which is.
func parse(env Env, fs *flag.FlagSet, help string, args []string) (bool, int) {
	if err := parseInterleaved(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeTo(env.Stdout, help)
			return false, ExitOK
		}
		fail(env, err)
		writeTo(env.Stderr, help)
		return false, ExitError
	}
	return true, ExitOK
}

// parseInterleaved parses flags that appear before, after, or among the
// positional arguments.
//
// The flag package stops at the first non-flag argument, so `recall query "x"
// --json` would treat --json as a second query term and report a usage error.
// That is the order most people type, and losing a flag silently would be
// worse than the error: --json changes the output format a script is parsing.
//
// Each pass consumes one positional and re-parses the rest, which keeps the
// flag package's own knowledge of which flags take values.
func parseInterleaved(fs *flag.FlagSet, args []string) error {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	// fs.Args() is how commands read their operands, so the collected
	// positionals are put back by parsing them as a trailing group.
	return fs.Parse(append([]string{"--"}, positional...))
}

// usageErr reports a problem with the arguments themselves and shows the help
// that would have prevented it.
func usageErr(env Env, help string, err error) int {
	fail(env, err)
	writeTo(env.Stderr, help)
	return ExitError
}

func fail(env Env, err error) {
	_, _ = fmt.Fprintf(env.Stderr, "recall: %v\n", err)
}

func writeTo(w io.Writer, s string) {
	_, _ = io.WriteString(w, s)
}

// report turns a write failure into an exit code. Output that could not be
// written is not a successful command: a caller redirecting into a full disk
// must not be told the query answered.
func report(env Env, err error) int {
	if err != nil {
		fail(env, err)
		return ExitError
	}
	return ExitOK
}
