package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/marcus/recall/internal/recall"
)

// Layer names where a resolved value came from. The order of the constants is
// the merge order: later layers win.
type Layer string

const (
	// LayerDefault is a value this package supplied because nothing declared
	// one. It appears in [Config.Explain] so a default is never mistaken for
	// something a user wrote.
	LayerDefault Layer = "default"
	// LayerBuiltin is an adapter compiled into Recall and registered in code.
	LayerBuiltin Layer = "builtin"
	// LayerUser is user configuration. It is trusted and is the only layer that
	// may declare an adapter command.
	LayerUser Layer = "user"
	// LayerProject is a project's recall.toml. It travels with a clone and is
	// untrusted.
	LayerProject Layer = "project"
)

// Origin is where one resolved value came from.
type Origin struct {
	Layer Layer `json:"layer"`
	// File is the file that declared the value. It is empty for LayerDefault
	// and LayerBuiltin, which have no file.
	File string `json:"file,omitempty"`
}

func (o Origin) String() string {
	if o.File == "" {
		return string(o.Layer)
	}
	return string(o.Layer) + " (" + o.File + ")"
}

// Prior bounds. A prior expresses expected authority for a query class; it
// does not calibrate a source's native score. The range is deliberately narrow
// so configuration cannot become an unbounded scoring language.
const (
	MinPrior = 0.5
	MaxPrior = 2.0
)

// Defaults are the request-shaping values a source instance inherits when it
// declares none of its own.
type Defaults struct {
	// Profile is the profile used when a request names none.
	Profile string `json:"profile"`
	// Timeout is the per-source query budget for sources that declare none.
	Timeout time.Duration `json:"timeout_ns"`
	// FusionReserve is held back from the request deadline so fusion has time
	// to run after the last source answers.
	FusionReserve time.Duration `json:"fusion_reserve_ns"`
}

// DefaultProfileName is the profile resolved when nothing names one.
const DefaultProfileName = "default"

var builtinDefaults = Defaults{
	Profile:       DefaultProfileName,
	Timeout:       2 * time.Second,
	FusionReserve: 25 * time.Millisecond,
}

// SecretRef is a reference to a credential. It is never the credential.
//
// This package resolves neither kind: it does not read the environment
// variable and does not touch a keychain. A resolved configuration therefore
// holds no secret material and is safe to serialize, log, and print.
type SecretRef struct {
	// EnvVar is the name of an environment variable, read at the moment of use.
	EnvVar string `json:"env_var,omitempty"`
	// Keychain is an OS keychain reference, in whatever form the platform
	// helper accepts.
	Keychain string `json:"keychain,omitempty"`
}

// String renders the reference. There is no method that renders a value.
func (s SecretRef) String() string {
	switch {
	case s.EnvVar != "":
		return "env_var:" + s.EnvVar
	case s.Keychain != "":
		return "keychain:" + s.Keychain
	default:
		return "unset"
	}
}

// Valid reports whether exactly one kind of reference is set.
func (s SecretRef) Valid() bool {
	return (s.EnvVar != "") != (s.Keychain != "")
}

// AdapterDefinition registers an adapter Recall may run.
//
// Only the user layer produces one: either from $XDG_CONFIG_HOME/recall
// (config.toml or adapters.d/*.toml), or from the built-in registry passed to
// [Load]. A project file may name an adapter but never define one.
type AdapterDefinition struct {
	Name string `json:"name"`
	// Builtin means the adapter is compiled into Recall and has no command.
	Builtin bool `json:"builtin"`

	// Command is the executable. It is either an absolute path or a bare name
	// looked up on PATH; a relative path is rejected, because it would resolve
	// against whatever directory Recall happened to be started in.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`

	// Env is non-secret process environment. Anything sensitive belongs in
	// Secrets, which holds references rather than values.
	Env     map[string]string    `json:"env,omitempty"`
	Secrets map[string]SecretRef `json:"secrets,omitempty"`

	// FreshnessModes is what this adapter can serve. A source instance naming a
	// mode outside this set is a configuration error rather than a surprise at
	// query time.
	FreshnessModes []recall.FreshnessMode `json:"freshness_modes"`

	// Conformance is the directory of recorded transcripts
	// `recall doctor --conformance` replays against Command. It is absolute for
	// the same reason Command is: a relative path would resolve against
	// whatever directory Recall happened to be started in, and a conformance
	// run that silently checked a different suite would be worse than one that
	// checked nothing.
	//
	// docs/adapter-protocol.md fixes the layout inside it. An adapter that
	// ships no transcripts declares nothing here and cannot be conformance
	// checked, which is a fact about the adapter and is reported as one.
	Conformance string `json:"conformance,omitempty"`

	Origin Origin `json:"origin"`
}

