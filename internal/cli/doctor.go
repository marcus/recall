package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/recall"
)

const doctorHelp = `usage: recall doctor [flags]

Check everything that has to be true before a query means anything:

  configuration    the layers load, merge, and validate
  trust_boundary   the project layer stayed inside what a cloned repository
                   may declare
  identity         every source has a source_uid, and no two share one
  access           every eligible source's location can be read
  health           every eligible source can be reached and is usable
  freshness        each source's freshness mode is one its adapter serves
  lineage          declared source-level derivation has no cycle

flags:
  --profile NAME    profile to resolve; default is the configured one
  --json            emit the diagnosis as JSON

` + exitCodes

// Check outcomes.
const (
	CheckPass    = "pass"
	CheckFail    = "fail"
	CheckSkipped = "skipped"
)

// Problem is one located defect. File and Key are carried separately from the
// message so a machine-readable surface reports them without parsing prose.
type Problem struct {
	File     string `json:"file,omitempty"`
	Key      string `json:"key,omitempty"`
	SourceID string `json:"source_id,omitempty"`
	Message  string `json:"message"`
}

// Check is one question doctor asked and what it found.
type Check struct {
	Name     string    `json:"name"`
	Status   string    `json:"status"`
	Detail   string    `json:"detail,omitempty"`
	Problems []Problem `json:"problems,omitempty"`
}

// Diagnosis is the whole `recall doctor` answer.
type Diagnosis struct {
	Status  string  `json:"status"`
	Profile string  `json:"profile,omitempty"`
	Checks  []Check `json:"checks"`
	Failed  int     `json:"failed_checks"`
}

// Check names, in the order they are reported. Each later check depends on the
// earlier ones having passed, which is why a failed load skips the rest rather
// than reporting a health failure caused by configuration nobody could parse.
const (
	checkConfiguration = "configuration"
	checkTrust         = "trust_boundary"
	checkIdentity      = "identity"
	checkAccess        = "access"
	checkHealth        = "health"
	checkFreshness     = "freshness"
	checkLineage       = "lineage"
)

func runDoctor(ctx context.Context, env Env, args []string) int {
	fs := newFlagSet("doctor")
	var (
		profile = fs.String("profile", "", "profile to resolve")
		asJSON  = fs.Bool("json", false, "emit JSON")
	)
	if ok, code := parse(env, fs, doctorHelp, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageErr(env, doctorHelp, fmt.Errorf("doctor takes no arguments"))
	}

	d := diagnose(ctx, env, *profile)

	if *asJSON {
		if code := report(env, emitJSON(env.Stdout, d)); code != ExitOK {
			return code
		}
	} else {
		var o out
		renderDiagnosis(&o, d)
		if code := report(env, o.flush(env.Stdout)); code != ExitOK {
			return code
		}
	}
	if d.Failed > 0 {
		// Non-zero for each failing check. A machine asking "is this
		// installation sound" gets its answer from the status, not from prose.
		return ExitError
	}
	return ExitOK
}

// diagnose runs every check and never stops at the first failure: someone
// fixing an installation should see the whole list.
func diagnose(ctx context.Context, env Env, profileName string) Diagnosis {
	var d Diagnosis

	cfg, err := env.load()
	if err != nil {
		d.add(loadFailure(err)...)
		for _, name := range []string{checkAccess, checkHealth, checkFreshness, checkLineage} {
			d.add(Check{
				Name:   name,
				Status: CheckSkipped,
				Detail: "configuration did not load",
			})
		}
		return d.finish()
	}

	d.add(configurationCheck(cfg), trustCheck(cfg), identityCheck(cfg))

	rt, err := newRuntime(env, cfg, profileName, 0)
	if err != nil {
		// A ranker that will not build is a configured prior fusion would have
		// applied, so nothing below this can be checked honestly.
		d.add(Check{
			Name:     checkAccess,
			Status:   CheckFail,
			Problems: []Problem{{Message: err.Error()}},
		})
		for _, name := range []string{checkHealth, checkFreshness, checkLineage} {
			d.add(Check{Name: name, Status: CheckSkipped, Detail: "ranking configuration is invalid"})
		}
		return d.finish()
	}
	defer func() { _ = rt.close() }()
	d.Profile = rt.profile

	profile, err := cfg.ActiveProfile(profileName)
	if err != nil {
		d.add(Check{
			Name:     checkAccess,
			Status:   CheckFail,
			Problems: []Problem{{Message: err.Error()}},
		})
		for _, name := range []string{checkHealth, checkFreshness, checkLineage} {
			d.add(Check{Name: name, Status: CheckSkipped, Detail: "no active profile"})
		}
		return d.finish()
	}

	eligible := eligibleSources(cfg, profile)
	d.add(accessCheck(eligible))

	health, manifests := healthCheck(ctx, rt, eligible)
	d.add(health, freshnessCheck(cfg, eligible, manifests), lineageCheck(cfg, manifests))
	return d.finish()
}

