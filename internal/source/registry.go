// Package source resolves configured source instances into live adapters and
// decides which of them a request may reach.
//
// Boundary: it owns the registry and the hard eligibility rules — explicit
// scope, permission, health, budget, as_of support, record types. There is no
// intent router and no identifier routing here, because broad parallel search
// is the default and exactness is settled at fusion, where the core never has
// to learn which source owns which identifier format.
package source

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/marcus/recall/internal/adapter"
	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// ErrUnknownAdapter means a source names an adapter nothing can build.
var ErrUnknownAdapter = errors.New("unknown adapter")

// Factory builds a built-in adapter. External adapters need no factory: they
// are a command, and the registry supervises them.
type Factory func() adapter.Adapter

// Registry turns configured source instances into adapters and keeps them for
// the process lifetime.
//
// Every adapter it hands out is wrapped by [adapter.WithIdentity]. That is the
// point of resolving them here rather than at each call site: identity stamping
// is what stops one source answering as another, and a boundary that can be
// forgotten is one that will be.
type Registry struct {
	cfg       *config.Config
	builtins  map[string]Factory
	stateDir  string
	adapterOf map[recall.SourceUID]adapter.Adapter

	mu sync.Mutex
}

// Options configure a registry.
type Options struct {
	// Builtins maps an adapter name to its constructor.
	Builtins map[string]Factory
	// StateDir is the root under which each adapter is given a workdir.
	StateDir string
}

// NewRegistry prepares a registry. Nothing is spawned or opened until a source
// is first used: a configured source that is never queried costs nothing.
func NewRegistry(cfg *config.Config, opt Options) *Registry {
	return &Registry{
		cfg:       cfg,
		builtins:  opt.Builtins,
		stateDir:  opt.StateDir,
		adapterOf: map[recall.SourceUID]adapter.Adapter{},
	}
}

// Adapter returns the adapter serving a source, building it on first use.
func (r *Registry) Adapter(inst *config.SourceInstance) (adapter.Adapter, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if a, ok := r.adapterOf[inst.UID]; ok {
		return a, nil
	}
	a, err := r.build(inst)
	if err != nil {
		return nil, err
	}
	// Identity is attached here and nowhere else.
	bound := adapter.WithIdentity(a, adapter.Identity{
		UID:   inst.UID,
		ID:    inst.ID,
		Floor: inst.Sensitivity,
	})
	r.adapterOf[inst.UID] = bound
	return bound, nil
}

func (r *Registry) build(inst *config.SourceInstance) (adapter.Adapter, error) {
	def, ok := r.cfg.Adapter(inst.Adapter)
	if !ok {
		return nil, fmt.Errorf("%w %q for source %q", ErrUnknownAdapter, inst.Adapter, inst.ID)
	}
	if def.Builtin {
		factory, ok := r.builtins[inst.Adapter]
		if !ok {
			return nil, fmt.Errorf("%w %q: declared built-in but not compiled in",
				ErrUnknownAdapter, inst.Adapter)
		}
		return factory(), nil
	}
	return adapter.NewExternal(adapter.Spec{
		Name:    inst.ID,
		Command: def.Command,
		Args:    def.Args,
		Env:     environ(def.Env),
		Config:  r.handshake(inst),
	}), nil
}

// environ renders a configured environment for exec, appending to the parent's
// so an adapter still finds a PATH.
//
// Sorted, because a process environment that varies between runs would make an
// evaluation run non-reproducible for no reason.
func environ(in map[string]string) []string {
	if len(in) == 0 {
		return nil
	}
	out := os.Environ()
	for _, k := range slices.Sorted(maps.Keys(in)) {
		out = append(out, k+"="+in[k])
	}
	return out
}

// handshake is what an adapter is told about the instance it serves.
func (r *Registry) handshake(inst *config.SourceInstance) adapter.Config {
	return adapter.Config{
		ProtocolVersionMin: protocol.MinVersion,
		ProtocolVersionMax: protocol.MaxVersion,
		Workdir:            r.Workdir(inst),
		SourceID:           inst.ID,
		Location:           inst.Location,
		BaseDir:            inst.BaseDir,
		Settings:           inst.Settings,
	}
}

// Workdir is where an adapter may write its index, keyed by immutable identity
// so a rename does not orphan a generation.
func (r *Registry) Workdir(inst *config.SourceInstance) string {
	return filepath.Join(r.stateDir, "adapters", string(inst.UID))
}

// Initialize brings a source up and returns its manifest.
func (r *Registry) Initialize(ctx context.Context, inst *config.SourceInstance) (recall.Manifest, error) {
	a, err := r.Adapter(inst)
	if err != nil {
		return recall.Manifest{}, err
	}
	return a.Initialize(ctx, r.handshake(inst))
}

// Close releases every adapter built so far.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var errs []error
	for uid, a := range r.adapterOf {
		if err := a.Close(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", uid, err))
		}
		delete(r.adapterOf, uid)
	}
	return errors.Join(errs...)
}
