package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/marcus/recall/internal/protocol"
)

// Defaults for one replay.
const (
	// DefaultResponseTimeout bounds the wait for one reply. It is generous: a
	// case that hits it has hung, and the difference between two seconds and
	// twenty does not change that.
	DefaultResponseTimeout = 20 * time.Second

	// DefaultDrainTimeout bounds the read that follows closing stdin. A process
	// that exits closes stdout and the drain ends immediately; the timeout is
	// for one that does not.
	DefaultDrainTimeout = 2 * time.Second

	// DefaultStopGrace is how long a clean exit has after stdin closes, before
	// the harness kills the process itself.
	DefaultStopGrace = 2 * time.Second
)

// Process is one running adapter under replay.
//
// It is stdio and nothing else, because that is all the transport is: the
// harness writes request lines to Stdin, reads frames from Stdout, and never
// parses Stderr. Stderr and Stop may be nil for a target that has neither.
type Process struct {
	Stdin  io.WriteCloser
	Stdout io.Reader

	// Stderr returns whatever the adapter has logged so far. It is attached to
	// a failure report and never interpreted as protocol.
	Stderr func() string

	// Stop releases the process and must guarantee it is gone, and Stdout with
	// it, when it returns. The format lets a case assume the adapter exits on
	// its own after recall/shutdown or after stdin closes; a harness must still
	// kill one that does not, and a Stop that left Stdout open would strand the
	// reader waiting on it.
	Stop func()
}

// Target starts the adapter under test.
//
// It is a function rather than a command string so the same engine can drive a
// binary, a wrapper around one, or an in-process server. The evaluation runner
// needs the last of those; a conformance run uses [Command].
type Target func(ctx context.Context) (*Process, error)

// Command runs an adapter binary with no arguments beyond the ones given, which
// is what a case may assume about how it is started.
//
// The pipes are ours rather than exec's: exec's StdoutPipe closes on Wait, which
// races a reader still draining a frame. Owning the descriptors means the read
// side closes when the harness says so.
func Command(name string, args ...string) Target {
	return func(_ context.Context) (*Process, error) {
		inR, inW, err := os.Pipe()
		if err != nil {
			return nil, fmt.Errorf("conformance: stdin pipe: %w", err)
		}
		outR, outW, err := os.Pipe()
		if err != nil {
			closeAll(inR, inW)
			return nil, fmt.Errorf("conformance: stdout pipe: %w", err)
		}

		// stderr is free-form and is never parsed. exec copies it on a goroutine
		// that outlives the process, and the buffer is read while the case is
		// still running, so it has to be guarded.
		stderr := &syncBuffer{}
		cmd := exec.Command(name, args...) //nolint:gosec // the command names the adapter under test and comes from the caller
		cmd.Stdin = inR
		cmd.Stdout = outW
		cmd.Stderr = stderr
		if err := cmd.Start(); err != nil {
			closeAll(inR, inW, outR, outW)
			return nil, fmt.Errorf("conformance: start %s: %w", name, err)
		}
		// The child holds its own copies now. Dropping ours is what makes EOF
		// arrive when the child exits.
		closeAll(inR, outW)

		exited := make(chan struct{})
		go func() {
			defer close(exited)
			_ = cmd.Wait()
		}()

		var once sync.Once
		return &Process{
			Stdin:  inW,
			Stdout: outR,
			Stderr: stderr.String,
			Stop: func() {
				once.Do(func() {
					// Releasing stdin lets a polite adapter exit on its own; the
					// format says a case may assume it does. A harness must still
					// kill one that does not, and must not wait forever for a
					// process that ignored SIGKILL while blocked in I/O.
					_ = inW.Close()
					if !closedWithin(exited, DefaultStopGrace) {
						_ = cmd.Process.Kill()
						closedWithin(exited, DefaultStopGrace)
					}
					closeAll(outR)
				})
			},
		}, nil
	}
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// closedWithin reports whether c closed within d.
func closedWithin(c <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-c:
		return true
	case <-timer.C:
		return false
	}
}

// Options tune one replay. The zero value uses the defaults above and a fresh
// temporary workdir per case.
type Options struct {
	// Workdir is the parent of each case's ${WORKDIR}. A fresh empty directory
	// is created inside it per case and per run; an empty value uses the system
	// temporary directory. Nothing is reused between cases, because an adapter
	// that found a warm index would be replaying against state its recording
	// never had.
	Workdir string

	// KeepWorkdir leaves each case's workdir behind for inspection.
	KeepWorkdir bool

	ResponseTimeout time.Duration
	DrainTimeout    time.Duration
}

func (o Options) responseTimeout() time.Duration {
	if o.ResponseTimeout > 0 {
		return o.ResponseTimeout
	}
	return DefaultResponseTimeout
}