func (d *Diagnosis) add(checks ...Check) { d.Checks = append(d.Checks, checks...) }

func (d Diagnosis) finish() Diagnosis {
	d.Status = "ok"
	for _, c := range d.Checks {
		if c.Status == CheckFail {
			d.Failed++
			d.Status = "failed"
		}
	}
	return d
}

// loadFailure turns a load error into the checks it belongs to.
//
// Configuration reports every problem it found rather than the first, and each
// one carries the file and key it was found at. Routing them by class is what
// makes "your project file declared a command" and "two sources claim one
// identity" separate answers instead of one wall of text.
func loadFailure(err error) []Check {
	byCheck := map[string][]Problem{}
	for _, leaf := range flatten(err) {
		name := checkConfiguration
		p := Problem{Message: leaf.Error()}

		var ce *config.Error
		if errors.As(leaf, &ce) {
			p = Problem{File: ce.File, Key: ce.Key, Message: ce.Msg}
		}
		switch {
		case errors.Is(leaf, config.ErrTrustBoundary):
			name = checkTrust
		case strings.Contains(p.Key, "source_uid"):
			name = checkIdentity
		}
		byCheck[name] = append(byCheck[name], p)
	}

	out := make([]Check, 0, 3)
	for _, name := range []string{checkConfiguration, checkTrust, checkIdentity} {
		problems := byCheck[name]
		if len(problems) == 0 {
			out = append(out, Check{
				Name:   name,
				Status: CheckSkipped,
				Detail: "configuration did not load",
			})
			continue
		}
		out = append(out, Check{Name: name, Status: CheckFail, Problems: problems})
	}
	return out
}

// flatten walks a joined error into its leaves, so one load reports every
// problem it found rather than whichever one happened to be joined first.
func flatten(err error) []error {
	if err == nil {
		return nil
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []error{err}
	}
	var out []error
	for _, e := range joined.Unwrap() {
		out = append(out, flatten(e)...)
	}
	return out
}

func configurationCheck(cfg *config.Config) Check {
	files := make([]string, 0, len(cfg.Files()))
	for _, f := range cfg.Files() {
		files = append(files, fmt.Sprintf("%s (%s)", f.Path, f.Layer))
	}
	detail := "no configuration file; built-in defaults are in force"
	if len(files) > 0 {
		detail = strings.Join(files, ", ")
	}
	return Check{Name: checkConfiguration, Status: CheckPass, Detail: detail}
}

// trustCheck reports the boundary that was enforced at load.
//
// It can only pass here: a project file that reached past the boundary fails
// the load, and [loadFailure] reports it. Saying so explicitly is the point —
// an operator has to be able to see which file was treated as untrusted.
func trustCheck(cfg *config.Config) Check {
	var project []string
	for _, f := range cfg.Files() {
		if f.Layer == config.LayerProject {
			project = append(project, f.Path)
		}
	}
	if len(project) == 0 {
		return Check{
			Name:   checkTrust,
			Status: CheckPass,
			Detail: "no project configuration; every adapter command came from user configuration",
		}
	}
	return Check{
		Name:   checkTrust,
		Status: CheckPass,
		Detail: fmt.Sprintf("project layer %s declared no adapter command, identity, or [defaults]",
			strings.Join(project, ", ")),
	}
}

