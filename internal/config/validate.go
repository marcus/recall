package config

import (
	"maps"
	"math"
	"path/filepath"
	"slices"
	"strings"

	"github.com/marcus/recall/pkg/recall"
)

// validate checks everything that can be decided without contacting a source.
//
// It reports every problem it finds rather than the first: a person fixing
// configuration should see the whole list, and `recall doctor` renders exactly
// this. Nothing here is clamped or repaired. A prior outside its range is a
// mistake with a visible effect on ranking, and silently moving it into range
// would hide that the configuration and the results disagree.
func (c *Config) validate() error {
	var probs problems

	probs.add(c.validateDefaults())
	probs.add(c.validateEvaluation())
	for _, name := range slices.Sorted(maps.Keys(c.Adapters)) {
		probs.add(c.validateAdapter(c.Adapters[name]))
	}

	uids := map[recall.SourceUID]*SourceInstance{}
	ids := map[string]*SourceInstance{}
	for _, s := range c.Sources {
		probs.add(c.validateSource(s))
		probs.add(checkUnique(s, uids, ids))
	}

	for _, name := range c.ProfileNames() {
		probs.add(c.validateProfile(c.Profiles[name]))
	}
	return probs.err()
}

func (c *Config) validateEvaluation() error {
	var probs problems
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "development_pack", value: c.Evaluation.DevelopmentPack},
		{name: "development_baseline", value: c.Evaluation.DevelopmentBaseline},
	} {
		name, value := field.name, field.value
		if value == "" {
			continue
		}
		if !filepath.IsAbs(value) {
			probs.add(invalidErrorf(c.evaluationOrigins[name].File, "evaluation."+name,
				"must be an absolute path; private evaluation artifacts must not depend "+
					"on the directory Recall was started in"))
		}
	}
	return probs.err()
}

// checkUnique enforces that both identities are one-to-one.
//
// Duplicate source_uid is the more serious of the two: two sources sharing an
// identity would collapse into one lineage namespace, so a saved locator would
// expand against whichever of them happened to answer.
func checkUnique(s *SourceInstance, uids map[recall.SourceUID]*SourceInstance, ids map[string]*SourceInstance) error {
	var probs problems
	if prev, dup := ids[s.ID]; dup {
		probs.add(invalidErrorf(s.declaredIn.File, s.fieldKey("source_id"),
			"source_id %q is declared twice, by %s and %s", s.ID, prev.declaredIn, s.declaredIn))
	}
	ids[s.ID] = s

	if s.UID != "" {
		if prev, dup := uids[s.UID]; dup {
			probs.add(invalidErrorf(s.declaredIn.File, s.fieldKey("source_uid"),
				"source_uid %q is claimed by both %q (%s) and %q (%s); an immutable identity "+
					"belongs to exactly one source",
				s.UID, prev.ID, prev.declaredIn, s.ID, s.declaredIn))
		}
		uids[s.UID] = s
	}
	return probs.err()
}

func (c *Config) validateDefaults() error {
	var probs problems
	file := c.defaultOrigins["timeout_ms"].File
	if c.Defaults.Timeout <= 0 {
		probs.add(invalidErrorf(file, "defaults.timeout_ms",
			"must be positive; a source with no time to answer is a source that reports "+
				"timeout, never empty success"))
	}
	// Zero means "unset": the engine keeps its built-in request budget. A
	// negative value is never a ceiling, only a configuration mistake.
	if c.Defaults.Budget < 0 {
		probs.add(invalidErrorf(c.defaultOrigins["budget_ms"].File,
			"defaults.budget_ms", "must not be negative; 0 leaves the engine fallback"))
	}
	if c.Defaults.FusionReserve < 0 {
		probs.add(invalidErrorf(c.defaultOrigins["fusion_reserve_ms"].File,
			"defaults.fusion_reserve_ms", "must not be negative"))
	}
	if c.Defaults.MaxResults < 0 {
		probs.add(invalidErrorf(c.defaultOrigins["max_results"].File,
			"defaults.max_results", "must not be negative; 0 is unbounded"))
	}
	// The same bound internal/ranking enforces, stated here so a machine is told
	// about it by `recall doctor` rather than by a query that would not fuse.
	// A floor of 1 is refused rather than accepted as "keep nothing": on the
	// shared definition, relevance is exactly 1 for a browse with no query terms
	// and for a record whose source could not report a length, so that floor
	// keeps precisely the candidates that told fusion nothing.
	floor := c.Defaults.RelevanceFloor
	if math.IsNaN(floor) || floor < 0 || floor >= 1 {
		probs.add(invalidErrorf(c.defaultOrigins["relevance_floor"].File,
			"defaults.relevance_floor", "must be in [0, 1); 0 admits every candidate"))
	}
	if err := validateName("profile", c.Defaults.Profile); err != nil {
		probs.add(invalidErrorf(c.defaultOrigins["profile"].File, "defaults.profile", "%s", err))
	}
	return probs.err()
}

