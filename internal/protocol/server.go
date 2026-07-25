package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/marcus/recall/internal/recall"
)

// Handler is the adapter end of the contract: six handlers over stdin and
// stdout. Any language with a JSON library can implement the same thing; this
// interface exists so a Go adapter, built-in or external, does not have to
// reimplement framing to be reachable over the wire.
type Handler interface {
	// Initialize negotiates the protocol version and declares what the adapter
	// can do. A range it cannot satisfy is an error, never a downgrade.
	Initialize(ctx context.Context, p InitializeParams) (recall.Manifest, error)
	Search(ctx context.Context, req recall.SearchRequest) (recall.SearchResponse, error)
	Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error)
	Health(ctx context.Context) (recall.Health, error)
	// Refresh brings an adapter-owned projection up to date and reports the
	// resulting health. An adapter that owns no index returns its health
	// unchanged. A failed refresh returns both the error and the health of the
	// generation still published, which is the one still answering.
	Refresh(ctx context.Context, p RefreshParams) (recall.Health, error)
	// Shutdown is asked for a clean exit. Serve returns once in-flight work
	// finishes; a handler that never finishes is what SIGTERM is for.
	Shutdown(ctx context.Context) error
}

// Serve reads frames from r, dispatches them to h, and writes replies to w.
//
// Requests run concurrently, each under a context that recall/cancel cancels
// and the request's own deadline bounds. The encoder serializes writes, so two
// concurrent replies can never interleave into an unparseable line.
//
// Serve returns when the peer closes the stream or asks for shutdown.
func Serve(ctx context.Context, r io.Reader, w io.Writer, h Handler) error {
	schemas, err := Schemas()
	if err != nil {
		return err
	}
	s := &server{
		enc:      NewEncoder(w),
		dec:      NewDecoder(r),
		schemas:  schemas,
		handler:  h,
		inflight: make(map[string]context.CancelFunc),
	}
	return s.run(ctx)
}

type server struct {
	enc     *Encoder
	dec     *Decoder
	schemas *SchemaSet
	handler Handler

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
	writeErr error

	wg sync.WaitGroup
}

func (s *server) run(ctx context.Context) error {
	ctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()

	for {
		msg, err := s.dec.Decode()
		if err != nil {
			if Recoverable(err) {
				// A malformed line cannot be answered — there is no id to
				// answer to — so it is skipped and the stream stays aligned.
				continue
			}
			// The peer is gone. In-flight work has nowhere to reply to, so it
			// is cancelled rather than waited on: a clean exit is what
			// recall/shutdown is for.
			cancelAll()
			if errors.Is(err, io.EOF) {
				return s.finalWriteErr()
			}
			return err
		}

		switch {
		case msg.IsNotification():
			s.notify(msg)
		case msg.IsRequest() && msg.Method == MethodShutdown:
			s.shutdown(ctx, *msg.ID)
			s.wg.Wait()
			return s.finalWriteErr()
		case msg.IsRequest():
			s.dispatch(ctx, msg)
		default:
			// A response arriving at a server has no meaning: the adapter
			// issues no requests. Ignoring it keeps a confused peer from
			// steering the handler.
		}
	}
}

func (s *server) notify(msg *Message) {
	if msg.Method != MethodCancel {
		return
	}
	var p CancelParams
	if err := json.Unmarshal(msg.Params, &p); err != nil {
		return
	}
	s.mu.Lock()
	cancel, ok := s.inflight[p.ID.raw]
	s.mu.Unlock()
	if ok {
		cancel()
	}
}

func (s *server) shutdown(ctx context.Context, id ID) {
	if err := s.handler.Shutdown(ctx); err != nil {
		s.reply(NewErrorResponse(id, AsError(err)))
		return
	}
	s.reply(NewResult(id, json.RawMessage(`{}`)))
}