// identityCheck enforces that both identities are one-to-one.
func identityCheck(cfg *config.Config) Check {
	var problems []Problem
	uids := map[recall.SourceUID]string{}
	ids := map[string]bool{}

	for _, s := range cfg.Sources {
		switch {
		case s.UID == "":
			problems = append(problems, Problem{
				SourceID: s.ID,
				Key:      "source_uid",
				Message:  "source has no immutable identity; every persisted locator and judgment keys on one",
			})
		case uids[s.UID] != "":
			problems = append(problems, Problem{
				SourceID: s.ID,
				Key:      "source_uid",
				Message: fmt.Sprintf("source_uid %q is claimed by both %q and %q; two sources sharing "+
					"an identity collapse into one lineage namespace", s.UID, uids[s.UID], s.ID),
			})
		default:
			uids[s.UID] = s.ID
		}
		if ids[s.ID] {
			problems = append(problems, Problem{
				SourceID: s.ID,
				Key:      "source_id",
				Message:  fmt.Sprintf("source_id %q is declared twice", s.ID),
			})
		}
		ids[s.ID] = true
	}
	return finishCheck(checkIdentity,
		fmt.Sprintf("%d sources, %d distinct identities", len(cfg.Sources), len(uids)), problems)
}

// eligibleSources are the profile members a query could actually reach. The
// rest are listed by `recall sources` and are not probed here: a disabled or
// denied source that cannot be reached is the configuration working as asked.
func eligibleSources(cfg *config.Config, profile config.Profile) []*config.SourceInstance {
	var out []*config.SourceInstance
	for _, id := range profile.SourceIDs {
		inst, ok := cfg.Source(id)
		if !ok || !inst.Enabled || !profile.Permits(*inst) {
			continue
		}
		out = append(out, inst)
	}
	return out
}

// accessCheck reads what it can without contacting an adapter: a location that
// names a path must be one that exists here. A configured source may simply be
// unavailable on another machine, and that is worth saying before a query
// reports it as an unhealthy source.
func accessCheck(sources []*config.SourceInstance) Check {
	var problems []Problem
	checked := 0
	for _, s := range sources {
		if s.Location == "" || strings.Contains(s.Location, "://") {
			continue
		}
		checked++
		if _, err := os.Stat(s.Location); err != nil {
			problems = append(problems, Problem{
				SourceID: s.ID,
				Key:      "location",
				Message:  err.Error(),
			})
		}
	}
	return finishCheck(checkAccess,
		fmt.Sprintf("%d of %d eligible sources name a local path", checked, len(sources)), problems)
}

// healthCheck contacts every eligible source. An unreachable source is a
// failure here so that it is a failure before a query, not a degraded answer
// during one.
func healthCheck(ctx context.Context, rt *runtime, sources []*config.SourceInstance) (Check, map[string]recall.Manifest) {
	var problems []Problem
	manifests := make(map[string]recall.Manifest, len(sources))

	for _, s := range sources {
		manifest, health, err := rt.probe(ctx, s)
		switch {
		case err != nil:
			problems = append(problems, Problem{
				SourceID: s.ID,
				Message:  err.Error(),
			})
			continue
		case !health.Usable():
			problems = append(problems, Problem{
				SourceID: s.ID,
				Message:  fmt.Sprintf("health is %s: %s", health.Status, diagnostics(health.Diagnostics)),
			})
			continue
		}
		manifests[s.ID] = manifest
	}
	return finishCheck(checkHealth,
		fmt.Sprintf("%d of %d eligible sources answered a health probe", len(manifests), len(sources)),
		problems), manifests
}