func (c *Config) validateAdapter(a AdapterDefinition) error {
	var probs problems
	key := "adapters." + a.Name
	if err := validateName("adapter", a.Name); err != nil {
		probs.add(invalidErrorf(a.Origin.File, key, "%s", err))
	}
	if len(a.FreshnessModes) == 0 {
		probs.add(invalidErrorf(a.Origin.File, key+".freshness_modes",
			"an adapter must declare the freshness modes it can serve, so a source naming an "+
				"unsupported one fails at load rather than at query time"))
	}
	if !a.Builtin && a.Command == "" {
		probs.add(invalidErrorf(a.Origin.File, key+".command",
			"an external adapter must declare a command"))
	}
	if a.Builtin && a.Command != "" {
		probs.add(invalidErrorf(a.Origin.File, key+".command",
			"adapter %q is built in and runs no command", a.Name))
	}
	if a.Command != "" && !isSafeCommand(a.Command) {
		probs.add(invalidErrorf(a.Origin.File, key+".command",
			"command %q must be an absolute path or a bare name resolved on PATH; a relative "+
				"path would resolve against whatever directory Recall was started in", a.Command))
	}
	if a.Builtin && a.Conformance != "" {
		probs.add(invalidErrorf(a.Origin.File, key+".conformance",
			"adapter %q is built in and runs no process for a transcript to be replayed against",
			a.Name))
	}
	if a.Conformance != "" && !strings.HasPrefix(a.Conformance, "/") {
		probs.add(invalidErrorf(a.Origin.File, key+".conformance",
			"conformance %q must be an absolute path; a relative one would resolve against "+
				"whatever directory Recall was started in, and a run that checked the wrong "+
				"suite is worse than one that checked nothing", a.Conformance))
	}
	for _, name := range slices.Sorted(maps.Keys(a.Secrets)) {
		probs.add(validateSecret(a.Origin.File, key+".secrets."+name, a.Secrets[name]))
	}
	return probs.err()
}

