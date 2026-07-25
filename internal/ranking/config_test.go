package ranking_test

import (
	"errors"
	"math"
	"testing"

	"github.com/marcus/recall/internal/ranking"
	"github.com/marcus/recall/internal/recall"
)

// A prior outside its range is a configuration defect, not a value to clamp.
// Clamping would let a machine rank differently from the configuration that was
// reviewed, and the difference would never appear in any explanation.
func TestConfigRejectsOutOfRangeValues(t *testing.T) {
	sources := func(sc ranking.SourceConfig) map[recall.SourceUID]ranking.SourceConfig {
		return map[recall.SourceUID]ranking.SourceConfig{"uid-tasks": sc}
	}

	cases := map[string]ranking.Config{
		"prior below the floor": {
			Sources: sources(ranking.SourceConfig{SourceID: "tasks", BasePrior: 0.4}),
		},
		"prior above the ceiling": {
			Sources: sources(ranking.SourceConfig{SourceID: "tasks", BasePrior: 2.1}),
		},
		"prior not a number": {
			Sources: sources(ranking.SourceConfig{SourceID: "tasks", BasePrior: math.NaN()}),
		},
		"intent prior leaves the range": {
			Sources: sources(ranking.SourceConfig{
				SourceID:     "tasks",
				BasePrior:    1.8,
				IntentPriors: []ranking.IntentPrior{{Rule: "boost", QueryClass: "task", Effective: 3.0}},
			}),
		},
		"intent prior names no rule": {
			Sources: sources(ranking.SourceConfig{
				SourceID:     "tasks",
				BasePrior:    1,
				IntentPriors: []ranking.IntentPrior{{QueryClass: "task", Effective: 1.5}},
			}),
		},
		"intent prior names no query class": {
			Sources: sources(ranking.SourceConfig{
				SourceID:     "tasks",
				BasePrior:    1,
				IntentPriors: []ranking.IntentPrior{{Rule: "boost", Effective: 1.5}},
			}),
		},
		"two rules for one query class": {
			Sources: sources(ranking.SourceConfig{
				SourceID:  "tasks",
				BasePrior: 1,
				IntentPriors: []ranking.IntentPrior{
					{Rule: "first", QueryClass: "task", Effective: 1.2},
					{Rule: "second", QueryClass: "task", Effective: 1.3},
				},
			}),
		},
		"negative rank constant": {
			RankConstant: -1,
			Sources:      sources(ranking.SourceConfig{SourceID: "tasks", BasePrior: 1}),
		},
		"corroboration cap below one": {
			CorroborationCap: 0.5,
			Sources:          sources(ranking.SourceConfig{SourceID: "tasks", BasePrior: 1}),
		},
		"negative limit": {
			Limit:   -1,
			Sources: sources(ranking.SourceConfig{SourceID: "tasks", BasePrior: 1}),
		},
		"negative per-source cap": {
			MaxPerSource: -1,
			Sources:      sources(ranking.SourceConfig{SourceID: "tasks", BasePrior: 1}),
		},
	}

	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ranking.New(cfg); !errors.Is(err, ranking.ErrConfig) {
				t.Fatalf("err = %v, want ErrConfig", err)
			}
		})
	}
}

// The defaults are the spec's, and an unset value means "the default", not
// "zero" — a rank constant of zero would make rank 1 twice as good as rank 2.
func TestConfigDefaults(t *testing.T) {
	r, err := ranking.New(ranking.Config{
		Sources: map[recall.SourceUID]ranking.SourceConfig{
			"uid-tasks": {SourceID: "tasks", BasePrior: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := r.Config()
	if got.RankConstant != ranking.DefaultRankConstant {
		t.Errorf("rank constant = %v, want %v", got.RankConstant, ranking.DefaultRankConstant)
	}
	if got.CorroborationCap != ranking.DefaultCorroborationCap {
		t.Errorf("corroboration cap = %v, want %v", got.CorroborationCap, ranking.DefaultCorroborationCap)
	}
	if ranking.MinPrior != 0.5 || ranking.MaxPrior != 2.0 {
		t.Errorf("prior range = [%v, %v], want [0.5, 2.0]", ranking.MinPrior, ranking.MaxPrior)
	}
}

// A ranker holds the configuration it validated. A caller that keeps mutating
// its own map must not be able to change how a request already in flight ranks.
func TestConfigIsCopiedOnConstruction(t *testing.T) {
	sources := map[recall.SourceUID]ranking.SourceConfig{
		"uid-tasks": {SourceID: "tasks", BasePrior: 1},
	}
	r, err := ranking.New(ranking.Config{Sources: sources})
	if err != nil {
		t.Fatal(err)
	}
	sources["uid-tasks"] = ranking.SourceConfig{SourceID: "tasks", BasePrior: 2}

	if got := r.Config().Sources["uid-tasks"].BasePrior; got != 1 {
		t.Errorf("base prior = %v, want the validated 1", got)
	}
}