// freshnessCheck compares what a source asks for against what its adapter says
// it can serve, in the registration and in the manifest. The registration is
// what configuration validated against before any process existed, so a
// manifest that contradicts it means one of the two is lying about the source.
func freshnessCheck(cfg *config.Config, sources []*config.SourceInstance, manifests map[string]recall.Manifest) Check {
	var problems []Problem
	for _, s := range sources {
		manifest, probed := manifests[s.ID]
		if !probed {
			continue
		}
		if !manifest.Supports(s.FreshnessMode) {
			problems = append(problems, Problem{
				SourceID: s.ID,
				Key:      "freshness_mode",
				Message: fmt.Sprintf("source asks for %q; adapter %q serves %v",
					s.FreshnessMode, s.Adapter, strv(manifest.FreshnessModes)),
			})
		}
		def, ok := cfg.Adapter(s.Adapter)
		if !ok {
			continue
		}
		for _, mode := range def.FreshnessModes {
			if !manifest.Supports(mode) {
				problems = append(problems, Problem{
					SourceID: s.ID,
					Key:      "adapters." + s.Adapter + ".freshness_modes",
					Message: fmt.Sprintf("registration declares %q, which the manifest does not; "+
						"configuration was validated against a claim the adapter does not make", mode),
				})
			}
		}
	}
	return finishCheck(checkFreshness,
		fmt.Sprintf("%d probed sources serve the freshness mode they were configured with", len(manifests)),
		problems)
}

// lineageCheck follows the declared source-level derivation graph.
//
// Only the declared edges can be checked without running a query: a
// record-level derived_from cycle lives in candidates, so internal/lineage
// bounds the walk and reports it per request. What is configuration — a source
// whose manifest says it projects another — is checkable here, and a cycle in
// it is a defect no query would recover from.
func lineageCheck(cfg *config.Config, manifests map[string]recall.Manifest) Check {
	edges := map[string]string{}
	var problems []Problem
	var unresolved []string

	for _, id := range slices.Sorted(maps.Keys(manifests)) {
		upstream := manifests[id].DerivesFrom
		if upstream == "" {
			continue
		}
		if _, ok := cfg.Source(upstream); !ok {
			// An edge naming a source this machine does not have is dropped and
			// reported, never fatal: the locator is still portable elsewhere.
			unresolved = append(unresolved, id+" -> "+upstream)
			continue
		}
		edges[id] = upstream
	}

	for _, id := range slices.Sorted(maps.Keys(edges)) {
		if cycle := walk(edges, id); len(cycle) > 0 {
			problems = append(problems, Problem{
				SourceID: id,
				Key:      "derives_from",
				Message:  "declared derivation cycle: " + strings.Join(cycle, " -> "),
			})
		}
	}

	detail := fmt.Sprintf("%d declared source-level derivation edges", len(edges))
	if len(unresolved) > 0 {
		detail += "; dropped, upstream not configured here: " + strings.Join(unresolved, ", ")
	}
	return finishCheck(checkLineage, detail, problems)
}

// walk follows edges from one source and returns the cycle it closed, if any.
func walk(edges map[string]string, start string) []string {
	seen := map[string]bool{start: true}
	path := []string{start}
	for at := start; ; {
		next, ok := edges[at]
		if !ok {
			return nil
		}
		path = append(path, next)
		if seen[next] {
			return path
		}
		seen[next] = true
		at = next
	}
}

func finishCheck(name, detail string, problems []Problem) Check {
	if len(problems) > 0 {
		return Check{Name: name, Status: CheckFail, Detail: detail, Problems: problems}
	}
	return Check{Name: name, Status: CheckPass, Detail: detail}
}

func renderDiagnosis(o *out, d Diagnosis) {
	var head fields
	head.text("status", d.Status)
	head.text("profile", d.Profile)
	head.count("failed checks", d.Failed)
	o.line(head.String())

	for _, c := range d.Checks {
		o.blank()
		o.printf("%-14s %s\n", c.Name, c.Status)
		if c.Detail != "" {
			o.block("  ", c.Detail)
		}
		for _, p := range c.Problems {
			var f fields
			f.text("source", p.SourceID)
			f.text("file", p.File)
			f.text("key", p.Key)
			if f.empty() {
				o.block("  ", "- "+p.Message)
				continue
			}
			o.block("  ", "- "+f.String())
			o.block("    ", p.Message)
		}
	}
}
