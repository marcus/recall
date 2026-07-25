package conformance_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcus/recall/internal/conformance"
)

// The reference suite is cmd/recall-stream's, recorded against that binary in a
// real process. Replaying it here is the acceptance bar for this package: the
// harness and the recorder are separate implementations of one format, and the
// only honest check that they agree is running one against the other's output.
const (
	referenceSuite = "../../cmd/recall-stream/conformance"
	referenceCases = 9
)

// binPath is the reference adapter, built once.
var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "recall-conformance-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance: temp dir:", err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "recall-stream")
	build := exec.Command("go", "build", "-o", binPath, "../../cmd/recall-stream")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "conformance: build recall-stream:", err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestVerifyReplaysTheReferenceSuite(t *testing.T) {
	results, err := conformance.Verify(t.Context(), referenceSuite, conformance.Command(binPath), conformance.Options{})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(results) != referenceCases {
		t.Fatalf("replayed %d cases, the suite has %d", len(results), referenceCases)
	}
	for _, res := range results {
		if !res.OK() {
			t.Errorf("%s", res.Report())
		}
	}
}

// TestReplayNamesTheMutatedField is the negative half of the acceptance bar. A
// harness that reported green for a real adapter proves nothing until it also
// reports red, at the right pointer, for one that answers almost correctly.
func TestReplayNamesTheMutatedField(t *testing.T) {
	tr := loadCase(t, "handshake")
	target := mutating(conformance.Command(binPath), 1, "adapter_id", "recall-stream/9")

	res, err := conformance.Replay(t.Context(), tr, target, conformance.Options{})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if res.OK() {
		t.Fatal("a mutated response replayed clean")
	}
	if len(res.Differences) != 1 {
		t.Fatalf("expected the one mutated field, got:\n%s", res.Report())
	}
	d := res.Differences[0]
	if d.Case != "handshake" || d.Pointer != "/result/adapter_id" {
		t.Errorf("difference = %s, want case handshake at /result/adapter_id", d)
	}
	// The report is what a person reads, so the case and the pointer have to
	// survive into it and not only into the struct.
	report := res.Report()
	for _, part := range []string{"handshake", "/result/adapter_id", "recall-stream/9"} {
		if !strings.Contains(report, part) {
			t.Errorf("report does not mention %q:\n%s", part, report)
		}
	}
}

// TestReplayFailsOnTheResponseCount covers the case the declared count exists
// for. An adapter that stops answering matches every frame it did write, so
// without the count a short exchange would pass.
func TestReplayFailsOnTheResponseCount(t *testing.T) {
	tr := loadCase(t, "handshake") // two responses: the manifest, then health
	target := truncating(conformance.Command(binPath), 1)

	res, err := conformance.Replay(t.Context(), tr, target, conformance.Options{})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if res.OK() {
		t.Fatal("an adapter that stopped answering replayed clean")
	}
	if len(res.Responses) != 1 {
		t.Fatalf("expected one response, got %d", len(res.Responses))
	}
	// The count is the first thing reported, so the failure reads as "it stopped
	// answering" rather than as a pile of missing frames.
	first := res.Differences[0]
	if !strings.Contains(first.Detail, "declares 2 responses") || !strings.Contains(first.Detail, "produced 1") {
		t.Errorf("first difference = %s, want the declared count", first)
	}
	if res.Stopped == "" || !strings.Contains(first.Detail, res.Stopped) {
		t.Errorf("the count difference does not carry why the replay stopped: %q", res.Stopped)
	}
	if first.Case != "handshake" {
		t.Errorf("difference names case %q", first.Case)
	}
}