func (o Options) drainTimeout() time.Duration {
	if o.DrainTimeout > 0 {
		return o.DrainTimeout
	}
	return DefaultDrainTimeout
}

// Result is what one case did.
type Result struct {
	Case string
	Dir  string

	// Responses are the frames the adapter wrote, in the order it wrote them.
	Responses [][]byte

	// Volatile is the case's declared-volatile pointer list, carried here so a
	// recorder can mask them with [Redact] before committing a transcript
	// rather than writing a machine's clock and home directory into the tree.
	Volatile []string

	// Stderr is the adapter's free-form logging, captured for the report and
	// never parsed.
	Stderr string

	// Stopped says why the harness stopped driving before it had sent every
	// line, and is empty when it sent them all. An adapter that stops answering
	// still fails on the response count; this is the reason to print beside it.
	Stopped string

	Differences []Difference
}

// OK reports whether the case replayed exactly as recorded.
func (r *Result) OK() bool { return len(r.Differences) == 0 }

// reportLimit bounds how many differences a report prints. A frame that changed
// wholesale produces one difference per field, and a hundred lines of them
// hides the first one, which is usually the cause of the rest.
const reportLimit = 20

// Report renders the failure a person reads: the case, one line per differing
// pointer, and the adapter's stderr.
func (r *Result) Report() string {
	if r.OK() {
		return fmt.Sprintf("case %s: %d responses, as recorded", r.Case, len(r.Responses))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "case %s: %d differences", r.Case, len(r.Differences))
	if r.Stopped != "" {
		fmt.Fprintf(&b, " (%s)", r.Stopped)
	}
	for i, d := range r.Differences {
		if i == reportLimit {
			fmt.Fprintf(&b, "\n  … and %d more", len(r.Differences)-reportLimit)
			break
		}
		fmt.Fprintf(&b, "\n  %s", d)
	}
	if stderr := strings.TrimSpace(r.Stderr); stderr != "" {
		fmt.Fprintf(&b, "\n  adapter stderr:\n%s", indent(stderr, "    "))
	}
	return b.String()
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// Verify replays every case under root against target.
//
// It returns a result per case, in suite order, and an error only when the
// suite itself could not be run. A failed case is a [Result] that is not OK,
// not an error: a caller reporting conformance wants every case's verdict, not
// the first one that went wrong.
func Verify(ctx context.Context, root string, target Target, opts Options) ([]*Result, error) {
	suite, err := LoadSuite(root)
	if err != nil {
		return nil, err
	}
	results := make([]*Result, 0, len(suite))
	for _, tr := range suite {
		res, err := Replay(ctx, tr, target, opts)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

// Replay drives one transcript against target and diffs what came back.
//
// The error return is for a harness failure — a workdir that cannot be created,
// a process that cannot be started. Everything the adapter did wrong is in the
// result, because a conformance run reports on adapters and only fails on
// itself.
func Replay(ctx context.Context, tr *Transcript, target Target, opts Options) (*Result, error) {
	workdir, err := os.MkdirTemp(opts.Workdir, "recall-conformance-"+tr.Manifest.Case+"-")
	if err != nil {
		return nil, fmt.Errorf("conformance: case %q: workdir: %w", tr.Manifest.Case, err)
	}
	if !opts.KeepWorkdir {
		defer func() { _ = os.RemoveAll(workdir) }()
	}
	// MkdirTemp resolves nothing, and on macOS the temporary directory is a
	// symlink. An adapter that reports a path back would report the resolved
	// one, so the binding is resolved before it is substituted.
	if resolved, err := filepath.EvalSymlinks(workdir); err == nil {
		workdir = resolved
	}

	requests, err := tr.Bind(Bindings{Workdir: workdir})
	if err != nil {
		return nil, err
	}

	proc, err := target(ctx)
	if err != nil {
		return nil, err
	}
	if proc.Stop != nil {
		defer proc.Stop()
	}

	res := &Result{Case: tr.Manifest.Case, Dir: tr.Dir, Volatile: tr.Manifest.Volatile}
	res.Responses, res.Stopped = drive(ctx, requests, proc, opts)
	if proc.Stderr != nil {
		res.Stderr = proc.Stderr()
	}

	// The declared count comes first, so a case that stopped answering reads as
	// that rather than as a pile of missing frames.
	if len(res.Responses) != tr.Manifest.Responses {
		detail := fmt.Sprintf("the manifest declares %d responses, the replay produced %d",
			tr.Manifest.Responses, len(res.Responses))
		if res.Stopped != "" {
			detail += "; " + res.Stopped
		}
		res.Differences = append(res.Differences, Difference{Case: res.Case, Detail: detail})
	}
	res.Differences = append(res.Differences,
		Compare(tr.Manifest.Case, tr.Recorded, res.Responses, tr.Manifest.Volatile)...)
	return res, nil
}

// drive dispatches one transcript under flow "lockstep".
//
// Lines go out in file order. A request waits for its own response before the
// next request is sent; a notification goes out immediately, which is the whole
// reason a cancellation case is recordable — waiting for the response to the
// search being cancelled would deadlock. Then stdin closes and stdout is
// drained, and anything read while draining counts.
//
// Nothing here fails the run. An adapter that closes stdout mid-exchange, or
// stops answering, or breaks the pipe, ends the drive with a reason; the
// response count is what turns that into a failure, so a short exchange cannot
// pass by matching as far as it got.
func drive(ctx context.Context, requests [][]byte, proc *Process, opts Options) (frames [][]byte, stopped string) {
	// The reader outlives nothing: done releases it even when the drive gives
	// up with output still queued, so a wedged adapter cannot leak a goroutine
	// per case.
	done := make(chan struct{})
	defer close(done)
	events := readFrames(proc.Stdout, done)

	await := func(timeout time.Duration) bool {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case ev, ok := <-events:
			if !ok {
				stopped = "the adapter closed stdout with a request outstanding"
				return false
			}
			if ev.err != nil {
				stopped = "reading stdout: " + ev.err.Error()
				return false
			}
			frames = append(frames, ev.line)
			return true
		case <-timer.C:
			stopped = "no response within " + timeout.String()
			return false
		case <-ctx.Done():
			stopped = "replay cancelled: " + ctx.Err().Error()
			return false
		}
	}

	pending := false
	for i, req := range requests {
		notification, err := isNotification(req)
		if err != nil {
			stopped = fmt.Sprintf("request %d: %v", i+1, err)
			return frames, stopped
		}
		if pending && !notification {
			if !await(opts.responseTimeout()) {
				return frames, stopped
			}
			pending = false
		}
		line := append(append(make([]byte, 0, len(req)+1), req...), '\n')
		if _, err := proc.Stdin.Write(line); err != nil {
			stopped = fmt.Sprintf("writing request %d: %v", i+1, err)
			return frames, stopped
		}
		pending = pending || !notification
	}
	if pending && !await(opts.responseTimeout()) {
		return frames, stopped
	}

	// A clean exit closes stdout. Draining proves nothing was written after the
	// last expected reply, which is how an adapter that answers shutdown and
	// then keeps talking is caught.
	_ = proc.Stdin.Close()
	drain := time.NewTimer(opts.drainTimeout())
	defer drain.Stop()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return frames, stopped
			}
			if ev.err != nil {
				stopped = "reading stdout: " + ev.err.Error()
				return frames, stopped
			}
			frames = append(frames, ev.line)
		case <-drain.C:
			return frames, stopped
		case <-ctx.Done():
			stopped = "replay cancelled: " + ctx.Err().Error()
			return frames, stopped
		}
	}
}

