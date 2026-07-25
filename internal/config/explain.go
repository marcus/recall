package config

import (
	"maps"
	"slices"

	"github.com/marcus/recall/internal/recall"
)

// Explanation is the resolved configuration with the provenance of every
// value: what Recall will do, and which file said so.
//
// It is the structure behind `recall config explain --json`. Two layers merge
// silently by design, so without this a user cannot tell a value they wrote
// from one a project file replaced or one this package defaulted.
//
// It carries no secret material. Secrets are references throughout, and this
// view renders the reference, never a lookup of it.
type Explanation struct {
	Paths      PathsView         `json:"paths"`
	Files      []FileView        `json:"files"`
	Defaults   map[string]Field  `json:"defaults"`
	Evaluation map[string]Field  `json:"evaluation,omitempty"`
	Adapters   []AdapterView     `json:"adapters"`
	Sources    []SourceView      `json:"sources"`
	Profiles   []ProfileView     `json:"profiles"`
	Identity   []IdentityMapping `json:"identity"`
}

// Field is one resolved value and where it came from.
type Field struct {
	Value  any    `json:"value"`
	Layer  Layer  `json:"layer"`
	Origin string `json:"origin,omitempty"`
}

// FileView is one file that contributed, in merge order.
type FileView struct {
	Path  string `json:"path"`
	Layer Layer  `json:"layer"`
}

// PathsView is where Recall reads and writes for the active profile.
type PathsView struct {
	ConfigFile  string `json:"config_file"`
	AdaptersDir string `json:"adapters_dir"`
	StateDir    string `json:"state_dir"`
	CacheDir    string `json:"cache_dir"`
}

