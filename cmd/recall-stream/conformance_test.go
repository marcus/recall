package main_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcus/recall/internal/conformance"
)

// The conformance suite drives the real binary in a real process.
//
// docs/adapter-protocol.md#conformance calls for recorded transcripts, and a
// transcript recorded against an in-process fixture would prove the fixture.
// Every case here starts cmd/recall-stream, writes request.jsonl to its stdin,
// and compares what comes back on its stdout with response.jsonl. Rerecord
// with:
//
//	go test ./cmd/recall-stream -run TestConformance -record
//
// See conformance/FORMAT.md for the transcript format the replay harness
// consumes.
var rerecord = flag.Bool("record", false, "rewrite each case's response.jsonl from a live run")

// binPath is the adapter binary under test, built once in TestMain.
var binPath string

// responseTimeout bounds the wait for one reply. It is generous: a case that
// hits it has hung, and the difference between two seconds and ten does not
// change that.
const responseTimeout = 20 * time.Second

func TestMain(m *testing.M) {
	flag.Parse()
	dir, err := os.MkdirTemp("", "recall-stream-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance: temp dir:", err)
		os.Exit(1)
	}
	binPath = filepath.Join(dir, "recall-stream")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "conformance: build:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// caseManifest is conformance/<case>/manifest.json. The replay harness reads
// the same fields; FORMAT.md is its specification.
type caseManifest struct {
	Case         string            `json:"case"`
	Description  string            `json:"description"`
	Flow         string            `json:"flow"`
	Placeholders map[string]string `json:"placeholders"`
	Volatile     []string          `json:"volatile"`
	Responses    int               `json:"responses"`
}

func TestConformance(t *testing.T) {
	entries, err := os.ReadDir("conformance")
	if err != nil {
		t.Fatalf("read conformance directory: %v", err)
	}
	cases := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		cases++
		t.Run(entry.Name(), func(t *testing.T) {
			runCase(t, filepath.Join("conformance", entry.Name()))
		})
	}
	// The eight required cases in docs/adapter-protocol.md#conformance, with
	// the handshake's two halves and shutdown recorded separately. A suite that
	// quietly lost a directory would still pass every case it kept.
	if cases != 9 {
		t.Fatalf("expected 9 conformance cases, found %d", cases)
	}
}

func runCase(t *testing.T, dir string) {
	t.Helper()

	man := readManifest(t, dir)
	if man.Flow != "lockstep" {
		t.Fatalf("unknown flow %q; FORMAT.md defines only lockstep", man.Flow)
	}
	requests := readLines(t, filepath.Join(dir, "request.jsonl"))

	fixture, err := filepath.Abs(filepath.Join(dir, "fixture"))
	if err != nil {
		t.Fatalf("resolve fixture: %v", err)
	}
	replacer := strings.NewReplacer("${FIXTURE}", jsonPath(fixture), "${WORKDIR}", jsonPath(t.TempDir()))
	for i, line := range requests {
		requests[i] = []byte(replacer.Replace(string(line)))
	}

	got, stderr := drive(t, requests)
	t.Logf("adapter stderr:\n%s", stderr)

	if man.Responses != len(got) {
		t.Fatalf("manifest declares %d responses, the run produced %d", man.Responses, len(got))
	}
	path := filepath.Join(dir, "response.jsonl")
	if *rerecord {
		// Redacted, not verbatim: a transcript must not commit the recording
		// machine's clock or paths under fields nothing compares.
		body := bytes.Join(append(conformance.Redact(got, man.Volatile), nil), []byte("\n"))
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		t.Logf("recorded %d responses into %s", len(got), path)
		return
	}

	want := readLines(t, path)
	if len(want) != len(got) {
		t.Fatalf("recorded %d responses, replay produced %d", len(want), len(got))
	}
	for i := range want {
		w := normalize(t, want[i], man.Volatile)
		g := normalize(t, got[i], man.Volatile)
		if !reflect.DeepEqual(w, g) {
			t.Errorf("response %d differs\n want: %s\n  got: %s", i+1, want[i], got[i])
		}
	}
}

