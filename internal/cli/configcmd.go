package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/marcus/recall/internal/config"
)

const configHelp = `usage: recall config explain [flags]

Print the resolved configuration with the origin of every value: what Recall
will do, and which file said so. Two layers merge silently by design, so this is
how a value you wrote is told apart from one a project file supplied or one that
was defaulted.

Secrets are references throughout — "env_var:NAME", "keychain:NAME" — and no
value is ever read from the environment or a keychain to print it.

subcommands:
  explain    print the resolved configuration

flags:
  --profile NAME    profile whose state and cache directories are reported
  --json            emit the explanation as JSON

` + exitCodes

func runConfig(_ context.Context, env Env, args []string) int {
	if len(args) == 0 {
		return usageErr(env, configHelp, fmt.Errorf("config takes a subcommand"))
	}
	switch args[0] {
	case "explain":
		return runConfigExplain(env, args[1:])
	case "-h", "--help", "help":
		writeTo(env.Stdout, configHelp)
		return ExitOK
	default:
		return usageErr(env, configHelp, fmt.Errorf("unknown config subcommand %q", args[0]))
	}
}

func runConfigExplain(env Env, args []string) int {
	fs := newFlagSet("config explain")
	var (
		profile = fs.String("profile", "", "profile to resolve")
		asJSON  = fs.Bool("json", false, "emit JSON")
	)
	if ok, code := parse(env, fs, configHelp, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageErr(env, configHelp, fmt.Errorf("config explain takes no arguments"))
	}

	cfg, err := env.load()
	if err != nil {
		// An unloadable configuration has no resolved values to explain.
		// `recall doctor` is the command that itemizes why.
		fail(env, err)
		return ExitError
	}
	explanation := cfg.Explain()
	if name := *profile; name != "" {
		// A profile nobody configured has no resolved state, so a typo is
		// reported rather than answered with directories that will never hold
		// anything.
		if _, err := cfg.ActiveProfile(name); err != nil {
			fail(env, err)
			return ExitError
		}
		// The paths a profile writes under are profile-scoped, so an explicit
		// --profile must not be answered with the default's directories.
		explanation.Paths.StateDir = cfg.Paths.StateDir(name)
		explanation.Paths.CacheDir = cfg.Paths.CacheDir(name)
	}

	if *asJSON {
		return report(env, emitJSON(env.Stdout, explanation))
	}
	var o out
	renderExplanation(&o, explanation)
	return report(env, o.flush(env.Stdout))
}

func renderExplanation(o *out, e *config.Explanation) {
	o.line("paths")
	o.block("  ", "config file  "+e.Paths.ConfigFile)
	o.block("  ", "adapters dir "+e.Paths.AdaptersDir)
	o.block("  ", "state dir    "+e.Paths.StateDir)
	o.block("  ", "cache dir    "+e.Paths.CacheDir)

	o.blank()
	o.line("files")
	if len(e.Files) == 0 {
		o.block("  ", "none; built-in defaults are in force")
	}
	for _, f := range e.Files {
		o.block("  ", fmt.Sprintf("%-8s %s", f.Layer, f.Path))
	}

	o.blank()
	o.line("defaults")
	renderFields(o, "  ", e.Defaults)

	o.blank()
	o.line("adapters")
	for _, a := range e.Adapters {
		var f fields
		f.flag("builtin", a.Builtin)
		f.text("command", a.Command)
		f.text("args", strings.Join(a.Args, " "))
		f.text("env", mapLine(a.Env))
		f.text("freshness modes", strings.Join(a.FreshnessModes, ", "))
		f.text("conformance", a.Conformance)
		f.text("secrets", mapLine(a.Secrets))
		o.block("  ", fmt.Sprintf("%s  %s  [%s]", a.Name, f.String(), origin(string(a.Layer), a.Origin)))
	}

	o.blank()
	o.line("sources")
	for _, s := range e.Sources {
		o.block("  ", fmt.Sprintf("%s (%s)  declared in %s", s.SourceID, s.SourceUID, s.DeclaredIn))
		renderFields(o, "    ", s.Fields)
		renderNamedFields(o, "    intent_priors", s.IntentPriors)
		renderNamedFields(o, "    settings", s.Settings)
		renderNamedFields(o, "    secrets", s.Secrets)
	}

	o.blank()
	o.line("profiles")
	for _, p := range e.Profiles {
		o.block("  ", fmt.Sprintf("%s  max_sensitivity %s  sources %v",
			p.Name, p.MaxSensitivity, p.SourceIDs))
		renderFields(o, "    ", p.Fields)
	}

	o.blank()
	o.line("identity")
	for _, m := range e.Identity {
		o.block("  ", fmt.Sprintf("%-20s %s", m.SourceID, m.SourceUID))
	}
}

func renderFields(o *out, indent string, in map[string]config.Field) {
	for _, name := range slices.Sorted(maps.Keys(in)) {
		f := in[name]
		// The value is rendered to a string before it is padded: a width verb
		// applied to a slice pads every element instead of the whole value, and
		// the result reads as if the padding were part of the data.
		o.block(indent, fmt.Sprintf("%-18s %-28s [%s]",
			name, fmt.Sprintf("%v", f.Value), origin(string(f.Layer), f.Origin)))
	}
}

func renderNamedFields(o *out, header string, in map[string]config.Field) {
	if len(in) == 0 {
		return
	}
	o.line(header)
	renderFields(o, strings.Repeat(" ", indentOf(header)+2), in)
}

// origin names where a value came from. The layer alone is enough for a
// default or a built-in, which have no file.
func origin(layer, file string) string {
	if file == "" {
		return layer
	}
	return layer + " " + file
}

func mapLine(in map[string]string) string {
	if len(in) == 0 {
		return ""
	}
	as := make(map[string]any, len(in))
	for k, v := range in {
		as[k] = v
	}
	return diagnostics(as)
}

func indentOf(s string) int {
	for i, r := range s {
		if r != ' ' {
			return i
		}
	}
	return len(s)
}