// AdapterView is a registered adapter, with its command shown because the
// whole point of the trust boundary is that a user can see what will run.
type AdapterView struct {
	Name           string            `json:"name"`
	Builtin        bool              `json:"builtin"`
	Layer          Layer             `json:"layer"`
	Origin         string            `json:"origin,omitempty"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	FreshnessModes []string          `json:"freshness_modes"`
	Conformance    string            `json:"conformance,omitempty"`
	// Secrets are rendered as references, for example "env_var:JIRA_TOKEN".
	Secrets map[string]string `json:"secrets,omitempty"`
}

// SourceView is one resolved source instance, field by field.
type SourceView struct {
	SourceUID  recall.SourceUID `json:"source_uid"`
	SourceID   string           `json:"source_id"`
	DeclaredIn string           `json:"declared_in,omitempty"`

	Fields       map[string]Field `json:"fields"`
	IntentPriors map[string]Field `json:"intent_priors,omitempty"`
	Settings     map[string]Field `json:"settings,omitempty"`
	// Secrets holds references only. No value here was read from the
	// environment or a keychain, because nothing in this package reads either.
	Secrets map[string]Field `json:"secrets,omitempty"`
}

// ProfileView is a resolved profile and its ceiling.
type ProfileView struct {
	Name           string           `json:"name"`
	SourceIDs      []string         `json:"sources"`
	MaxSensitivity string           `json:"max_sensitivity"`
	Fields         map[string]Field `json:"fields"`
}

// IdentityMapping is the source_id-to-source_uid table. Locator text uses the
// display name; every persisted reference uses the identity, so this is the
// table that makes an old evaluation pack readable after a rename.
type IdentityMapping struct {
	SourceID  string           `json:"source_id"`
	SourceUID recall.SourceUID `json:"source_uid"`
}

// Explain renders the resolved configuration and the origin of every value.
//
// The output is JSON-serializable and deterministic: sources and profiles are
// in sorted order and every map is keyed by name, so two runs over the same
// files produce byte-identical output and a diff means something changed.
func (c *Config) Explain() *Explanation {
	e := &Explanation{
		Paths: PathsView{
			ConfigFile:  c.Paths.ConfigFile(),
			AdaptersDir: c.Paths.AdaptersDir(),
			StateDir:    c.Paths.StateDir(c.Defaults.Profile),
			CacheDir:    c.Paths.CacheDir(c.Defaults.Profile),
		},
		Files:    c.Files(),
		Defaults: c.explainDefaults(),
	}
	if c.Evaluation.DevelopmentPack != "" || c.Evaluation.DevelopmentBaseline != "" {
		e.Evaluation = map[string]Field{}
		if c.Evaluation.DevelopmentPack != "" {
			e.Evaluation["development_pack"] = newField(
				c.Evaluation.DevelopmentPack, c.evaluationOrigins["development_pack"])
		}
		if c.Evaluation.DevelopmentBaseline != "" {
			e.Evaluation["development_baseline"] = newField(
				c.Evaluation.DevelopmentBaseline, c.evaluationOrigins["development_baseline"])
		}
	}

	for _, name := range slices.Sorted(maps.Keys(c.Adapters)) {
		e.Adapters = append(e.Adapters, explainAdapter(c.Adapters[name]))
	}
	for _, s := range c.Sources {
		e.Sources = append(e.Sources, explainSource(s))
		e.Identity = append(e.Identity, IdentityMapping{SourceID: s.ID, SourceUID: s.UID})
	}
	for _, name := range c.ProfileNames() {
		e.Profiles = append(e.Profiles, explainProfile(c.Profiles[name]))
	}
	return e
}

func (c *Config) explainDefaults() map[string]Field {
	field := func(name string, value any) Field {
		return newField(value, c.defaultOrigins[name])
	}
	return map[string]Field{
		"profile":           field("profile", c.Defaults.Profile),
		"timeout_ms":        field("timeout_ms", c.Defaults.Timeout.Milliseconds()),
		"fusion_reserve_ms": field("fusion_reserve_ms", c.Defaults.FusionReserve.Milliseconds()),
	}
}

func newField(value any, o Origin) Field {
	if o.Layer == "" {
		o.Layer = LayerDefault
	}
	return Field{Value: value, Layer: o.Layer, Origin: o.File}
}

func explainAdapter(a AdapterDefinition) AdapterView {
	v := AdapterView{
		Name:        a.Name,
		Builtin:     a.Builtin,
		Layer:       a.Origin.Layer,
		Origin:      a.Origin.File,
		Command:     a.Command,
		Args:        a.Args,
		Env:         a.Env,
		Conformance: a.Conformance,
	}
	for _, mode := range a.FreshnessModes {
		v.FreshnessModes = append(v.FreshnessModes, string(mode))
	}
	if len(a.Secrets) > 0 {
		v.Secrets = map[string]string{}
		for name, ref := range a.Secrets {
			v.Secrets[name] = ref.String()
		}
	}
	return v
}

func explainSource(s *SourceInstance) SourceView {
	v := SourceView{
		SourceUID:  s.UID,
		SourceID:   s.ID,
		DeclaredIn: s.declaredIn.File,
		Fields: map[string]Field{
			"source_uid":             newField(string(s.UID), s.Origin("source_uid")),
			"source_id":              newField(s.ID, s.Origin("source_id")),
			"adapter":                newField(s.Adapter, s.Origin("adapter")),
			"location":               newField(s.Location, s.Origin("location")),
			"location_original":      newField(s.DeclaredLocation, s.Origin("location")),
			"location_kind":          newField(string(s.LocationKind), s.Origin("location_kind")),
			"location_kind_explicit": newField(s.LocationKindExplicit, s.Origin("location_kind")),
			"location_rewritten":     newField(s.LocationRewritten, s.Origin("location")),
			"enabled":                newField(s.Enabled, s.Origin("enabled")),
			"freshness_mode":         newField(string(s.FreshnessMode), s.Origin("freshness_mode")),
			"freshness_policy":       newField(s.FreshnessPolicy, s.Origin("freshness_policy")),
			"base_prior":             newField(s.BasePrior, s.Origin("base_prior")),
			"sensitivity":            newField(s.Sensitivity.String(), s.Origin("sensitivity")),
			"timeout_ms":             newField(s.Timeout.Milliseconds(), s.Origin("timeout_ms")),
			"base_dir":               newField(s.BaseDir, s.Origin("location")),
		},
	}
	if s.RecordTypes != nil {
		types := make([]string, 0, len(s.RecordTypes))
		for _, rt := range s.RecordTypes {
			types = append(types, string(rt))
		}
		v.Fields["record_types"] = newField(types, s.Origin("record_types"))
	}
	v.IntentPriors = explainMap(s, "intent_priors", s.IntentPriors, func(f float64) any { return f })
	v.Settings = explainMap(s, "settings", s.Settings, func(a any) any { return a })
	// A secret renders as its reference. There is no branch here that could
	// produce a value: this package never resolves one.
	v.Secrets = explainMap(s, "secrets", s.Secrets, func(r SecretRef) any { return r.String() })
	return v
}

func explainMap[T any](s *SourceInstance, prefix string, in map[string]T, render func(T) any) map[string]Field {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]Field, len(in))
	for name, value := range in {
		out[name] = newField(render(value), s.Origin(prefix+"."+name))
	}
	return out
}

func explainProfile(p Profile) ProfileView {
	return ProfileView{
		Name:           p.Name,
		SourceIDs:      p.SourceIDs,
		MaxSensitivity: p.MaxSensitivity.String(),
		Fields: map[string]Field{
			"sources":         newField(p.SourceIDs, p.origins["sources"]),
			"max_sensitivity": newField(p.MaxSensitivity.String(), p.origins["max_sensitivity"]),
		},
	}
}
