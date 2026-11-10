package adapter_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marcus/recall/pkg/adapter"
)

func wedged(ctx context.Context) (struct{}, error) {
	<-ctx.Done()
	return struct{}{}, ctx.Err()
}

// #17. Each case names which bound may end the call and whether the error
// must say it was the adapter's own per-call limit.
func TestBoundCall(t *testing.T) {
	for _, tc := range []struct {
		name       string
		caller     time.Duration // 0: no caller deadline
		configured time.Duration
		fallback   time.Duration
		wantCall   bool // a *CallTimeoutError naming the per-call limit
	}{
		{"configured limit fires first", time.Minute, 20 * time.Millisecond, time.Minute, true},
		{"sooner caller deadline wins over configured", 20 * time.Millisecond, time.Minute, time.Minute, false},
		{"caller deadline replaces the default", 20 * time.Millisecond, 0, time.Millisecond, false},
		{"default bounds a caller with no deadline", 0, 0, 20 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			if tc.caller > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.caller)
				defer cancel()
			}
			_, err := adapter.BoundCall(ctx, "td info", "settings.timeout_ms", tc.configured, tc.fallback, wedged)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want a deadline", err)
			}
			var call *adapter.CallTimeoutError
			if got := errors.As(err, &call); got != tc.wantCall {
				t.Fatalf("per-call timeout reported = %t, want %t (%v)", got, tc.wantCall, err)
			}
			if tc.wantCall && (call.Call != "td info" || call.Setting != "settings.timeout_ms" || call.Limit != 20*time.Millisecond) {
				t.Errorf("error = %+v, want the call, its 20ms limit, and its setting", call)
			}
		})
	}
}
