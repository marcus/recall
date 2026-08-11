package source

import (
	"testing"
	"time"

	"github.com/marcus/recall/internal/config"
)

func TestResolveQueryBudget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		latencyMS int
		defaults  config.Defaults
		want      time.Duration
	}{
		{
			name: "unset request and unset defaults keep the 5s engine fallback",
			want: DefaultQueryBudget,
		},
		{
			name: "defaults.timeout_ms alone does not become the request budget",
			defaults: config.Defaults{
				Timeout: 15 * time.Second,
			},
			want: DefaultQueryBudget,
		},
		{
			name: "defaults.budget_ms supplies the request budget when the caller omits one",
			defaults: config.Defaults{
				Budget:  15 * time.Second,
				Timeout: 15 * time.Second,
			},
			want: 15 * time.Second,
		},
		{
			name:      "explicit LatencyMS wins over defaults.budget_ms",
			latencyMS: 20_000,
			defaults: config.Defaults{
				Budget: 15 * time.Second,
			},
			want: 20 * time.Second,
		},
		{
			name:      "zero LatencyMS is treated as unset, not a zero ceiling",
			latencyMS: 0,
			defaults: config.Defaults{
				Budget: 15 * time.Second,
			},
			want: 15 * time.Second,
		},
		{
			name: "zero defaults.Budget leaves the engine fallback",
			defaults: config.Defaults{
				Budget: 0,
			},
			want: DefaultQueryBudget,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveQueryBudget(tt.latencyMS, tt.defaults)
			if got != tt.want {
				t.Fatalf("resolveQueryBudget(%d, %+v) = %v, want %v",
					tt.latencyMS, tt.defaults, got, tt.want)
			}
		})
	}
}