// drive runs one transcript against a live process.
//
// The flow rule is the one FORMAT.md states: lines go out in order, a request
// waits for its own response before the next request is sent, and a
// notification is sent without waiting. Cancellation depends on the second
// half — the cancel notification has to reach a request that is still running.
func drive(t *testing.T, requests [][]byte) ([][]byte, string) {
	t.Helper()

	cmd := exec.Command(binPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	// exec copies stderr on its own goroutine, which outlives the process. The
	// buffer is read while a case is still running, so it has to be guarded.
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_, _ = cmd.Process.Wait()
	}()

	lines := make(chan []byte, 16)
	readErr := make(chan error, 1)
	go func() {
		defer close(lines)
		reader := bufio.NewReader(stdout)
		for {
			line, err := reader.ReadBytes('\n')
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				lines <- trimmed
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					readErr <- err
				}
				return
			}
		}
	}()

	var (
		got     [][]byte
		pending bool
	)
	await := func() {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("adapter closed stdout with a request outstanding\nstderr:\n%s", stderr.String())
			}
			got = append(got, line)
		case err := <-readErr:
			t.Fatalf("read from adapter: %v", err)
		case <-time.After(responseTimeout):
			t.Fatalf("no response within %s\nstderr:\n%s", responseTimeout, stderr.String())
		}
	}

	for _, req := range requests {
		notification := isNotification(t, req)
		if pending && !notification {
			await()
			pending = false
		}
		if _, err := stdin.Write(append(append([]byte{}, req...), '\n')); err != nil {
			t.Fatalf("write request: %v", err)
		}
		pending = pending || !notification
	}
	if pending {
		await()
	}

	// A clean shutdown closes stdout. Draining it proves nothing extra was
	// written after the last expected reply.
	_ = stdin.Close()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return got, stderr.String()
			}
			got = append(got, line)
		case <-time.After(2 * time.Second):
			return got, stderr.String()
		}
	}
}

// syncBuffer collects adapter stderr without racing exec's copier.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func isNotification(t *testing.T, line []byte) bool {
	t.Helper()
	var frame struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(line, &frame); err != nil {
		t.Fatalf("request line is not JSON: %v", err)
	}
	return frame.Method != "" && frame.ID == nil
}

// normalize decodes a frame and replaces every declared-volatile field with a
// constant, so a timestamp or a latency cannot fail a replay while a changed
// locator still does.
func normalize(t *testing.T, line []byte, volatile []string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(line, &v); err != nil {
		t.Fatalf("frame is not JSON: %v\n%s", err, line)
	}
	for _, path := range volatile {
		mask(v, strings.Split(strings.TrimPrefix(path, "/"), "/"))
	}
	return v
}

// mask walks a JSON-pointer-like path and blanks what it reaches. "*" matches
// every element of an array or every value of an object, which is what makes
// one declaration cover a whole candidate list.
func mask(v any, path []string) {
	if len(path) == 0 {
		return
	}
	head := path[0]
	switch node := v.(type) {
	case map[string]any:
		if head == "*" {
			for key := range node {
				step(node, key, path)
			}
			return
		}
		if _, ok := node[head]; ok {
			step(node, head, path)
		}
	case []any:
		if head != "*" {
			return
		}
		for _, item := range node {
			mask(item, path[1:])
		}
	}
}

func step(node map[string]any, key string, path []string) {
	if len(path) == 1 {
		node[key] = "<volatile>"
		return
	}
	mask(node[key], path[1:])
}

func readManifest(t *testing.T, dir string) caseManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var man caseManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}
	if man.Case != filepath.Base(dir) {
		t.Fatalf("manifest names case %q, directory is %q", man.Case, filepath.Base(dir))
	}
	if man.Description == "" {
		t.Fatal("manifest carries no description; a transcript nobody can read is not documentation")
	}
	return man
}

func readLines(t *testing.T, path string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			out = append(out, trimmed)
		}
	}
	return out
}

// jsonPath escapes a filesystem path for embedding in a JSON string. A temp
// directory rarely needs it; a case that fails only on a machine whose paths
// contain a backslash would be a miserable thing to debug.
func jsonPath(p string) string {
	encoded, err := json.Marshal(p)
	if err != nil {
		return p
	}
	return strings.Trim(string(encoded), `"`)
}
