package cli

import (
	"context"
	"fmt"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/pkg/recall"
)

const sourcesHelp = `usage: recall sources [flags]

List the configured source instances with their capabilities, health, and
freshness evidence. Sources in the active profile are probed; a disabled source
is listed and not contacted.

flags:
  --profile NAME    profile to resolve; default is the configured one
  --json            emit the listing as JSON
  --server URL      dispatch to a running recall serve instance
  --auth-token-env ENV
                    read the server bearer token from ENV

` + exitCodes

// SourceStatus is one source instance as an operator sees it: what it is, what
// it can do, and how it is right now.
//
// It exists because the application layer exposes no listing of its own — it
// answers queries and expansions. The fields are read straight off resolved
// configuration and the adapter's own manifest and health report; nothing here
// decides anything.
type SourceStatus struct {
	SourceUID recall.SourceUID `json:"source_uid"`
	SourceID  string           `json:"source_id"`
	Adapter   string           `json:"adapter"`
	Enabled   bool             `json:"enabled"`
	InProfile bool             `json:"in_profile"`

	Location        string               `json:"location,omitempty"`
	Sensitivity     string               `json:"sensitivity"`
	Permitted       bool                 `json:"permitted"`
	FreshnessMode   recall.FreshnessMode `json:"freshness_mode,omitempty"`
	FreshnessPolicy string               `json:"freshness_policy,omitempty"`
	BasePrior       float64              `json:"base_prior"`
	IntentPriors    map[string]float64   `json:"intent_priors,omitempty"`
	RecordTypes     []recall.RecordType  `json:"record_types,omitempty"`
	TimeoutMS       int64                `json:"timeout_ms"`

	// Probed says whether the adapter was contacted. A disabled source is not,
	// so its empty health is the absence of a probe and not a bad report.
	Probed bool `json:"probed"`

	AdapterID    string              `json:"adapter_id,omitempty"`
	DisplayName  string              `json:"display_name,omitempty"`
	Capabilities []recall.Capability `json:"capabilities,omitempty"`
	QueryModes   []recall.QueryMode  `json:"query_modes,omitempty"`
	AsOfSupport  recall.AsOfSupport  `json:"as_of_support,omitempty"`
	DerivesFrom  string              `json:"derives_from,omitempty"`
	Health       *recall.Health      `json:"health,omitempty"`

	// Error is why a probe failed, in the adapter's own words. It is shown
	// because a source nobody can see the failure of is a source nobody can fix.
	Error string `json:"error,omitempty"`
}

// SourceListing is the whole `recall sources` answer.
type SourceListing struct {
	Profile        string         `json:"profile"`
	MaxSensitivity string         `json:"max_sensitivity"`
	Sources        []SourceStatus `json:"sources"`
}

func runSources(ctx context.Context, env Env, args []string) int {
	fs := newFlagSet("sources")
	var (
		profile = fs.String("profile", "", "profile to resolve")
		asJSON  = fs.Bool("json", false, "emit JSON")
	)
	remote := addRemoteFlags(fs)
	if ok, code := parse(env, fs, sourcesHelp, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageErr(env, sourcesHelp, fmt.Errorf("sources takes no arguments"))
	}

	core, closeCore, err := openCore(env, *profile, 0, remote)
	if err != nil {
		fail(env, err)
		return ExitError
	}
	defer func() { _ = closeCore() }()

	raw, err := core.Sources(ctx)
	if err != nil {
		fail(env, err)
		return ExitError
	}
	var listing SourceListing
	if err := listingInto(raw.Payload, &listing); err != nil {
		fail(env, err)
		return ExitError
	}

	if *asJSON {
		if code := report(env, emitJSON(env.Stdout, listing)); code != ExitOK {
			return code
		}
	} else {
		var o out
		renderSources(&o, listing)
		if code := report(env, o.flush(env.Stdout)); code != ExitOK {
			return code
		}
	}
	return sourcesExit(listing)
}

// sourcesExit reports degraded when a source that would be eligible cannot
// answer, so a health check in a script does not have to parse the output.
func sourcesExit(listing SourceListing) int {
	for _, s := range listing.Sources {
		if !s.Probed {
			continue
		}
		if s.Error != "" || s.Health == nil || !s.Health.Usable() {
			return ExitDegraded
		}
	}
	return ExitOK
}

func (r *runtime) listSources(ctx context.Context) (SourceListing, error) {
	profile, err := r.cfg.ActiveProfile(r.profile)
	if err != nil {
		return SourceListing{}, err
	}
	members := map[string]bool{}
	for _, id := range profile.SourceIDs {
		members[id] = true
	}

	listing := SourceListing{
		Profile:        profile.Name,
		MaxSensitivity: profile.MaxSensitivity.String(),
	}
	for _, inst := range r.cfg.Sources {
		listing.Sources = append(listing.Sources, r.status(ctx, profile, members[inst.ID], inst))
	}
	return listing, nil
}