// Supports reports whether the adapter declares a freshness mode.
func (a AdapterDefinition) Supports(mode recall.FreshnessMode) bool {
	for _, got := range a.FreshnessModes {
		if got == mode {
			return true
		}
	}
	return false
}

// Builtin describes an adapter compiled into this binary. The registry owns
// these; configuration validates against them and never creates one.
type Builtin struct {
	Name           string
	FreshnessModes []recall.FreshnessMode
}

// LocationKind is the effective semantic contract for a source location. It is
// explicit when configuration declares location_kind and otherwise inferred by
// the documented compatibility rule. Recording it on the resolved source keeps
// every consumer on the same decision; in particular, doctor must not
// rediscover "path or identifier" from the resolved string.
type LocationKind string

const (
	LocationEmpty  LocationKind = "empty"
	LocationPath   LocationKind = "path"
	LocationOpaque LocationKind = "opaque"
	LocationScheme LocationKind = "uri"
)

// SourceInstance is one configured use of an adapter: its own identity,
// location, policy, and ranking prior. Two projects may use one adapter
// without becoming one source.
type SourceInstance struct {
	// UID is immutable, generated once, and never edited. Every persisted
	// reference keys on it, so a rename cannot invalidate an evaluation pack.
	UID recall.SourceUID `json:"source_uid"`
	// ID is the display and CLI name. It may be renamed freely and may not
	// contain the locator separator.
	ID string `json:"source_id"`

	Adapter  string `json:"adapter"`
	Location string `json:"location,omitempty"`
	Enabled  bool   `json:"enabled"`

	// DeclaredLocation is the exact configured value. Location is what the
	// adapter receives after path resolution; for opaque identifiers and
	// schemes the two are identical.
	DeclaredLocation     string       `json:"declared_location,omitempty"`
	LocationKind         LocationKind `json:"location_kind"`
	LocationKindExplicit bool         `json:"location_kind_explicit"`
	LocationRewritten    bool         `json:"location_rewritten"`

	// RecordTypes optionally narrows the source below the adapter's default
	// scope. Empty means the adapter's default.
	RecordTypes []recall.RecordType `json:"record_types,omitempty"`

	FreshnessMode   recall.FreshnessMode `json:"freshness_mode"`
	FreshnessPolicy string               `json:"freshness_policy,omitempty"`

	// BasePrior is the cross-source prior. IntentPriors replace it for a named
	// query class. Both are validated into [MinPrior, MaxPrior] and both appear
	// in every score explanation.
	BasePrior    float64            `json:"base_prior"`
	IntentPriors map[string]float64 `json:"intent_priors,omitempty"`

	// Sensitivity is this source's classification floor. A candidate may raise
	// it, never lower it.
	Sensitivity recall.Sensitivity `json:"sensitivity"`

	Timeout time.Duration `json:"timeout_ns"`

	// Settings is adapter-owned. This package carries it, bounds nothing inside
	// it, and leaves schema validation to the handshake, where the adapter's
	// declared settings schema is known.
	Settings map[string]any `json:"settings,omitempty"`

	// Secrets are references only. See [SecretRef].
	Secrets map[string]SecretRef `json:"secrets,omitempty"`

	// BaseDir is the directory of the file that last declared this source. An
	// adapter resolving a relative path out of Settings resolves it against
	// this, for the same reason Location does.
	BaseDir string `json:"base_dir,omitempty"`

	// origins records which file supplied each field, keyed by its TOML name.
	// Map-valued fields are keyed per entry, as "settings.k".
	origins map[string]Origin
	// keys records the dotted TOML path each field was written at, in the file
	// that last wrote it, so an error points at the line rather than the
	// concept: "sources[1].base_prior", not "base_prior".
	keys map[string]string
	// declaredIn is the file that introduced this source, and declaredKey is
	// where in that file. A later layer may adjust the instance but never
	// claims to have created it.
	declaredIn  Origin
	declaredKey string
}

