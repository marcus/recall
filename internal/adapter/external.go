package adapter

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/marcus/recall/internal/protocol"
	"github.com/marcus/recall/internal/recall"
)

// External supervises one out-of-process adapter for one source instance.
//
// The process is spawned on first use and pooled: one process per source
// instance, reused for its lifetime, with in-flight requests bounded by the
// manifest's max_concurrency. Two projects using the same adapter binary are
// two source instances and get two processes, because they are two sources.
//
// Deadline enforcement escalates and is never silent. The advisory
// recall/cancel notification goes first; an adapter that answers it keeps its
// process. One that does not is wedged, gets SIGTERM, then SIGKILL, and the
// request is reported as a timeout. A killed process is respawned on the next
// use rather than retried inside the failed request: a retry would spend budget
// the caller did not grant and could re-run work the adapter had already begun.
type External struct {
	spec Spec
	diag *protocol.Diagnostics

	// reaping tracks background teardowns so Close can still promise that no
	// adapter process outlives it.
	reaping sync.WaitGroup

	mu       sync.Mutex
	sess     *session
	cfg      Config
	closed   bool
	spawns   int
	lastExit string
}

// NewExternal prepares an adapter. Nothing is spawned until the first request:
// a configured source that is never queried costs nothing.
func NewExternal(spec Spec) *External {
	diag := spec.Diagnostics
	if diag == nil {
		diag = protocol.NewDiagnostics()
	}
	spec.Diagnostics = diag
	return &External{spec: spec, diag: diag, cfg: spec.Config}
}

// session is one live process and the connection to it.
type session struct {
	conn   *Conn
	cmd    *exec.Cmd
	exited chan struct{}

	// pgid is the adapter's process group, captured at spawn because it cannot
	// be looked up once the leader has exited — which is exactly when a
	// surviving grandchild needs sweeping.
	pgid int

	stdout *os.File
	stderr *os.File
	stdin  *os.File

	waitErr error
	once    sync.Once
}

// Diagnostics returns the adapter's captured stderr, protocol violations, and
// supervision history. Nothing here is parsed as protocol.
func (e *External) Diagnostics() map[string]any {
	out := e.diag.Map()
	if out == nil {
		out = make(map[string]any, 3)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out["spawns"] = e.spawns
	if e.lastExit != "" {
		out["last_exit"] = e.lastExit
	}
	return out
}

// Manifest returns the manifest of the live session, if there is one.
func (e *External) Manifest() (recall.Manifest, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sess == nil {
		return recall.Manifest{}, false
	}
	return e.sess.conn.Manifest(), true
}

// Initialize spawns the adapter if it is not running and returns its manifest.
//
// Negotiation happens once per process. cfg replaces the configured handshake
// input only when no process is running; an already-negotiated session reports
// what it agreed to.
func (e *External) Initialize(ctx context.Context, cfg Config) (recall.Manifest, error) {
	e.mu.Lock()
	if e.sess == nil && (cfg.Workdir != "" || cfg.ProtocolVersionMax != 0) {
		e.cfg = cfg
	}
	e.mu.Unlock()

	conn, err := e.ensure(ctx)
	if err != nil {
		return recall.Manifest{}, err
	}
	return conn.Manifest(), nil
}

// Search spawns if needed, then asks. A source that could not be started is
// unavailable, not empty.
func (e *External) Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error) {
	ctx, cancel := deadlineCtx(ctx, req.Deadline)
	defer cancel()

	conn, err := e.ensure(ctx)
	if err != nil {
		return FailedSearch(err), err
	}
	return conn.Search(ctx, req)
}

// Expand spawns if needed, then retrieves evidence.
func (e *External) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	ctx, cancel := deadlineCtx(ctx, req.Deadline)
	defer cancel()

	conn, err := e.ensure(ctx)
	if err != nil {
		return recall.ExpandResponse{}, err
	}
	return conn.Expand(ctx, req)
}

// Health spawns if needed, then probes. Health is probed on spawn and cached
// with a TTL, so a burst of queries pays for one probe.
func (e *External) Health(ctx context.Context) (recall.Health, error) {
	conn, err := e.ensure(ctx)
	if err != nil {
		return Unhealthy(err), err
	}
	return conn.Health(ctx)
}

// Refresh asks the adapter to bring its projection up to date.
func (e *External) Refresh(ctx context.Context, p protocol.RefreshParams) (recall.Health, error) {
	conn, err := e.ensure(ctx)
	if err != nil {
		return Unhealthy(err), err
	}
	return conn.Refresh(ctx, p)
}

