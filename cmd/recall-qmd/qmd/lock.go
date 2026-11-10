package qmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// heldLocksKey carries every lock file this operation holds, so a spawned qmd
// inherits all of them (see [ExecRunner.Run]).
type heldLocksKey struct{}

func withHeldLock(ctx context.Context, f *os.File) context.Context {
	held, _ := ctx.Value(heldLocksKey{}).([]*os.File)
	next := append(append([]*os.File(nil), held...), f)
	return context.WithValue(ctx, heldLocksKey{}, next)
}

func heldLocks(ctx context.Context) []*os.File {
	held, _ := ctx.Value(heldLocksKey{}).([]*os.File)
	return held
}

// acquireFlock opens file and takes an advisory lock on it, polling until ctx
// ends. The file is never unlinked: replacing a locked inode lets a second
// writer bypass its owner. Closing the returned file releases the lock.
func acquireFlock(ctx context.Context, file string, exclusive bool) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	operation := syscall.LOCK_SH
	if exclusive {
		operation = syscall.LOCK_EX
	}
	for {
		if err = ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err = syscall.Flock(int(f.Fd()), operation|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

// A stable sibling inode coordinates all profiles and adapter processes.
// Refresh owns the entire verification/invalidation/mirror/qmd/receipt sequence;
// readers hold a shared lock through validation and result mapping. Corpus edits
// outside Recall are still detected by the before/after content checks.
func (a *Adapter) lockDerived(ctx context.Context, exclusive bool) (context.Context, func(), error) {
	if err := a.validateMirror(); err != nil {
		return ctx, nil, err
	}
	f, err := acquireFlock(ctx, a.paths().Receipt+".lock", exclusive)
	if err != nil {
		if ctx.Err() != nil {
			return ctx, nil, err
		}
		return ctx, nil, fmt.Errorf("qmd derived lock: %w", err)
	}
	return withHeldLock(ctx, f), func() { _ = f.Close() }, nil
}

// LockDirEnv overrides where index maintenance locks live.
const LockDirEnv = "RECALL_QMD_LOCK_DIR"

// errUnknownIndex refuses maintenance when qmd did not say which index it
// opened. A fallback key would not meet the lock a sibling source took under
// the real index path, so the two could race again (#16).
var errUnknownIndex = errors.New("qmd status did not report the index path; refusing maintenance without the index lock")

// indexLockFile names the maintenance lock for one qmd index.
//
// `qmd update` rewrites every collection in the index it opens, and qmd embed
// writes index-wide tables, so two sources over one index refreshing at once
// race on the same SQLite rows (#16). The lock is keyed on the index file, not
// the source, so sources, profiles, and adapter processes sharing an index all
// meet on one inode. It lives outside qmd's directory because Recall does not
// own that directory, and outside per-source state because two sources do not
// share theirs.
func (a *Adapter) indexLockFile(indexPath string) (string, error) {
	dir := a.opts.LockDir
	if dir == "" {
		dir = os.Getenv(LockDirEnv)
	}
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(cache, "recall", "qmd-index-locks")
	}
	if indexPath == "" {
		return "", errUnknownIndex
	}
	sum := sha256.Sum256([]byte(canonical(indexPath)))
	return filepath.Join(dir, hex.EncodeToString(sum[:8])+".lock"), nil
}

// lockIndex takes the exclusive maintenance lock for one qmd index. Waiting is
// bounded by ctx. A running qmd invocation inherits the descriptor, so killing
// only its adapter does not admit another writer before that invocation exits.
//
// Lock order is fixed: a derived source lock, when held, is always taken
// before the index lock, never after it.
func (a *Adapter) lockIndex(ctx context.Context, indexPath string) (context.Context, func(), error) {
	file, err := a.indexLockFile(indexPath)
	if err != nil {
		return ctx, nil, fmt.Errorf("qmd index lock: %w", err)
	}
	f, err := acquireFlock(ctx, file, true)
	if err != nil {
		if ctx.Err() != nil {
			return ctx, nil, err
		}
		return ctx, nil, fmt.Errorf("qmd index lock: %w", err)
	}
	return withHeldLock(ctx, f), func() { _ = f.Close() }, nil
}

// resolveIndexPath asks qmd which index it opens. An unreadable answer is "",
// which [Adapter.lockIndex] refuses with [errUnknownIndex].
func (a *Adapter) resolveIndexPath(ctx context.Context) string {
	set, _, _, _, err := a.session()
	if err != nil {
		return ""
	}
	res, err := a.run(ctx, "status")
	if err != nil {
		return ""
	}
	text, err := decodeText(res, "status")
	if err != nil {
		return ""
	}
	return parseStatus(text, set.Collection).IndexPath
}