// frameEvent is one line read from stdout, or the failure that ended the read.
type frameEvent struct {
	line []byte
	err  error
}

// readFrames reads newline-delimited frames on its own goroutine and closes the
// channel at end of stream.
//
// Both bounds below exist so a runaway adapter costs a failed case rather than
// the harness's memory: the line limit is the protocol's, and the frame count is
// far above any case a person would write.
func readFrames(r io.Reader, done <-chan struct{}) <-chan frameEvent {
	events := make(chan frameEvent, 16)
	go func() {
		defer close(events)
		send := func(ev frameEvent) bool {
			select {
			case events <- ev:
				return true
			case <-done:
				return false
			}
		}
		reader := bufio.NewReader(r)
		for count := 0; ; {
			line, err := reader.ReadBytes('\n')
			switch trimmed := bytes.TrimSpace(line); {
			case len(trimmed) > protocol.MaxFrameBytes:
				send(frameEvent{err: protocol.ErrFrameTooLarge})
				return
			case len(trimmed) > 0:
				if !send(frameEvent{line: trimmed}) {
					return
				}
				count++
				if count >= maxFrames {
					send(frameEvent{err: fmt.Errorf("adapter wrote more than %d frames", maxFrames)})
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					send(frameEvent{err: err})
				}
				return
			}
		}
	}()
	return events
}

// maxFrames bounds the total a case may write.
const maxFrames = 1024

// isNotification reports whether a request line expects no response. The frame
// is validated as JSON-RPC on the way past: a transcript that sends something
// the protocol does not define is a defect in the transcript.
func isNotification(line []byte) (bool, error) {
	var m protocol.Message
	if err := json.Unmarshal(line, &m); err != nil {
		return false, fmt.Errorf("request line is not JSON: %w", err)
	}
	if err := m.Validate(); err != nil {
		return false, fmt.Errorf("request line is not a JSON-RPC frame: %w", err)
	}
	return m.IsNotification(), nil
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
