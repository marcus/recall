package qmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type derivedLockKey struct{}

// A stable sibling inode coordinates all profiles and adapter processes. Never
// unlink it: replacing a locked inode lets a second writer bypass its owner.
// Refresh owns the entire verification/invalidation/mirror/qmd/receipt sequence;
// readers hold a shared lock through validation and result mapping. Corpus edits
// outside Recall are still detected by the before/after content checks.
func (a *Adapter) lockDerived(ctx context.Context, exclusive bool) (context.Context, func(), error) {
	if err := a.validateMirror(); err != nil {
		return ctx, nil, err
	}
	file := a.paths().Receipt + ".lock"
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return ctx, nil, err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return ctx, nil, err
	}
	operation := syscall.LOCK_SH
	if exclusive {
		operation = syscall.LOCK_EX
	}
	for {
		if err = ctx.Err(); err != nil {
			_ = f.Close()
			return ctx, nil, err
		}
		err = syscall.Flock(int(f.Fd()), operation|syscall.LOCK_NB)
		if err == nil {
			return context.WithValue(ctx, derivedLockKey{}, f), func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return ctx, nil, fmt.Errorf("qmd derived lock: %w", err)
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}
