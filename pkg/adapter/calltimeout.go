package adapter

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CallTimeoutError reports that one subprocess call ran out the adapter's own
// per-call limit, not the deadline its caller set. It unwraps to the context
// error, so errors.Is(err, context.DeadlineExceeded) still holds and the core
// still reports a timeout. The message names the limit and the setting that
// controls it.
type CallTimeoutError struct {
	Call    string        // the command that ran out, e.g. "td info"
	Limit   time.Duration // the per-call limit that fired
	Setting string        // the setting that controls Limit, e.g. "settings.timeout_ms"
	Err     error
}

func (e *CallTimeoutError) Error() string {
	return fmt.Sprintf("%s ran out its per-call limit of %s (%s): %v", e.Call, e.Limit, e.Setting, e.Err)
}

func (e *CallTimeoutError) Unwrap() error { return e.Err }

// BoundCall runs call under a per-call limit composed with the caller's
// deadline; the sooner wins.
//
// A configured limit (> 0) always applies. Otherwise fallback applies only
// when ctx carries no deadline: a caller that set a deadline has already
// decided how long the call may take, and a hidden shorter default would make
// that caller's own timeout the wrong knob to turn (#17).
//
// When the per-call limit, and not the caller's deadline, ends the call, the
// error is wrapped in a [*CallTimeoutError].
func BoundCall[T any](
	ctx context.Context,
	name, setting string,
	configured, fallback time.Duration,
	call func(context.Context) (T, error),
) (T, error) {
	limit := configured
	if limit <= 0 {
		if _, ok := ctx.Deadline(); ok {
			return call(ctx)
		}
		limit = fallback
	}
	if limit <= 0 {
		return call(ctx)
	}
	bounded, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	out, err := call(bounded)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		err = &CallTimeoutError{Call: name, Limit: limit, Setting: setting, Err: err}
	}
	return out, err
}