// TestReplayFailsWhenTheAdapterStopsWithoutClosing is the other way an adapter
// stops answering: it stays alive and says nothing. That must end in the same
// verdict rather than in a hung suite.
func TestReplayFailsWhenTheAdapterStopsWithoutClosing(t *testing.T) {
	tr := loadCase(t, "handshake")
	target := silentAfter(conformance.Command(binPath), 1)

	res, err := conformance.Replay(t.Context(), tr, target, conformance.Options{
		ResponseTimeout: 500 * time.Millisecond,
		DrainTimeout:    200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if res.OK() {
		t.Fatal("a silent adapter replayed clean")
	}
	if !strings.Contains(res.Stopped, "no response within") {
		t.Errorf("stopped = %q, want the response timeout", res.Stopped)
	}
	if !strings.Contains(res.Differences[0].Detail, "declares 2 responses") {
		t.Errorf("first difference = %s, want the declared count", res.Differences[0])
	}
}

// TestReplayGivesEachCaseAFreshWorkdir checks the binding the format calls for.
// Reusing a workdir would let a second replay observe a warm index, which is
// state the recording never had.
func TestReplayGivesEachCaseAFreshWorkdir(t *testing.T) {
	tr := loadCase(t, "handshake")
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		target := observing(conformance.Command(binPath), func(workdir string) {
			if seen[workdir] {
				t.Errorf("workdir %q was handed to two replays", workdir)
			}
			seen[workdir] = true
		})
		res, err := conformance.Replay(t.Context(), tr, target, conformance.Options{})
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if !res.OK() {
			t.Fatalf("%s", res.Report())
		}
	}
	if len(seen) != 2 {
		t.Fatalf("observed %d workdirs, want 2", len(seen))
	}
}

func loadCase(t *testing.T, name string) *conformance.Transcript {
	t.Helper()
	tr, err := conformance.Load(filepath.Join(referenceSuite, name))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return tr
}

// filtering wraps a target's stdout, letting a test corrupt what a real adapter
// says. Running the real binary and changing one thing about its answers is the
// only way to check the diff against a transcript that was genuinely recorded.
func filtering(base conformance.Target, edit func(n int, line []byte) ([]byte, bool)) conformance.Target {
	return func(ctx context.Context) (*conformance.Process, error) {
		proc, err := base(ctx)
		if err != nil {
			return nil, err
		}
		pipeR, pipeW := io.Pipe()
		go func() {
			reader := bufio.NewReader(proc.Stdout)
			n := 0
			for {
				line, err := reader.ReadBytes('\n')
				if len(bytes.TrimSpace(line)) > 0 {
					n++
					out, keep := edit(n, line)
					if !keep {
						_ = pipeW.Close()
						return
					}
					if _, werr := pipeW.Write(out); werr != nil {
						return
					}
				}
				if err != nil {
					_ = pipeW.Close()
					return
				}
			}
		}()
		return &conformance.Process{
			Stdin:  proc.Stdin,
			Stdout: pipeR,
			Stderr: proc.Stderr,
			Stop: func() {
				if proc.Stop != nil {
					proc.Stop()
				}
				_ = pipeR.Close()
			},
		}, nil
	}
}

// mutating rewrites one member of one response's result.
func mutating(base conformance.Target, nth int, member string, value any) conformance.Target {
	return filtering(base, func(n int, line []byte) ([]byte, bool) {
		if n != nth {
			return line, true
		}
		var frame map[string]any
		if err := json.Unmarshal(line, &frame); err != nil {
			return line, true
		}
		if result, ok := frame["result"].(map[string]any); ok {
			result[member] = value
		}
		edited, err := json.Marshal(frame)
		if err != nil {
			return line, true
		}
		return append(edited, '\n'), true
	})
}

// truncating closes stdout after the nth frame: an adapter that stops answering
// and hangs up.
func truncating(base conformance.Target, after int) conformance.Target {
	return filtering(base, func(n int, line []byte) ([]byte, bool) {
		return line, n <= after
	})
}

// silentAfter passes the first frames through and then swallows everything,
// leaving the stream open: an adapter that stops answering without hanging up.
func silentAfter(base conformance.Target, after int) conformance.Target {
	return filtering(base, func(n int, line []byte) ([]byte, bool) {
		if n > after {
			return nil, true
		}
		return line, true
	})
}

// observing reports the workdir the harness bound for this replay, read out of
// the initialize request as it goes past.
func observing(base conformance.Target, report func(workdir string)) conformance.Target {
	return func(ctx context.Context) (*conformance.Process, error) {
		proc, err := base(ctx)
		if err != nil {
			return nil, err
		}
		return &conformance.Process{
			Stdin:  &watchingWriter{to: proc.Stdin, report: report},
			Stdout: proc.Stdout,
			Stderr: proc.Stderr,
			Stop:   proc.Stop,
		}, nil
	}
}

type watchingWriter struct {
	to     io.WriteCloser
	report func(string)
	done   bool
}

func (w *watchingWriter) Write(p []byte) (int, error) {
	if !w.done {
		var frame struct {
			Params struct {
				Workdir string `json:"workdir"`
			} `json:"params"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(p), &frame); err == nil && frame.Params.Workdir != "" {
			w.done = true
			w.report(frame.Params.Workdir)
		}
	}
	return w.to.Write(p)
}

func (w *watchingWriter) Close() error { return w.to.Close() }
