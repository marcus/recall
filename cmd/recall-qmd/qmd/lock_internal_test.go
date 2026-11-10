package qmd

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcus/recall/pkg/adapter"
)

func lockAdapter(t *testing.T, root string) *Adapter {
	t.Helper()
	a := New(Options{})
	_, err := a.Initialize(t.Context(), adapter.Config{ProtocolVersionMin: 1, ProtocolVersionMax: 1, Location: root, Workdir: filepath.Join(root, "unused"), SharedWorkdir: root + "-state", Settings: map[string]any{"collection": "fixture", "derive_extensions": []string{".html"}, "derived_collection": "html"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}
func TestDerivedLockReadersAndCancelledWriter(t *testing.T) {
	root := t.TempDir()
	a := lockAdapter(t, root)
	b := lockAdapter(t, root)
	_, release, err := a.lockDerived(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, release2, err := b.lockDerived(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Millisecond)
	defer cancel()
	if _, _, err := b.lockDerived(ctx, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writer bypassed readers: %v", err)
	}
	release2()
	// Closing a waiting descriptor must not disturb an existing reader.
	ctx2, cancel2 := context.WithTimeout(t.Context(), 35*time.Millisecond)
	defer cancel2()
	if _, _, err := b.lockDerived(ctx2, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reader lock lost: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root + "-state") })
}

func TestDerivedLockProcess(t *testing.T) {
	mode := os.Getenv("RECALL_LOCK_HELPER")
	if mode == "" {
		return
	}
	root := os.Getenv("RECALL_LOCK_ROOT")
	if mode == "engine" {
		os.WriteFile(filepath.Join(root, "engine"), []byte(strconv.Itoa(os.Getpid())), 0600)
		for {
			if _, err := os.Stat(filepath.Join(root, "release")); err == nil {
				os.Exit(0)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	a := lockAdapter(t, root)
	ctx, release, err := a.lockDerived(context.Background(), true)
	if err != nil {
		os.Exit(2)
	}
	defer release()
	if err := a.saveReceipt(mirrorReceipt{Version: 1, Corpus: root, Failure: "refresh did not complete"}); err != nil {
		os.Exit(3)
	}
	exe, _ := os.Executable()
	env := append(os.Environ(), "RECALL_LOCK_HELPER=engine")
	_, err = (ExecRunner{Binary: exe, Env: env}).Run(ctx, "-test.run=^TestDerivedLockProcess$")
	if err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}
func TestDerivedLockSurvivesAdapterCrashUntilEngineExits(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(func() { os.RemoveAll(root + "-state") })
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestDerivedLockProcess$")
	cmd.Env = append(os.Environ(), "RECALL_LOCK_HELPER=owner", "RECALL_LOCK_ROOT="+root)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	deadline := time.Now().Add(3 * time.Second)
	var pid int
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(root, "engine"))
		if err == nil {
			pid, _ = strconv.Atoi(string(data))
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("engine did not start")
	}
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	a := lockAdapter(t, root)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := a.lockDerived(ctx, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("orphan engine lost inherited lock: %v", err)
	}
	r, err := a.loadReceipt()
	if err != nil || r.Ready {
		t.Fatalf("crash left ready receipt: %+v %v", r, err)
	}
	os.WriteFile(filepath.Join(root, "release"), nil, 0600)
	ctx2, cancel2 := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel2()
	_, release, err := a.lockDerived(ctx2, true)
	if err != nil {
		t.Fatalf("crashed owner permanently locked source: %v", err)
	}
	release()
}
func TestCollectionMismatchKeepsLongFullPaths(t *testing.T) {
	got := "/old/profile/" + strings.Repeat("directory/", 80) + "uid"
	want := "/shared/" + strings.Repeat("directory/", 80) + "uid"
	err := checkCollectionPath(collectionReport{Name: "html", Path: got}, want)
	message := safeErr(err)
	if !strings.Contains(message, got) || !strings.Contains(message, want) {
		t.Fatalf("full paths clipped: %s", message)
	}
}

func TestExecRunnerPassesEveryHeldLock(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	for _, name := range []string{"a.lock", "b.lock"} {
		f, err := acquireFlock(t.Context(), filepath.Join(dir, name), true)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		ctx = withHeldLock(ctx, f)
	}
	res, err := (ExecRunner{Binary: "/bin/sh"}).Run(ctx, "-c", "[ -e /dev/fd/3 ] && [ -e /dev/fd/4 ]")
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("child did not inherit both locks: exit %d, %v", res.ExitCode, err)
	}
}