// Close asks for a clean exit and then guarantees one. It is idempotent.
func (e *External) Close() error {
	e.mu.Lock()
	e.closed = true
	sess := e.sess
	e.sess = nil
	e.mu.Unlock()

	if sess == nil {
		// A session retired by a timeout may still be being torn down.
		e.awaitReaping()
		return nil
	}
	err := sess.conn.Close()
	select {
	case <-sess.exited:
		// Exiting cleanly is not the same as leaving nothing behind.
		sess.sweepGroup()
		sess.drain()
	case <-time.After(e.spec.termGrace()):
		// A clean exit was asked for and did not happen. The contract is that
		// the process is gone when Close returns, so it is made gone.
		sess.terminate(e.spec.termGrace())
	}
	e.recordExit(sess)
	e.awaitReaping()
	return err
}

// ensure returns a live connection, spawning one if needed.
func (e *External) ensure(ctx context.Context) (*Conn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return nil, fmt.Errorf("%s: %w", e.spec.Name, ErrClosed)
	}
	if e.sess != nil {
		select {
		case <-e.sess.exited:
			// The process died since the last request. Drop it and start
			// again; the caller learns about the restart through diagnostics,
			// never through a quietly empty result.
			e.recordExitLocked(e.sess)
			// Release the pipe handles here rather than leaving them to
			// finalizers. This is the one supervision path that dropped a
			// session without draining it.
			e.sess.drain()
			e.sess = nil
		default:
			return e.sess.conn, nil
		}
	}
	return e.spawnLocked(ctx)
}

func (e *External) spawnLocked(ctx context.Context) (*Conn, error) {
	if e.spec.Command == "" {
		return nil, &SpawnError{Name: e.spec.Name, Command: "(none)", Err: os.ErrInvalid}
	}
	// The workdir is Recall's to provide. An adapter that cannot write its
	// index has nowhere legitimate to put one, so failing here is better than
	// letting it invent a location.
	if e.cfg.Workdir != "" {
		if err := os.MkdirAll(e.cfg.Workdir, 0o700); err != nil {
			return nil, &SpawnError{Name: e.spec.Name, Command: e.spec.Command, Err: err}
		}
	}

	start := time.Now()
	sess, err := startProcess(e.spec)
	if err != nil {
		return nil, err
	}

	go e.diag.Capture(sess.stderr)

	spawnCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		spawnCtx, cancel = context.WithTimeout(ctx, e.spec.handshakeTimeout())
		defer cancel()
	}

	conn, err := connect(spawnCtx, sess.stdout, sess.stdin, e.cfg, e.spec.Options, time.Since(start))
	if err != nil {
		// A failed handshake leaves a process that agreed to nothing. It is
		// terminated rather than left running: it holds a workdir and may hold
		// a database.
		sess.terminate(e.spec.termGrace())
		e.recordExitLocked(sess)
		return nil, fmt.Errorf("%s: %w", e.spec.Name, err)
	}
	conn.name = e.spec.Name
	conn.escalate = e.wedged
	sess.conn = conn

	e.sess = sess
	e.spawns++
	return conn, nil
}

// wedged terminates a process that outlived a deadline without answering the
// cancel notification. The request that triggered it is already reported as a
// timeout; this makes sure the process cannot go on holding resources or
// answering a request nobody is waiting for.
// wedged retires a session whose adapter stopped answering.
//
// Termination runs on its own goroutine because this is called from inside the
// request that timed out. Tearing the process down synchronously spent the
// grace ladder — cancel grace, then SIGTERM grace, then the wait after
// SIGKILL — on the caller's clock, so a request with a 200ms budget returned
// seconds later. The budget is a promise to the caller; cleanup afterwards is
// Recall's problem, not theirs.
//
// The session is already detached under the lock, so a concurrent call starts a
// fresh process rather than waiting for this one to finish dying.
func (e *External) wedged(string) {
	e.mu.Lock()
	sess := e.sess
	e.sess = nil
	e.mu.Unlock()

	if sess == nil {
		return
	}
	e.reaping.Add(1)
	go func() {
		defer e.reaping.Done()
		sess.terminate(e.spec.termGrace())
		e.recordExit(sess)
	}()
}

// awaitReaping blocks until every background teardown has finished. Close uses
// it so a caller that shuts Recall down still gets the guarantee that no
// adapter process outlives the call.
func (e *External) awaitReaping() { e.reaping.Wait() }