// isSafeCommand accepts an absolute path or a bare executable name.
func isSafeCommand(cmd string) bool {
	if strings.HasPrefix(cmd, "/") {
		return true
	}
	return !strings.ContainsAny(cmd, `/\`)
}

func (c *Config) validateSource(s *SourceInstance) error {
	var p problems
	// A field nobody declared has no file of its own, so the problem is
	// reported against the file that declared the source it belongs to.
	origin := func(field string) string {
		if file := s.Origin(field).File; file != "" {
			return file
		}
		return s.declaredIn.File
	}
	key := s.fieldKey

	if err := validateSourceUID(s.UID); err != nil {
		p.add(invalidErrorf(s.declaredIn.File, key("source_uid"),
			"source %q: %s", s.ID, err))
	}
	if err := validateSourceID(s.ID); err != nil {
		p.add(invalidErrorf(origin("source_id"), key("source_id"), "%s", err))
	}

	adapter, known := c.Adapters[s.Adapter]
	switch {
	case s.Adapter == "":
		p.add(invalidErrorf(s.declaredIn.File, key("adapter"),
			"source %q names no adapter", s.ID))
	case !known:
		p.add(invalidErrorf(origin("adapter"), key("adapter"),
			"source %q names adapter %q, which is not registered; adapters are defined in user "+
				"configuration or built in, and the registered set is %v",
			s.ID, s.Adapter, slices.Sorted(maps.Keys(c.Adapters))))
	default:
		p.add(checkFreshness(s, adapter, origin, key))
	}

	p.add(checkPrior(origin("base_prior"), key("base_prior"), s.ID, "base_prior", s.BasePrior))
	for _, class := range slices.Sorted(maps.Keys(s.IntentPriors)) {
		field := "intent_priors." + class
		if class == "" {
			p.add(invalidErrorf(origin(field), key(field),
				"source %q: an intent prior must name the query class it applies to", s.ID))
		}
		p.add(checkPrior(origin(field), key(field), s.ID, field, s.IntentPriors[class]))
	}

	if s.Timeout <= 0 {
		p.add(invalidErrorf(origin("timeout_ms"), key("timeout_ms"),
			"source %q: timeout must be positive; a source with no time to answer reports "+
				"timeout, never empty success", s.ID))
	}
	if !s.Sensitivity.Valid() {
		p.add(invalidErrorf(origin("sensitivity"), key("sensitivity"),
			"source %q: unknown sensitivity", s.ID))
	}
	for _, rt := range s.RecordTypes {
		if strings.TrimSpace(string(rt)) == "" {
			p.add(invalidErrorf(origin("record_types"), key("record_types"),
				"source %q: record types must be named", s.ID))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.Secrets)) {
		field := "secrets." + name
		p.add(validateSecret(origin(field), key(field), s.Secrets[name]))
	}
	return p.err()
}

// checkFreshness enforces that a source asks its adapter for something the
// adapter says it can do. A mode nothing supports would otherwise surface as a
// runtime failure on a source that looked fine in `recall sources`.
//
// A mode left out is filled in during the merge, but only when the adapter
// supports exactly one; anything still empty here was genuinely ambiguous.
func checkFreshness(s *SourceInstance, a AdapterDefinition, origin, key func(string) string) error {
	if s.FreshnessMode == "" {
		return invalidErrorf(s.declaredIn.File, key("freshness_mode"),
			"source %q must choose a freshness mode: adapter %q supports %v",
			s.ID, a.Name, a.FreshnessModes)
	}
	if !a.Supports(s.FreshnessMode) {
		return invalidErrorf(origin("freshness_mode"), key("freshness_mode"),
			"source %q asks for freshness mode %q, which adapter %q does not support; it "+
				"supports %v", s.ID, s.FreshnessMode, a.Name, a.FreshnessModes)
	}
	return nil
}

// checkPrior enforces the validated range. A prior expresses expected
// authority for a query class; the bounds keep configuration from becoming an
// unbounded scoring language.
func checkPrior(file, key, sourceID, field string, value float64) error {
	if value >= MinPrior && value <= MaxPrior {
		return nil
	}
	return invalidErrorf(file, key,
		"source %q: %s is %g, outside the validated range [%g, %g]; priors are evaluation "+
			"parameters and are never clamped, because a clamped prior would explain a rank "+
			"the configuration does not show",
		sourceID, field, value, MinPrior, MaxPrior)
}

func validateSecret(file, key string, ref SecretRef) error {
	if !ref.Valid() {
		return invalidErrorf(file, key,
			"a secret is a reference: declare exactly one of env_var or keychain, never a value")
	}
	if ref.EnvVar != "" {
		if err := validateEnvVarName(ref.EnvVar); err != nil {
			return invalidErrorf(file, key+".env_var", "%s", err)
		}
	}
	return nil
}

func (c *Config) validateProfile(p Profile) error {
	var probs problems
	key := "profiles." + p.Name
	if err := validateName("profile", p.Name); err != nil {
		probs.add(invalidErrorf(p.Origin.File, key, "%s", err))
	}
	if !p.MaxSensitivity.Valid() {
		probs.add(invalidErrorf(p.origins["max_sensitivity"].File, key+".max_sensitivity",
			"unknown sensitivity ceiling"))
	}
	seen := map[string]bool{}
	for _, id := range p.SourceIDs {
		switch {
		case seen[id]:
			probs.add(invalidErrorf(p.origins["sources"].File, key+".sources",
				"profile %q names source %q twice", p.Name, id))
		case c.find(id) == nil:
			probs.add(invalidErrorf(p.origins["sources"].File, key+".sources",
				"profile %q names source %q, which is not configured", p.Name, id))
		}
		seen[id] = true
	}
	return probs.err()
}

// A Config is the resolver: it is the only place the mapping between a
// renameable name and an immutable identity is written down, so nothing else
// may implement this pair by inference. The interface is restated structurally
// rather than imported so that configuration stays below the locator layer.
var _ interface {
	UID(string) (recall.SourceUID, bool)
	ID(recall.SourceUID) (string, bool)
} = (*Config)(nil)
