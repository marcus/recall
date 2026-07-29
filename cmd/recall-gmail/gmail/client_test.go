package gmail

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/marcus/recall/pkg/protocol"
)

func TestLiveRunnerAllowsOnlyReadCommandsAndAddsSafetyFlags(t *testing.T) {
	runner := &liveRunner{binary: "gog", account: "owner@example.test", timeout: time.Second}
	for _, args := range [][]string{
		{"gmail", "send", "--to", "someone@example.test"},
		{"gmail", "archive", "abc"},
	} {
		if _, err := runner.argv(args, ""); !errors.Is(err, protocol.ErrSourceUnavailable) {
			t.Fatalf("argv(%v) error = %v", args, err)
		}
	}
	argv, err := runner.argv([]string{"gmail", "search", "--max", "5"}, "-in:spam x")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--gmail-no-send", "--no-input", "-j"} {
		if !contains(argv, want) {
			t.Errorf("argv %v missing %q", argv, want)
		}
	}
	if got := argv[len(argv)-2:]; !reflect.DeepEqual(got, []string{"--", "-in:spam x"}) {
		t.Fatalf("argv tail = %v", got)
	}
}

func TestGogFailuresClassifyDeniedAndUnavailable(t *testing.T) {
	if err := classifyGog(4, "No auth for gmail"); !errors.Is(err, protocol.ErrSourceDenied) {
		t.Fatalf("denied error = %v", err)
	}
	if err := classifyGog(2, "network is down"); !errors.Is(err, protocol.ErrSourceUnavailable) {
		t.Fatalf("unavailable error = %v", err)
	}
}

func TestReplayRunnerReadsSuccessAndFailureFixtures(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "clock.json"),
		[]byte(`{"now":"2026-07-25T16:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gmail-search.0.json"),
		[]byte(`{"threads":[{"id":"thr_1"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gmail-probe.4.json"),
		[]byte(`No auth for gmail`), 0o644); err != nil {
		t.Fatal(err)
	}
	runner, err := newReplayRunner(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got searchPayload
	if err := runner.Run(t.Context(), "gmail-search", nil, "", &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Threads) != 1 || got.Threads[0].ID != "thr_1" {
		t.Fatalf("payload = %+v", got)
	}
	if err := runner.Run(t.Context(), "gmail-probe", nil, "", &got); !errors.Is(err, protocol.ErrSourceDenied) {
		t.Fatalf("failure = %v", err)
	}
	if now, ok := runner.Now(); !ok || !now.Equal(testNow) {
		t.Fatalf("clock = %s, ok = %v", now, ok)
	}
}
