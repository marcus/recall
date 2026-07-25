package conformance_test

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/marcus/recall/internal/conformance"
)

// TestPlayerAnswersFromTheRecording is the evaluation half of the engine. A
// fixture run replays recorded adapter responses instead of running a live
// source, and the check that it replays them faithfully is the same [Compare]
// the conformance side uses.
func TestPlayerAnswersFromTheRecording(t *testing.T) {
	for _, name := range []string{"handshake", "search-ranked", "expand-details", "cancel-inflight"} {
		t.Run(name, func(t *testing.T) {
			tr := loadCase(t, name)
			player, err := conformance.NewPlayer(tr)
			if err != nil {
				t.Fatalf("new player: %v", err)
			}

			requests, err := tr.Bind(conformance.Bindings{Workdir: t.TempDir()})
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			got := playThrough(t, player, requests)

			// Nothing is masked: a fixture run is deterministic precisely
			// because the recorded timestamps come back unchanged.
			if diffs := conformance.Compare(name, tr.Recorded, got, nil); len(diffs) != 0 {
				t.Fatalf("playback differs from the recording:\n%s", render(diffs))
			}
		})
	}
}

func TestPlayerEchoesTheLiveRequestID(t *testing.T) {
	// The runner numbers its own requests, and a recording's ids are an
	// artifact of the run that produced it. Answering with the recorded id
	// would correlate every response to the wrong request.
	tr := loadCase(t, "handshake")
	player, err := conformance.NewPlayer(tr)
	if err != nil {
		t.Fatalf("new player: %v", err)
	}
	got := playThrough(t, player, [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":"live-a","method":"recall/initialize","params":{}}`),
		[]byte(`{"jsonrpc":"2.0","id":"live-b","method":"recall/health","params":{}}`),
	})
	if len(got) != 2 {
		t.Fatalf("got %d responses, want 2", len(got))
	}
	for i, want := range []string{"live-a", "live-b"} {
		var frame struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(got[i], &frame); err != nil {
			t.Fatalf("response %d: %v", i+1, err)
		}
		if frame.ID != want {
			t.Errorf("response %d id = %q, want %q", i+1, frame.ID, want)
		}
	}
}

func TestPlayerRefusesAnUnrecordedRequest(t *testing.T) {
	// A source that hands back evidence for a question nobody asked is worse
	// than a source that is unavailable, so divergence from the recorded order
	// ends the playback rather than being answered from the nearest frame.
	tr := loadCase(t, "handshake")
	player, err := conformance.NewPlayer(tr)
	if err != nil {
		t.Fatalf("new player: %v", err)
	}

	in, inWriter := io.Pipe()
	outReader, out := io.Pipe()
	errs := make(chan error, 1)
	go func() { errs <- player.Serve(t.Context(), in, out); _ = out.Close() }()
	go func() { _, _ = io.Copy(io.Discard, outReader) }()

	if _, err := inWriter.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"recall/search","params":{}}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = inWriter.Close()

	err = <-errs
	if err == nil {
		t.Fatal("expected a refusal")
	}
	for _, part := range []string{"handshake", "recall/search", "recall/initialize"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error %q does not mention %q", err, part)
		}
	}
}

func TestNewPlayerRejectsAnUncorrelatedRecording(t *testing.T) {
	// A transcript missing an answer is a defect in the pack, and the run that
	// discovers it should never have started.
	dir := writeCase(t, "synthetic",
		`{"case":"synthetic","description":"d","flow":"lockstep","responses":1}`,
		[]string{
			`{"jsonrpc":"2.0","id":1,"method":"recall/health","params":{}}`,
			`{"jsonrpc":"2.0","id":2,"method":"recall/search","params":{}}`,
		},
		[]string{`{"jsonrpc":"2.0","id":1,"result":{}}`})
	tr, err := conformance.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := conformance.NewPlayer(tr); err == nil || !strings.Contains(err.Error(), "request id 2") {
		t.Fatalf("error = %v, want one naming the unanswered request", err)
	}
}

// playThrough drives a player over a pipe and returns the frames it wrote.
func playThrough(t *testing.T, player *conformance.Player, requests [][]byte) [][]byte {
	t.Helper()

	in, inWriter := io.Pipe()
	outReader, out := io.Pipe()
	errs := make(chan error, 1)
	go func() {
		err := player.Serve(t.Context(), in, out)
		_ = out.CloseWithError(err)
		errs <- err
	}()

	frames := make(chan []byte, len(requests)+1)
	go func() {
		defer close(frames)
		reader := bufio.NewReader(outReader)
		for {
			line, err := reader.ReadBytes('\n')
			if trimmed := strings.TrimSpace(string(line)); trimmed != "" {
				frames <- []byte(trimmed)
			}
			if err != nil {
				return
			}
		}
	}()

	for i, req := range requests {
		if _, err := inWriter.Write(append(append([]byte{}, req...), '\n')); err != nil {
			t.Fatalf("write request %d: %v", i+1, err)
		}
	}
	_ = inWriter.Close()
	if err := <-errs; err != nil {
		t.Fatalf("serve: %v", err)
	}

	var out2 [][]byte
	for frame := range frames {
		out2 = append(out2, frame)
	}
	return out2
}
