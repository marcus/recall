package config

import (
	"fmt"
	"slices"

	"github.com/marcus/recall/internal/recall"
)

// Config is a resolved configuration: the user layer with the project layer
// merged over it, validated, and ordered deterministically.
//
// It is also the resolver that maps between a source's renameable display name
// and its immutable identity. Nothing else may infer that mapping — it exists
// only because a person wrote both halves in one place — which is why the
// locator layer takes it as an interface and this type satisfies it.
type Config struct {
	Paths    Paths                        `json:"paths"`
	Defaults Defaults                     `json:"defaults"`
	Adapters map[string]AdapterDefinition `json:"adapters"`

	// Sources is every configured source instance, ordered by source_id.
	Sources []*SourceInstance `json:"sources"`

	Profiles map[string]Profile `json:"profiles"`

	// files is every file that contributed, in merge order.
	files []sourceFile
	// defaultOrigins records which file supplied each entry in Defaults.
	defaultOrigins map[string]Origin

	index map[string]*SourceInstance
	uids  map[recall.SourceUID]*SourceInstance
}

// find returns the instance currently resolved under a display name, or nil.
// It scans rather than using the index, because the index is only built once
// merging is finished.
func (c *Config) find(sourceID string) *SourceInstance {
	for _, s := range c.Sources {
		if s.ID == sourceID {
			return s
		}
	}
	return nil
}

// UID returns the immutable identity of a configured display name. It, with
// [Config.ID], is the resolver the locator and lineage layers require.
func (c *Config) UID(sourceID string) (recall.SourceUID, bool) {
	s, ok := c.index[sourceID]
	if !ok {
		return "", false
	}
	return s.UID, true
}

// ID returns the display name of an immutable identity.
func (c *Config) ID(uid recall.SourceUID) (string, bool) {
	s, ok := c.uids[uid]
	if !ok {
		return "", false
	}
	return s.ID, true
}

// Source returns a configured source instance by display name.
func (c *Config) Source(sourceID string) (*SourceInstance, bool) {
	s, ok := c.index[sourceID]
	return s, ok
}

// SourceByUID returns a configured source instance by immutable identity. A
// locator that arrives in persisted form resolves through here, and a miss is
// the source_not_configured case: the locator is portable, this machine simply
// does not have that source.
func (c *Config) SourceByUID(uid recall.SourceUID) (*SourceInstance, bool) {
	s, ok := c.uids[uid]
	return s, ok
}

// Profile returns a named profile.
func (c *Config) Profile(name string) (Profile, bool) {
	p, ok := c.Profiles[name]
	return p, ok
}

// ActiveProfile resolves the profile for a request. An empty name selects the
// configured default, which is itself synthesized from every enabled source
// when nothing declared one.
func (c *Config) ActiveProfile(name string) (Profile, error) {
	if name == "" {
		name = c.Defaults.Profile
	}
	p, ok := c.Profiles[name]
	if !ok {
		return Profile{}, fmt.Errorf("%w: no profile named %q; configured profiles are %v",
			ErrInvalid, name, c.ProfileNames())
	}
	return p, nil
}

// ProfileNames lists configured profiles in sorted order.
func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// ProfileSources returns a profile's members in profile order.
//
// Every member is returned, including disabled ones and ones the ceiling
// denies. Eligibility is a reported decision, not a filter applied here:
// dropping a denied source silently would make it indistinguishable from one
// that was never configured.
func (c *Config) ProfileSources(name string) ([]*SourceInstance, error) {
	p, err := c.ActiveProfile(name)
	if err != nil {
		return nil, err
	}
	out := make([]*SourceInstance, 0, len(p.SourceIDs))
	for _, id := range p.SourceIDs {
		if s, ok := c.index[id]; ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// Adapter returns a registered adapter definition.
func (c *Config) Adapter(name string) (AdapterDefinition, bool) {
	a, ok := c.Adapters[name]
	return a, ok
}

// Files lists every configuration file that contributed, in merge order.
func (c *Config) Files() []FileView {
	out := make([]FileView, 0, len(c.files))
	for _, f := range c.files {
		out = append(out, FileView{Path: f.Path, Layer: f.Layer})
	}
	return out
}
