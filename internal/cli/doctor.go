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
	"github.com/marcus/recall/internal/conformance"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

const doctorHelp = `usage: recall doctor [flags]

Check everything that has to be true before a query means anything:

  configuration    the layers load, merge, and validate
  trust_boundary   the project layer stayed inside what a cloned repository
                   may declare
  identity         every source has a source_uid, and no two share one
  access           every eligible filesystem location can be read
  health           every eligible source can be reached and is usable
  serving          every source that answered is serving its whole corpus,
                   rather than a stale or partial one
  store_isolation  no two instances of one adapter opened the same store
  freshness        each source's freshness mode is one its adapter serves
  lineage          declared source-level derivation has no cycle

A check that fails means the installation is misconfigured and exits 1. A check
that degrades means it is configured correctly and is not serving what it was
configured to serve — a stale index, a partial corpus — and exits 3. The two
have different remedies, so they are not the same answer.

With --conformance, ask a different question entirely: replay one adapter's
recorded transcripts against its command and diff every response. The suite is
the adapter registration's ` + "`conformance`" + ` directory; see
docs/adapter-protocol.md for its layout.

flags:
  --profile NAME      profile to resolve; default is the configured one
  --conformance NAME  replay a registered adapter's recorded transcripts
  --json              emit the diagnosis as JSON
  --server URL        dispatch to a running recall serve instance
  --auth-token-env ENV
                      read the server bearer token from ENV

` + exitCodes

// Check outcomes.
//
// CheckDegraded is deliberately neither pass nor fail. The two questions
// "is this installation configured correctly" and "is it serving what it was
// configured to serve" have different answers and different remedies — the
// first is fixed by editing a file, the second by rebuilding an index or
// waking a machine — and collapsing them is what let a green doctor mean
// nothing. It carries its own exit code so a script can still tell them apart.
const (
	CheckPass     = "pass"
	CheckFail     = "fail"
	CheckDegraded = "degraded"
	CheckSkipped  = "skipped"
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
	Status   string  `json:"status"`
	Profile  string  `json:"profile,omitempty"`
	Checks   []Check `json:"checks"`
	Failed   int     `json:"failed_checks"`
	Degraded int     `json:"degraded_checks"`
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
	checkServing       = "serving"
	checkIsolation     = "store_isolation"
	checkFreshness     = "freshness"
	checkLineage       = "lineage"
	checkConformance   = "conformance"
)