func (r *runtime) status(ctx context.Context, profile config.Profile, member bool, inst *config.SourceInstance) SourceStatus {
	s := SourceStatus{
		SourceUID:       inst.UID,
		SourceID:        inst.ID,
		Adapter:         inst.Adapter,
		Enabled:         inst.Enabled,
		InProfile:       member,
		Location:        inst.Location,
		Sensitivity:     inst.Sensitivity.String(),
		Permitted:       profile.Permits(*inst),
		FreshnessMode:   inst.FreshnessMode,
		FreshnessPolicy: inst.FreshnessPolicy,
		BasePrior:       inst.BasePrior,
		IntentPriors:    inst.IntentPriors,
		RecordTypes:     inst.RecordTypes,
		TimeoutMS:       inst.Timeout.Milliseconds(),
	}
	// A disabled source, or one the profile denies, is listed and not
	// contacted: probing it would send traffic the configuration said not to.
	if !inst.Enabled || !member || !s.Permitted {
		return s
	}

	s.Probed = true
	manifest, health, err := r.probe(ctx, inst)
	s.AdapterID = manifest.AdapterID
	s.DisplayName = manifest.DisplayName
	s.Capabilities = manifest.Capabilities
	s.QueryModes = manifest.QueryModes
	s.AsOfSupport = manifest.AsOfSupport
	s.DerivesFrom = manifest.DerivesFrom
	if s.FreshnessPolicy == "" {
		s.FreshnessPolicy = manifest.FreshnessPolicy
	}
	s.Health = &health
	if err != nil {
		s.Error = err.Error()
	}
	return s
}

func renderSources(o *out, listing SourceListing) {
	var head fields
	head.text("profile", listing.Profile)
	head.text("max sensitivity", listing.MaxSensitivity)
	head.count("sources", len(listing.Sources))
	o.line(head.String())

	for _, s := range listing.Sources {
		o.blank()
		var f fields
		f.text("adapter", s.Adapter)
		f.raw(enabled(s.Enabled))
		f.flag("in profile", s.InProfile)
		f.flag("denied by ceiling", !s.Permitted)
		o.printf("%s (%s)  %s\n", s.SourceID, s.SourceUID, f.String())

		var cfgf fields
		cfgf.text("location", s.Location)
		cfgf.text("sensitivity", s.Sensitivity)
		cfgf.text("freshness", string(s.FreshnessMode))
		cfgf.number("prior", s.BasePrior)
		cfgf.text("intent priors", priors(s.IntentPriors))
		cfgf.text("record types", join(s.RecordTypes))
		cfgf.count64("timeout_ms", s.TimeoutMS)
		o.block("  ", cfgf.String())

		var capf fields
		capf.text("adapter id", s.AdapterID)
		capf.text("name", s.DisplayName)
		capf.text("capabilities", join(s.Capabilities))
		capf.text("query modes", join(s.QueryModes))
		capf.text("as_of", string(s.AsOfSupport))
		capf.text("derives from", s.DerivesFrom)
		if !capf.empty() {
			o.block("  ", capf.String())
		}
		if s.FreshnessPolicy != "" {
			o.block("  ", "policy "+s.FreshnessPolicy)
		}
		if !s.Probed {
			o.block("  ", "health not probed")
		}
		if s.Health != nil {
			o.block("  ", "health "+healthLine(*s.Health))
		}
		if s.Error != "" {
			o.block("  ", "error "+s.Error)
		}
	}
}

// healthLine renders an adapter's self-report. A recent index timestamp alone
// is not health, so the watermarks and counts travel with the status.
func healthLine(h recall.Health) string {
	var f fields
	f.raw(string(h.Status))
	f.text("coverage", string(h.Coverage))
	if !h.CheckedAt.IsZero() {
		f.text("checked", stamp(h.CheckedAt))
	}
	f.at("last success", h.LastSuccess)
	f.text("watermark", h.SourceWatermark)
	f.text("index watermark", h.IndexWatermark)
	f.text("generation", h.IndexGeneration)
	f.text("model", h.IndexModel)
	f.text("index config", h.IndexConfig)
	f.count64("records", h.RecordCount)
	f.count64("indexed", h.IndexedCount)
	f.count64("failed", h.FailedCount)
	f.dur("cold start", h.ColdStart)
	f.text("diagnostics", diagnostics(h.Diagnostics))
	return f.String()
}

func priors(in map[string]float64) string {
	if len(in) == 0 {
		return ""
	}
	as := make(map[string]any, len(in))
	for k, v := range in {
		as[k] = num(v)
	}
	return diagnostics(as)
}

func enabled(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}
