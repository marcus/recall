package source

import (
	"testing"
	"time"

	"github.com/marcus/recall/internal/config"
	"github.com/marcus/recall/pkg/adapter"
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

// #13, #14, #15: the plan-time probe gets the source's own timeout, never less
// than the old fixed probe limit, never more than DefaultProbeCeiling unless the
// source sets probe_timeout_ms, and never more than the request has left.
func TestProbeDeadline(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000, 0)
	tests := []struct {
		name     string
		usable   time.Duration
		timeout  time.Duration
		explicit time.Duration
		want     time.Duration
	}{
		{"source timeout between floor and ceiling", 60 * time.Second, 8 * time.Second, 0, 8 * time.Second},
		{"long source timeout stops at the ceiling", 60 * time.Second, 180 * time.Second, 0, DefaultProbeCeiling},
		{"short inherited timeout keeps the old floor", 60 * time.Second, 2 * time.Second, 0, adapter.DefaultProbeTimeout},
		{"request budget caps the ceiling", 8 * time.Second, 180 * time.Second, 0, 8 * time.Second},
		{"request budget caps the floor too", time.Second, 2 * time.Second, 0, time.Second},
		{"explicit probe timeout passes the ceiling", 60 * time.Second, 180 * time.Second, 30 * time.Second, 30 * time.Second},
		{"explicit probe timeout may be below the floor", 60 * time.Second, 15 * time.Second, time.Second, time.Second},
		{"request budget caps an explicit probe timeout", 8 * time.Second, 180 * time.Second, 30 * time.Second, 8 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := probeDeadline(now, now.Add(tt.usable), tt.timeout, tt.explicit).Sub(now)
			if got != tt.want {
				t.Fatalf("probeDeadline(usable %s, timeout %s, explicit %s) = %s, want %s",
					tt.usable, tt.timeout, tt.explicit, got, tt.want)
			}
		})
	}
}