func runDoctor(ctx context.Context, env Env, args []string) int {
	fs := newFlagSet("doctor")
	var (
		profile    = fs.String("profile", "", "profile to resolve")
		adapterFor = fs.String("conformance", "", "replay a registered adapter's recorded transcripts")
		asJSON     = fs.Bool("json", false, "emit JSON")
	)
	remote := addRemoteFlags(fs)
	if ok, code := parse(env, fs, doctorHelp, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageErr(env, doctorHelp, fmt.Errorf("doctor takes no arguments"))
	}

	// --conformance asks a different question, so it gets a different
	// diagnosis rather than an extra check appended to this one. Replaying
	// recorded transcripts is about an adapter implementation; whether this
	// machine's sources happen to be reachable has nothing to do with it, and
	// letting a sleeping laptop fail a conformance run would make the answer
	// mean something it does not.
	var d Diagnosis
	switch {
	case *adapterFor != "":
		if *remote.server != "" {
			return usageErr(env, doctorHelp, errors.New("--conformance cannot be combined with --server"))
		}
		d = diagnoseConformance(ctx, env, *adapterFor)
	case *remote.server == "" && env.Core == nil:
		// Doctor must turn configuration-load failures into a structured
		// diagnosis. openCore cannot do that because an invalid configuration
		// has no core to return.
		if *remote.tokenEnvName != "" {
			return usageErr(env, doctorHelp, errors.New("--auth-token-env requires --server"))
		}
		d = diagnose(ctx, env, *profile)
	default:
		core, closeCore, err := openCore(env, *profile, 0, remote)
		if err != nil {
			fail(env, err)
			return ExitError
		}
		defer func() { _ = closeCore() }()
		listing, err := core.Doctor(ctx)
		if err != nil {
			fail(env, err)
			return ExitError
		}
		if err := listingInto(listing.Payload, &d); err != nil {
			fail(env, err)
			return ExitError
		}
	}

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
	switch {
	case d.Failed > 0:
		// Non-zero for each failing check. A machine asking "is this
		// installation sound" gets its answer from the status, not from prose.
		return ExitError
	case d.Degraded > 0:
		// Configured correctly, not serving what it was configured to serve.
		// It is the same distinction ExitDegraded already draws for a query —
		// the sources are right and one of them could not answer properly —
		// so it gets the same code rather than a fourth meaning for exit 1.
		return ExitDegraded
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

	rt, err := newRuntime(env, cfg, profileName, 0)
	if err != nil {
		d.add(configurationCheck(cfg), trustCheck(cfg), identityCheck(cfg))
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
	return diagnoseRuntime(ctx, cfg, rt)
}

// diagnoseRuntime runs the live checks against an already assembled runtime.
// recall serve uses it so a doctor request observes the same long-lived
// adapters and indexes as query, instead of creating a second pool beside the
// one the server exists to amortize.
func diagnoseRuntime(ctx context.Context, cfg *config.Config, rt *runtime) Diagnosis {
	var d Diagnosis
	d.add(configurationCheck(cfg), trustCheck(cfg), identityCheck(cfg))
	d.Profile = rt.profile

	profile, err := cfg.ActiveProfile(rt.profile)
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

	health, manifests, healths := healthCheck(ctx, rt, eligible)
	d.add(health, servingCheck(eligible, manifests, healths), isolationCheck(eligible, healths),
		freshnessCheck(cfg, eligible, manifests), lineageCheck(cfg, manifests))
	return d.finish()
}

// diagnoseConformance replays one registered adapter's recorded transcripts.
//
// It runs the three checks that have to hold for the question to be answerable
// at all — the configuration loads, the project layer stayed inside the trust
// boundary, and identities are one to one — and then the replay. The
// source-probing checks are skipped and say so: a conformance run is a
// statement about an adapter binary, and one that failed because a laptop was
// asleep would be a statement about nothing.
func diagnoseConformance(ctx context.Context, env Env, name string) Diagnosis {
	var d Diagnosis

	cfg, err := env.load()
	if err != nil {
		d.add(loadFailure(err)...)
		d.add(Check{
			Name:   checkConformance,
			Status: CheckSkipped,
			Detail: "configuration did not load",
		})
		return d.finish()
	}
	d.add(configurationCheck(cfg), trustCheck(cfg), identityCheck(cfg))
	d.add(conformanceCheck(ctx, cfg, name))
	return d.finish()
}

// conformanceCheck drives the suite and reports one problem per failing case.
//
// A case that differs is a problem, not an abort: someone fixing an adapter
// should see every case that moved, in one pass, the same way every other
// doctor check reports.
func conformanceCheck(ctx context.Context, cfg *config.Config, name string) Check {
	def, ok := cfg.Adapter(name)
	switch {
	case !ok:
		return Check{Name: checkConformance, Status: CheckFail, Problems: []Problem{{
			Key:     "adapters." + name,
			Message: "no adapter by that name is registered",
		}}}
	case def.Builtin:
		return Check{Name: checkConformance, Status: CheckFail, Problems: []Problem{{
			Key: "adapters." + name,
			Message: "adapter is built in, so there is no process to replay a transcript against; " +
				"its conformance suite runs in the Go test suite instead",
		}}}
	case def.Conformance == "":
		// Not a pass. An adapter with no recorded transcripts has not been
		// shown to honor the contract, and reporting that as "checked" is the
		// one answer a conformance run must never give for having checked
		// nothing.
		return Check{Name: checkConformance, Status: CheckFail, Problems: []Problem{{
			Key: "adapters." + name + ".conformance",
			Message: "adapter declares no conformance directory, so it ships no recorded " +
				"transcripts to replay",
		}}}
	}

	results, err := conformance.Verify(ctx, def.Conformance,
		conformance.Command(def.Command, def.Args...), conformance.Options{})
	if err != nil {
		// The suite itself could not be run: an unreadable directory, a
		// malformed manifest, a binary that will not start. That is a failure
		// of the check, not a verdict about the adapter.
		return Check{Name: checkConformance, Status: CheckFail, Problems: []Problem{{
			Key:     "adapters." + name + ".conformance",
			Message: err.Error(),
		}}}
	}

	var problems []Problem
	passed := 0
	for _, res := range results {
		if res.OK() {
			passed++
			continue
		}
		problems = append(problems, Problem{
			Key:     "adapters." + name + ".conformance." + res.Case,
			Message: res.Report(),
		})
	}
	return finishCheck(checkConformance,
		fmt.Sprintf("%s: %d of %d recorded cases replayed as recorded",
			def.Command, passed, len(results)), problems)
}

func (d *Diagnosis) add(checks ...Check) { d.Checks = append(d.Checks, checks...) }

func (d Diagnosis) finish() Diagnosis {
	for _, c := range d.Checks {
		switch c.Status {
		case CheckFail:
			d.Failed++
		case CheckDegraded:
			d.Degraded++
		}
	}
	switch {
	case d.Failed > 0:
		// A broken configuration outranks a degraded one: there is no point
		// telling someone their index is stale when the file naming it does
		// not load.
		d.Status = "failed"
	case d.Degraded > 0:
		d.Status = "degraded"
	default:
		d.Status = "ok"
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
		if s.LocationKind != config.LocationPath {
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
//
// The healths it returns are what every later check reads. They are kept
// separate from the manifests because a source can answer a probe and still
// report something a query would suffer from — a stale generation, a partial
// index, two instances over one store — and those are questions about the
// deployment rather than about the adapter.
func healthCheck(ctx context.Context, rt *runtime, sources []*config.SourceInstance) (
	Check, map[string]recall.Manifest, map[string]recall.Health,
) {
	var problems []Problem
	manifests := make(map[string]recall.Manifest, len(sources))
	healths := make(map[string]recall.Health, len(sources))

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
		healths[s.ID] = health
	}
	return finishCheck(checkHealth,
		fmt.Sprintf("%d of %d eligible sources answered a health probe", len(manifests), len(sources)),
		problems), manifests, healths
}

// servingCheck asks what the health check does not: is each source serving
// what it was configured to serve.
//
// [healthCheck] tests [recall.Health.Usable], which is liveness — did the
// source answer. A source can answer from a stale, partial index and still
// count as a pass, and it did: `recall doctor` reported "9 of 9 eligible
// sources answered a health probe" and exited 0 while the highest-prior source
// on the machine was degraded, coverage partial, and serving a generation
// missing two of its nine documents. You had to run `recall sources` to find
// that out, and in the meantime a green doctor was used as evidence that a
// configuration was sound.
//
// So this reports every source that answered but is not whole. It does not
// FAIL: nothing here is misconfigured, and a laptop whose index is a rebuild
// behind should not read the same as a project file that tried to run a
// command. It degrades, which carries its own exit code.
//
// The base prior travels with each line because it is what makes the finding
// actionable. A stale source at prior 0.9 is a nuisance; the same staleness at
// 1.5 is silently shaping every answer on the machine, and the two should not
// look alike in a report someone skims.
func servingCheck(
	sources []*config.SourceInstance,
	manifests map[string]recall.Manifest,
	healths map[string]recall.Health,
) Check {
	var problems []Problem
	whole := 0

	for _, s := range sources {
		h, probed := healths[s.ID]
		if !probed {
			// Unreachable, and already reported as a health failure. Saying it
			// twice would make one broken source look like two problems.
			continue
		}
		var found []string
		if h.Status != recall.HealthHealthy {
			found = append(found, "health "+string(h.Status))
		}
		if h.Coverage != recall.IndexComplete {
			found = append(found, "coverage "+string(h.Coverage))
		}
		if h.RecordCount > 0 && h.IndexedCount > 0 && h.IndexedCount < h.RecordCount {
			// The index represents less than the source holds, which is the
			// exact shape of the miss: a search over it returns fewer results
			// and reports nothing about why.
			found = append(found, fmt.Sprintf("%d of %d records indexed", h.IndexedCount, h.RecordCount))
		}
		if h.FailedCount > 0 {
			found = append(found, fmt.Sprintf("%d records rejected", h.FailedCount))
		}
		if len(found) == 0 {
			whole++
			continue
		}
		message := fmt.Sprintf("%s (base_prior %g); it answers, so a query will use it, and the answer "+
			"will be drawn from less than this source holds",
			strings.Join(found, ", "), s.BasePrior)
		if manifests[s.ID].Can(recall.CapCheckpoint) {
			message += fmt.Sprintf("; run recall refresh --source %s", s.ID)
		}
		problems = append(problems, Problem{
			SourceID: s.ID,
			Message:  message,
		})
	}

	detail := fmt.Sprintf("%d of %d probed sources are serving their whole corpus", whole, len(healths))
	if len(problems) > 0 {
		return Check{Name: checkServing, Status: CheckDegraded, Detail: detail, Problems: problems}
	}
	return Check{Name: checkServing, Status: CheckPass, Detail: detail}
}

// isolationCheck refuses a profile in which two enabled instances of one
// adapter are reading the same store.
//
// This is the check that would have caught three separate defects in this
// system, each found only after it had been shipping: two document sources
// over overlapping roots, two catalog instances over one server, and two td
// sources whose locations resolved to one database. All three have the same
// shape. Lineage groups on source_uid plus source_record_id, so one record
// reaching the core through two instances arrives as two independent pieces of
// evidence, and the corroboration bonus then promotes it for agreeing with
// itself. Nothing downstream can see the duplication, because by the time the
// core has the candidates the two instances are two legitimate sources.
//
// It reads [protocol.DiagStoreIdentity], which an adapter sets to name the
// store it actually OPENED — not the one configuration asked for, which would
// compare equal exactly when the configuration is already consistent.
// Comparison is within one adapter only: two adapters are free to describe
// their stores in the same words, and nothing about a shared string across
// adapters means a shared store.
//
// An adapter that leaves the key unset makes no exclusivity claim and is not
// checked. That is deliberate rather than a gap: for the ongoing catalog two
// instances over one server is the intended configuration — "everything" and
// "the things that need attention" are different questions — and its
// candidates carry a content fingerprint that collapses them. Silence there is
// the adapter saying the overlap is by design.
func isolationCheck(sources []*config.SourceInstance, healths map[string]recall.Health) Check {
	type claim struct{ adapter, identity string }
	claimed := map[claim][]string{}
	var order []claim
	naming := 0

	for _, s := range sources {
		identity, ok := healths[s.ID].Diagnostics[protocol.DiagStoreIdentity].(string)
		if !ok || identity == "" {
			continue
		}
		naming++
		c := claim{adapter: s.Adapter, identity: identity}
		if len(claimed[c]) == 0 {
			order = append(order, c)
		}
		claimed[c] = append(claimed[c], s.ID)
	}

	var problems []Problem
	for _, c := range order {
		ids := claimed[c]
		if len(ids) < 2 {
			continue
		}
		problems = append(problems, Problem{
			SourceID: ids[0],
			Key:      "location",
			Message: fmt.Sprintf(
				"%s all opened the same %s store %s; one record reaching the core through two "+
					"sources is counted as two independent pieces of evidence and scored up for "+
					"corroborating itself. Configure one instance, or point them at separate stores",
				strings.Join(ids, ", "), c.adapter, c.identity),
		})
	}
	detail := fmt.Sprintf("%d of %d probed sources named the store they opened, %s",
		naming, len(healths), plural(len(order), "distinct store"))
	if naming == 0 {
		// Said plainly rather than reported as a clean pass over nothing: no
		// adapter in this profile claims a store exclusively, so this check
		// looked at nothing and must not read as having cleared anything.
		detail = "no probed source claims a store exclusively; nothing to compare"
	}
	return finishCheck(checkIsolation, detail, problems)
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

// plural renders a count with its noun, so a detail line does not read "1
// distinct stores" at an operator who is already looking for something wrong.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
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
	head.count("degraded checks", d.Degraded)
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