// DeclaredIn reports the file that introduced this source instance, as opposed
// to the files that later adjusted it.
func (s SourceInstance) DeclaredIn() Origin { return s.declaredIn }

// fieldKey is the dotted TOML path to report a problem with a field at. A
// field nobody declared is reported against the entry that should have
// declared it.
func (s SourceInstance) fieldKey(field string) string {
	if k, ok := s.keys[field]; ok {
		return k
	}
	return s.declaredKey + "." + field
}

// Origin reports where a field came from, keyed by its TOML name.
func (s SourceInstance) Origin(field string) Origin {
	if o, ok := s.origins[field]; ok {
		return o
	}
	return Origin{Layer: LayerDefault}
}

// Prior returns the prior for a query class, explained.
//
// An intent prior replaces the base rather than scaling it, so the configured
// number is the authority actually applied and the reported Intent is the
// visible difference from the base. A class with no configured prior fires no
// rule, which keeps an unattributable adjustment impossible.
func (s SourceInstance) Prior(intent string) recall.PriorExplanation {
	p := recall.PriorExplanation{Base: s.BasePrior, Effective: s.BasePrior}
	if v, ok := s.IntentPriors[intent]; ok {
		p.Rule = intent
		p.Intent = v - s.BasePrior
		p.Effective = v
	}
	return p
}

// Locator builds a fully resolved locator for a record in this source, with
// both the display name and the immutable identity attached.
func (s SourceInstance) Locator(local string) recall.Locator {
	return recall.Locator{SourceID: s.ID, SourceUID: s.UID, Local: local}
}

// Profile is a named set of source instances and a sensitivity ceiling.
type Profile struct {
	Name string `json:"name"`
	// SourceIDs names the members by display name, in declared order.
	SourceIDs []string `json:"sources"`
	// MaxSensitivity is the ceiling. A source above it is ineligible and is
	// reported as denied rather than quietly omitted.
	MaxSensitivity recall.Sensitivity `json:"max_sensitivity"`

	Origin Origin `json:"origin"`
	// origins records per-field provenance, keyed by TOML name.
	origins map[string]Origin
}

// Permits reports whether the profile's ceiling admits a source. It is the
// only sensitivity question configuration can answer: a candidate may raise
// its own classification later, and that check belongs with the candidate.
func (p Profile) Permits(s SourceInstance) bool {
	return s.Sensitivity.AtMost(p.MaxSensitivity)
}

// Contains reports whether a source is a member of the profile.
func (p Profile) Contains(sourceID string) bool {
	for _, id := range p.SourceIDs {
		if id == sourceID {
			return true
		}
	}
	return false
}

// parseFreshnessMode accepts a declared mode name.
func parseFreshnessMode(s string) (recall.FreshnessMode, error) {
	switch recall.FreshnessMode(strings.ToLower(s)) {
	case recall.FreshnessLive:
		return recall.FreshnessLive, nil
	case recall.FreshnessIndexed:
		return recall.FreshnessIndexed, nil
	case recall.FreshnessHybrid:
		return recall.FreshnessHybrid, nil
	default:
		return "", fmt.Errorf("unknown freshness mode %q, want live, indexed, or hybrid", s)
	}
}
