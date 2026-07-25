package app

import (
	"maps"
	"slices"
	"time"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/internal/ranking"
	"github.com/marcus/recall/internal/recall"
	"github.com/marcus/recall/internal/source"
)

// BuildOptions assemble the core from configuration.
type BuildOptions struct {
	Config   *config.Config
	Builtins map[string]source.Factory
	StateDir string

	// Limit is the default result cap. A request may override it.
	Limit int

	Now func() time.Time
}

// Build wires configuration into a running core: registry, ranker, app.
//
// It exists so no transport has to do this itself. The CLI, the HTTP API, and
// the MCP server must produce identical results for identical requests, and the
// surest way to break that is to have each one map configuration into ranking
// separately — priors and intent classes translated three times, diverging the
// first time one is edited. The spec forbids a surface acquiring its own ranking
// behavior; this is what makes that structurally true rather than a rule to
// remember.
func Build(opt BuildOptions) (*App, *source.Registry, error) {
	registry := source.NewRegistry(opt.Config, source.Options{
		Builtins: opt.Builtins,
		StateDir: opt.StateDir,
	})

	ranker, err := ranking.New(RankingConfig(opt.Config, opt.Limit))
	if err != nil {
		return nil, nil, err
	}

	return New(Options{
		Config:   opt.Config,
		Registry: registry,
		Ranker:   ranker,
		Now:      opt.Now,
	}), registry, nil
}

// RankingConfig maps configuration onto fusion.
//
// Exported because evaluation builds a core the same way and must get the same
// mapping; it is the one translation that decides how every source is weighed.
func RankingConfig(cfg *config.Config, limit int) ranking.Config {
	sources := make(map[recall.SourceUID]ranking.SourceConfig, len(cfg.Sources))
	for _, s := range cfg.Sources {
		sc := ranking.SourceConfig{SourceID: s.ID, BasePrior: s.BasePrior}
		// Sorted, so two runs over one configuration configure fusion
		// identically. A map's iteration order is not a fact about the file.
		for _, class := range slices.Sorted(maps.Keys(s.IntentPriors)) {
			sc.IntentPriors = append(sc.IntentPriors, ranking.IntentPrior{
				QueryClass: class,
				Effective:  s.IntentPriors[class],
			})
		}
		sources[s.UID] = sc
	}
	return ranking.Config{Sources: sources, Limit: limit}
}

// Locations reports where every configured source reads from, for an
// evaluation run's network policy check.
func Locations(cfg *config.Config) []SourceLocation {
	out := make([]SourceLocation, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		out = append(out, SourceLocation{SourceID: s.ID, Location: s.Location})
	}
	return out
}

// SourceLocation mirrors eval.SourceLocation without importing it: the
// application layer does not depend on the harness that measures it.
type SourceLocation struct {
	SourceID string
	Location string
}
