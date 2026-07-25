package config

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/marcus/recall/internal/recall"
)

// Options selects what [Load] reads.
type Options struct {
	// Paths locates the XDG directories. A zero value resolves them from the
	// environment; tests set it directly so nothing depends on the machine.
	Paths Paths

	// ProjectFile is a project recall.toml. Empty means the project layer is
	// absent. [DiscoverProject] finds one from a working directory.
	ProjectFile string

	// Builtins are the adapters compiled into this binary. Configuration
	// validates source instances against them; it never creates one, and a
	// built-in never carries a command.
	Builtins []Builtin
}

// Load resolves configuration: the user layer, then the project layer over it.
//
// It returns no partial result. A configuration that half-applied would
// silently change which sources answer a query, and invariant 2 forbids
// reporting that as anything but a failure.
func Load(opts Options) (*Config, error) {
	paths := opts.Paths
	if !paths.resolved() {
		resolved, err := XDGPaths()
		if err != nil {
			return nil, err
		}
		paths = resolved
	}

	files, err := readLayers(paths, opts.ProjectFile)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Paths:    paths,
		Defaults: builtinDefaults,
		Adapters: map[string]AdapterDefinition{},
		Profiles: map[string]Profile{},
		files:    files,
	}
	for _, b := range opts.Builtins {
		cfg.Adapters[b.Name] = AdapterDefinition{
			Name:           b.Name,
			Builtin:        true,
			FreshnessModes: slices.Clone(b.FreshnessModes),
			Origin:         Origin{Layer: LayerBuiltin},
		}
	}

	var probs problems
	for _, f := range files {
		probs.add(cfg.mergeFile(f))
	}
	if err := probs.err(); err != nil {
		return nil, err
	}

	cfg.finish()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// readLayers parses every file that contributes, in merge order: user
