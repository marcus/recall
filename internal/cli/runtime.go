package cli

import (
	"context"
	"os"
	"time"

	"github.com/marcus/recall/internal/app"
	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/recall"
	"github.com/marcus/recall/internal/source"
)

// load resolves configuration the way every command needs it: the user layer,
// then whichever project file governs the working directory.
func (e Env) load() (*config.Config, error) {
	dir := e.Dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		dir = wd
	}
	project, err := config.DiscoverProject(dir)
	if err != nil {
		return nil, err
	}

	builtins := make([]config.Builtin, 0, len(e.adapters()))
	for _, a := range e.adapters() {
		builtins = append(builtins, config.Builtin{Name: a.Name, FreshnessModes: a.FreshnessModes})
	}
	return config.Load(config.Options{
		Paths:       e.Paths,
		ProjectFile: project,
		Builtins:    builtins,
	})
}

// runtime is one invocation's assembled core: configuration, the source
// registry, and the application layer over both.
type runtime struct {
	cfg      *config.Config
	registry *source.Registry
	app      *app.App
	profile  string
}

// newRuntime wires configuration into the application layer.
//
// The wiring itself lives in [app.Build], not here. Every transport must
// produce identical results for identical requests, and the surest way to break
// that is to have each one translate priors and intent classes for itself —
// three mappings that diverge the first time one is edited.
func newRuntime(env Env, cfg *config.Config, profile string, limit int) (*runtime, error) {
	if profile == "" {
		profile = cfg.Defaults.Profile
	}
	core, registry, err := app.Build(app.BuildOptions{
		Config:   cfg,
		Builtins: factories(env),
		StateDir: cfg.Paths.StateDir(profile),
		Limit:    limit,
		Costs:    renderCosts(),
		Now:      env.now(),
	})
	if err != nil {
		return nil, err
	}
	return &runtime{cfg: cfg, registry: registry, app: core, profile: profile}, nil
}

func (r *runtime) close() error { return r.registry.Close() }

func factories(env Env) map[string]source.Factory {
	out := map[string]source.Factory{}
	for _, a := range env.adapters() {
		out[a.Name] = a.New
	}
	return out
}

// probe brings one source up and asks it how it is.
//
// A source that cannot be built or cannot be reached returns its error and
// whatever health it managed to report: `recall sources` and `recall doctor`
// exist to show exactly that state, so a failure here is a result, not an abort.
func (r *runtime) probe(ctx context.Context, inst *config.SourceInstance) (recall.Manifest, recall.Health, error) {
	timeout := inst.Timeout
	if timeout <= 0 {
		timeout = probeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	manifest, err := r.registry.Initialize(ctx, inst)
	if err != nil {
		return manifest, recall.Health{Status: recall.HealthUnavailable}, err
	}
	adp, err := r.registry.Adapter(inst)
	if err != nil {
		return manifest, recall.Health{Status: recall.HealthUnavailable}, err
	}
	health, err := adp.Health(ctx)
	return manifest, health, err
}

// probeTimeout bounds a source that declares no timeout of its own. A probe
// that hangs would make `recall doctor` the thing that needs diagnosing.
const probeTimeout = 10 * time.Second