func (s *server) dispatch(ctx context.Context, msg *Message) {
	id := *msg.ID
	method := msg.Method

	if err := s.schemas.ValidateParams(method, msg.Params); err != nil {
		s.reply(NewErrorResponse(id, AsError(err)))
		return
	}

	reqCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.inflight[id.raw] = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.inflight, id.raw)
			s.mu.Unlock()
			cancel()
		}()

		result, err := s.invoke(reqCtx, method, msg.Params)
		if err != nil {
			s.reply(NewErrorResponse(id, AsError(err)))
			return
		}
		if err := s.schemas.ValidateResult(method, result); err != nil {
			s.reply(NewErrorResponse(id, AsError(err)))
			return
		}
		s.reply(NewResult(id, result))
	}()
}

func (s *server) invoke(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	switch method {
	case MethodInitialize:
		var p InitializeParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		manifest, err := s.handler.Initialize(ctx, p)
		if err != nil {
			return nil, err
		}
		return json.Marshal(manifest)

	case MethodSearch:
		var req recall.SearchRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		ctx, cancel, err := withDeadline(ctx, req.Deadline)
		if err != nil {
			return nil, err
		}
		defer cancel()
		resp, err := s.handler.Search(ctx, req)
		if err != nil {
			return nil, err
		}
		return json.Marshal(resp)

	case MethodExpand:
		var req recall.ExpandRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		ctx, cancel, err := withDeadline(ctx, req.Deadline)
		if err != nil {
			return nil, err
		}
		defer cancel()
		resp, err := s.handler.Expand(ctx, req)
		if err != nil {
			return nil, err
		}
		return json.Marshal(resp)

	case MethodRefresh:
		var p RefreshParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		ctx, cancel, err := withDeadline(ctx, p.Deadline)
		if err != nil {
			return nil, err
		}
		defer cancel()
		health, err := s.handler.Refresh(ctx, p)
		if err != nil {
			return nil, err
		}
		return json.Marshal(health)

	case MethodHealth:
		var p HealthParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, Errorf(CodeInvalidParams, "%v", err)
		}
		ctx, cancel, err := withDeadline(ctx, p.Deadline)
		if err != nil {
			return nil, err
		}
		defer cancel()
		health, err := s.handler.Health(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(health)

	default:
		return nil, Errorf(CodeMethodNotFound, "unknown method %q", method)
	}
}

// withDeadline applies a request's own deadline.
//
// A zero deadline is refused. The schemas cannot catch it: time.Time has no
// omitempty, so an unset field marshals to "0001-01-01T00:00:00Z", which
// satisfies both `required` and `format: date-time`. Treating that as "no
// bound" would turn a caller's oversight into an unbounded adapter request —
// the exact opposite of what a missing deadline should mean, and a path around
// the guarantee that a hung source is reported rather than waited on.
func withDeadline(ctx context.Context, at time.Time) (context.Context, context.CancelFunc, error) {
	if at.IsZero() {
		return nil, nil, Errorf(CodeInvalidParams, "request carries no deadline")
	}
	ctx, cancel := context.WithDeadline(ctx, at)
	return ctx, cancel, nil
}

func (s *server) reply(msg *Message) {
	if err := s.enc.Encode(msg); err != nil {
		s.mu.Lock()
		if s.writeErr == nil {
			s.writeErr = err
		}
		s.mu.Unlock()
	}
}

func (s *server) finalWriteErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeErr
}

// AsError renders any handler failure as a protocol error. A handler that
// returns a typed protocol error keeps its code; anything else is an internal
// error, because guessing a Recall code from an arbitrary message would put
// words in the adapter's mouth.
func AsError(err error) *Error {
	var perr *Error
	if errors.As(err, &perr) {
		return perr
	}
	var verr *VersionError
	if errors.As(err, &verr) {
		return Errorf(CodeInvalidParams, "%s", verr.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return Errorf(CodeBudgetExceeded, "%s", err.Error())
	}
	return Errorf(CodeInternal, "%s", err.Error())
}