// configuration, then adapters.d, then the project file.
func readLayers(paths Paths, projectFile string) ([]sourceFile, error) {
	var files []sourceFile

	userPath := paths.ConfigFile()
	switch _, err := os.Stat(userPath); {
	case err == nil:
		f, err := parseFile(userPath, LayerUser)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("reading %s: %w", userPath, err)
	}

	adapters, err := parseAdaptersDir(paths.AdaptersDir())
	if err != nil {
		return nil, err
	}
	files = append(files, adapters...)

	if projectFile != "" {
		f, err := parseFile(projectFile, LayerProject)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
}

// mergeFile applies one file over what is already resolved.
func (c *Config) mergeFile(f sourceFile) error {
	var probs problems
	probs.add(c.mergeDefaults(f))
	probs.add(c.mergeAdapters(f))
	probs.add(c.mergeSources(f))
	probs.add(c.mergeProfiles(f))
	return probs.err()
}

func (c *Config) mergeDefaults(f sourceFile) error {
	d := f.Raw.Defaults
	if d == nil {
		return nil
	}
	if c.defaultOrigins == nil {
		c.defaultOrigins = map[string]Origin{}
	}
	if d.Profile != nil {
		c.Defaults.Profile = *d.Profile
		c.defaultOrigins["profile"] = f.Origin
	}
	if d.TimeoutMS != nil {
		c.Defaults.Timeout = time.Duration(*d.TimeoutMS) * time.Millisecond
		c.defaultOrigins["timeout_ms"] = f.Origin
	}
	if d.FusionReserveMS != nil {
		c.Defaults.FusionReserve = time.Duration(*d.FusionReserveMS) * time.Millisecond
		c.defaultOrigins["fusion_reserve_ms"] = f.Origin
	}
	return nil
}

// mergeAdapters registers adapter definitions. Only the user layer reaches
// here: the trust boundary check already refused an adapters table in a
// project file, and this is the second gate saying the same thing.
func (c *Config) mergeAdapters(f sourceFile) error {
	if len(f.Raw.Adapters) == 0 {
		return nil
	}
	if f.Layer != LayerUser {
		return trustErrorf(f.Path, "adapters", "only user configuration may define an adapter")
	}

	var probs problems
	for _, name := range slices.Sorted(maps.Keys(f.Raw.Adapters)) {
		raw := f.Raw.Adapters[name]
		key := "adapters." + name

		if prev, exists := c.Adapters[name]; exists {
			// Neither collision has a deterministic winner, and picking one
			// silently would make the command that runs depend on the order
			// files happened to be read in.
			if prev.Origin.Layer == LayerBuiltin {
				probs.add(invalidErrorf(f.Path, key,
					"adapter %q is built in and cannot be redefined", name))
			} else {
				probs.add(invalidErrorf(f.Path, key,
					"adapter %q is already defined by %s", name, prev.Origin))
			}
			continue
		}

		def := AdapterDefinition{
			Name:    name,
			Args:    slices.Clone(raw.Args),
			Env:     maps.Clone(raw.Env),
			Secrets: map[string]SecretRef{},
			Origin:  f.Origin,
		}
		if raw.Command != nil {
			def.Command = *raw.Command
		}
		for _, sname := range slices.Sorted(maps.Keys(raw.Secrets)) {
			def.Secrets[sname] = raw.Secrets[sname].ref()
		}
		if len(def.Secrets) == 0 {
			def.Secrets = nil
		}
		for _, mode := range raw.FreshnessModes {
			parsed, err := parseFreshnessMode(mode)
			if err != nil {
				probs.add(invalidErrorf(f.Path, key+".freshness_modes", "%s", err))
				continue
			}
			def.FreshnessModes = append(def.FreshnessModes, parsed)
		}
		c.Adapters[name] = def
	}
	return probs.err()
}

func (r rawSecret) ref() SecretRef {
	var ref SecretRef
	if r.EnvVar != nil {
		ref.EnvVar = *r.EnvVar
	}
	if r.Keychain != nil {
		ref.Keychain = *r.Keychain
	}
	return ref
}

// mergeSources applies a file's source instances.
//
// The merge key is source_id, the name a person writes. source_uid is the
// identity behind it and is assigned exactly once, by the layer that
// introduced the source: a project file may add a source of its own, and may
// adjust one the user declared, but may never restate or reassign an existing
// identity. Redirecting a source_uid would silently repoint every persisted
// locator and evaluation judgment at different data.
func (c *Config) mergeSources(f sourceFile) error {
	var probs problems
	seen := map[string]int{}

	for i, raw := range f.Raw.Sources {
		key := fmt.Sprintf("sources[%d]", i)
		if raw.SourceID == nil {
			probs.add(invalidErrorf(f.Path, key+".source_id",
				"source_id is required; it is how this entry is merged and displayed"))
			continue
		}
		id := *raw.SourceID
		if prev, dup := seen[id]; dup {
			probs.add(invalidErrorf(f.Path, key+".source_id",
				"source_id %q is already declared by sources[%d] in this file", id, prev))
			continue
		}
		seen[id] = i

		if existing := c.find(id); existing != nil {
			probs.add(c.overlaySource(f, key, existing, raw))
			continue
		}
		probs.add(c.newSource(f, key, id, raw))
	}
	return probs.err()
}

// newSource creates a source instance an earlier layer did not declare.
func (c *Config) newSource(f sourceFile, key, id string, raw rawSource) error {
	inst := &SourceInstance{
		ID:          id,
		Enabled:     true,
		BasePrior:   1.0,
		Sensitivity: recall.SensitivityInternal,
		BaseDir:     f.Dir,
		origins:     map[string]Origin{"source_id": f.Origin},
		keys:        map[string]string{"source_id": key + ".source_id"},
		declaredIn:  f.Origin,
		declaredKey: key,
	}
	if raw.SourceUID != nil {
		inst.UID = recall.SourceUID(*raw.SourceUID)
		inst.origins["source_uid"] = f.Origin
		inst.keys["source_uid"] = key + ".source_uid"
	}
	err := c.applyRaw(f, key, inst, raw)
	c.Sources = append(c.Sources, inst)
	return err
}

// overlaySource applies a later layer over a source an earlier one declared.
func (c *Config) overlaySource(f sourceFile, key string, inst *SourceInstance, raw rawSource) error {
	var probs problems
	if raw.SourceUID != nil {
		probs.add(trustErrorf(f.Path, key+".source_uid",
			"source %q already has an immutable identity from %s; reassigning it would "+
				"repoint every persisted locator and judgment that keys on it",
			inst.ID, inst.origins["source_uid"]))
	}
	if raw.Adapter != nil && f.Layer == LayerProject && *raw.Adapter != inst.Adapter {
		probs.add(trustErrorf(f.Path, key+".adapter",
			"source %q is served by adapter %q from %s; a project file may not repoint an "+
				"existing source at a different adapter", inst.ID, inst.Adapter, inst.declaredIn))
	}
	if raw.Sensitivity != nil && f.Layer == LayerProject {
		if err := checkSensitivityFloor(f, key, inst, *raw.Sensitivity); err != nil {
			probs.add(err)
		}
	}
	probs.add(c.applyRaw(f, key, inst, raw))
	return probs.err()
}

// checkSensitivityFloor keeps a project file from lowering a classification
// the user set. Sensitivity is a floor, and lowering one would widen what a
// profile ceiling admits — access granted by a file that arrived with a clone.
func checkSensitivityFloor(f sourceFile, key string, inst *SourceInstance, declared string) error {
	level, err := recall.ParseSensitivity(declared)
	if err != nil {
		return invalidErrorf(f.Path, key+".sensitivity", "%s", err)
	}
	if level < inst.Sensitivity {
		return trustErrorf(f.Path, key+".sensitivity",
			"source %q is classified %s by %s; a project file may raise a sensitivity floor "+
				"but never lower it to %s", inst.ID, inst.Sensitivity, inst.declaredIn, level)
	}
	return nil
}

// applyRaw writes every declared field onto an instance and records which file
// each one came from.
func (c *Config) applyRaw(f sourceFile, key string, inst *SourceInstance, raw rawSource) error {
	var probs problems
	set := func(field string) {
		inst.origins[field] = f.Origin
		inst.keys[field] = key + "." + field
	}

	if raw.Adapter != nil {
		inst.Adapter = *raw.Adapter
		set("adapter")
	}
	if raw.Location != nil {
		location, err := resolveLocation(*raw.Location, f.Dir)
		if err != nil {
			probs.add(invalidErrorf(f.Path, key+".location", "%s", err))
		} else {
			inst.Location = location
			set("location")
		}
	}
	if raw.Enabled != nil {
		inst.Enabled = *raw.Enabled
		set("enabled")
	}
	if raw.RecordTypes != nil {
		inst.RecordTypes = make([]recall.RecordType, 0, len(raw.RecordTypes))
		for _, rt := range raw.RecordTypes {
			inst.RecordTypes = append(inst.RecordTypes, recall.RecordType(rt))
		}
		set("record_types")
	}
	if raw.FreshnessMode != nil {
		mode, err := parseFreshnessMode(*raw.FreshnessMode)
		if err != nil {
			probs.add(invalidErrorf(f.Path, key+".freshness_mode", "%s", err))
		} else {
			inst.FreshnessMode = mode
			set("freshness_mode")
		}
	}
	if raw.FreshnessPolicy != nil {
		inst.FreshnessPolicy = *raw.FreshnessPolicy
		set("freshness_policy")
	}
	if raw.BasePrior != nil {
		inst.BasePrior = *raw.BasePrior
		set("base_prior")
	}
	for _, class := range slices.Sorted(maps.Keys(raw.IntentPriors)) {
		if inst.IntentPriors == nil {
			inst.IntentPriors = map[string]float64{}
		}
		inst.IntentPriors[class] = raw.IntentPriors[class]
		set("intent_priors." + class)
	}
	if raw.Sensitivity != nil {
		level, err := recall.ParseSensitivity(*raw.Sensitivity)
		if err != nil {
			probs.add(invalidErrorf(f.Path, key+".sensitivity", "%s", err))
		} else {
			inst.Sensitivity = level
			set("sensitivity")
		}
	}
	if raw.TimeoutMS != nil {
		inst.Timeout = time.Duration(*raw.TimeoutMS) * time.Millisecond
		set("timeout_ms")
	}
	for _, k := range slices.Sorted(maps.Keys(raw.Settings)) {
		if inst.Settings == nil {
			inst.Settings = map[string]any{}
		}
		inst.Settings[k] = raw.Settings[k]
		set("settings." + k)
	}
	for _, name := range slices.Sorted(maps.Keys(raw.Secrets)) {
		if inst.Secrets == nil {
			inst.Secrets = map[string]SecretRef{}
		}
		inst.Secrets[name] = raw.Secrets[name].ref()
		set("secrets." + name)
	}

	// A later layer's relative paths resolve against its own file, so the
	// directory an adapter should resolve settings paths against follows the
	// file that spoke last.
	inst.BaseDir = f.Dir
	return probs.err()
}

// mergeProfiles applies a file's profiles. A project file may narrow a
// ceiling, never widen one.
func (c *Config) mergeProfiles(f sourceFile) error {
	var probs problems
	for _, name := range slices.Sorted(maps.Keys(f.Raw.Profiles)) {
		raw := f.Raw.Profiles[name]
		key := "profiles." + name

		p, exists := c.Profiles[name]
		if !exists {
			p = Profile{
				Name:           name,
				MaxSensitivity: recall.SensitivityRestricted,
				Origin:         f.Origin,
				origins:        map[string]Origin{},
			}
		}
		if raw.Sources != nil {
			p.SourceIDs = slices.Clone(raw.Sources)
			p.origins["sources"] = f.Origin
		}
		if raw.MaxSensitivity != nil {
			level, err := recall.ParseSensitivity(*raw.MaxSensitivity)
			switch {
			case err != nil:
				probs.add(invalidErrorf(f.Path, key+".max_sensitivity", "%s", err))
			case exists && f.Layer == LayerProject && level > p.MaxSensitivity:
				probs.add(trustErrorf(f.Path, key+".max_sensitivity",
					"profile %q is capped at %s by %s; a project file may lower a ceiling "+
						"but never raise it to %s", name, p.MaxSensitivity, p.origins["max_sensitivity"], level))
			default:
				p.MaxSensitivity = level
				p.origins["max_sensitivity"] = f.Origin
			}
		}
		c.Profiles[name] = p
	}
	return probs.err()
}

// finish folds newly declared sources into the resolved set, orders everything
// deterministically, and synthesizes the default profile when none was
// declared.
func (c *Config) finish() {
	slices.SortFunc(c.Sources, func(a, b *SourceInstance) int { return strings.Compare(a.ID, b.ID) })

	// Inherited defaults are applied only once every layer has spoken, so a
	// project raising defaults.timeout_ms reaches the sources the user
	// declared. Inheritance is keyed on whether the field was declared, not on
	// whether it is zero: an explicit "timeout_ms = 0" is a mistake that must
	// still be reported, not quietly replaced by the default.
	for _, s := range c.Sources {
		if _, declared := s.origins["timeout_ms"]; !declared {
			s.Timeout = c.Defaults.Timeout
		}
		c.inferFreshnessMode(s)
	}

	if _, ok := c.Profiles[DefaultProfileName]; !ok {
		p := Profile{
			Name:           DefaultProfileName,
			MaxSensitivity: recall.SensitivityRestricted,
			Origin:         Origin{Layer: LayerDefault},
			origins:        map[string]Origin{},
		}
		for _, s := range c.Sources {
			if s.Enabled {
				p.SourceIDs = append(p.SourceIDs, s.ID)
			}
		}
		c.Profiles[DefaultProfileName] = p
	}

	c.buildIndexes()
}

// inferFreshnessMode fills in a mode the source left out, but only when the
// adapter supports exactly one. Choosing between two supported modes would be
// deciding freshness policy on the user's behalf without saying so, and the
// resulting error names the alternatives instead.
func (c *Config) inferFreshnessMode(s *SourceInstance) {
	if _, declared := s.origins["freshness_mode"]; declared {
		return
	}
	a, known := c.Adapters[s.Adapter]
	if !known || len(a.FreshnessModes) != 1 {
		return
	}
	s.FreshnessMode = a.FreshnessModes[0]
	s.origins["freshness_mode"] = a.Origin
}

func (c *Config) buildIndexes() {
	c.index = make(map[string]*SourceInstance, len(c.Sources))
	c.uids = make(map[recall.SourceUID]*SourceInstance, len(c.Sources))
	for _, s := range c.Sources {
		c.index[s.ID] = s
		if s.UID != "" {
			if _, taken := c.uids[s.UID]; !taken {
				c.uids[s.UID] = s
			}
		}
	}
}