func (e *External) recordExit(sess *session) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recordExitLocked(sess)
}

func (e *External) recordExitLocked(sess *session) {
	select {
	case <-sess.exited:
	default:
		return
	}
	if state := sess.cmd.ProcessState; state != nil {
		e.lastExit = state.String()
		return
	}
	if sess.waitErr != nil {
		e.lastExit = sess.waitErr.Error()
	}
}

// startProcess spawns the adapter with explicit pipes.
//
// The pipes are ours, not exec's: exec's StdoutPipe closes on Wait, which races
// a reader that is still draining a frame. Owning the file descriptors means
// the read side closes when we say so, and the child's copy closes when the
// child exits.
func startProcess(spec Spec) (*session, error) {
	fail := func(err error) (*session, error) {
		return nil, &SpawnError{Name: spec.Name, Command: spec.Command, Err: err}
	}

	inR, inW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW)
		return fail(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW, outR, outW)
		return fail(err)
	}

	cmd := exec.Command(spec.Command, spec.Args...) //nolint:gosec // the command comes from user configuration only; see the trust boundary
	// Own process group, so termination reaches what the adapter started.
	// Signalling the direct child alone leaves grandchildren running with the
	// descriptors they inherited, which makes "the process is gone when Close
	// returns" false for anything an adapter forks — and leaks one per spawn
	// from an adapter that is merely sloppy.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = spec.Dir
	if spec.Env != nil {
		cmd.Env = spec.Env
	}
	cmd.Stdin = inR
	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		closeAll(inR, inW, outR, outW, errR, errW)
		return fail(err)
	}
	// The child holds its own copies now. Dropping ours is what makes EOF
	// arrive when the child exits.
	closeAll(inR, outW, errW)

	sess := &session{
		cmd:    cmd,
		pgid:   cmd.Process.Pid,
		exited: make(chan struct{}),
		stdin:  inW,
		stdout: outR,
		stderr: errR,
	}
	go func() {
		sess.waitErr = cmd.Wait()
		close(sess.exited)
	}()
	return sess, nil
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// terminate escalates until the process is gone: release stdin so a polite
// adapter can exit, then SIGTERM, then SIGKILL.
func (s *session) terminate(grace time.Duration) {
	s.once.Do(func() {
		// Whatever happens below, the group is swept and the pipes released.
		// Three of these paths used to return early, and a clean exit reached
		// on any of them left the adapter's children running — which is the
		// common case, not the exotic one.
		defer func() {
			s.sweepGroup()
			s.drain()
		}()

		_ = s.stdin.Close()
		if s.waitFor(grace / 4) {
			return
		}

		// The whole group, so an adapter's children get the chance to exit
		// cleanly too.
		s.signal(syscall.SIGTERM)
		if s.waitFor(grace) {
			return
		}

		s.signal(syscall.SIGKILL)

		// SIGKILL is not a guarantee of exit: a child blocked in an
		// uninterruptible wait stays until its I/O returns, and an adapter's
		// location can point at a hung network mount. Waiting for it without a
		// bound would hang the caller permanently on the one path that exists
		// to stop that from happening.
		s.waitFor(grace)
	})
}

// waitFor reports whether the process exited within d.
func (s *session) waitFor(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-s.exited:
		return true
	case <-timer.C:
		return false
	}
}

// signal delivers to the adapter's whole process group, falling back to the
// direct child if the group is gone.
func (s *session) signal(sig syscall.Signal) {
	if s.pgid > 0 && syscall.Kill(-s.pgid, sig) == nil {
		return
	}
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Signal(sig)
	}
}

// sweepGroup kills anything still running in the adapter's process group after
// the adapter itself is gone.
//
// A clean exit does not imply an empty group: an adapter that forks a helper
// and then answers shutdown politely leaves that helper running, holding the
// descriptors it inherited. Termination never ran, because there was nothing to
// terminate. "The process is gone when Close returns" has to mean the processes
// it started, or a well-behaved adapter leaks one per spawn.
func (s *session) sweepGroup() {
	if s.pgid > 0 {
		_ = syscall.Kill(-s.pgid, syscall.SIGKILL)
	}
}

// drain releases the read ends once the child is gone, ending the client's read
// loop and the stderr capture.
func (s *session) drain() {
	closeAll(s.stdout, s.stderr)
}

var _ Adapter = (*External)(nil)
