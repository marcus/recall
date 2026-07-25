package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClosed reports a call on a client whose stream is gone.
var ErrClosed = errors.New("protocol: client closed")

// DefaultCancelGrace is how long a cancel notification has to be answered
// before the peer is treated as wedged. It is short: the deadline has already
// passed, so this only buys the adapter time to say "I stopped".
const DefaultCancelGrace = 250 * time.Millisecond

// CallTimeout reports a request that outlived its deadline.
//
// Acknowledged is the field that matters to a supervisor. A peer that answered
// the cancel notification is alive and can be reused; a peer that did not is
// wedged and must be signalled. Either way this is a timeout, never an empty
// success.
type CallTimeout struct {
	Method       string
	ID           ID
	Acknowledged bool
	// Cause is the context error that ended the wait.
	Cause error
}

func (e *CallTimeout) Error() string {
	state := "unacknowledged"
	if e.Acknowledged {
		state = "acknowledged"
	}
	return fmt.Sprintf("%s (id %s) exceeded its deadline, cancel %s: %v",
		e.Method, e.ID, state, e.Cause)
}

func (e *CallTimeout) Unwrap() error { return e.Cause }

// ClientOptions tunes a client. The zero value is usable.
type ClientOptions struct {
	// Diagnostics receives stderr and protocol violations. A nil value gets a
	// fresh buffer, so violations are always counted somewhere.
	Diagnostics *Diagnostics

	// CancelGrace bounds the wait for an answer to a cancel notification.
	CancelGrace time.Duration

	// MaxFrame lowers the frame limit on both directions.
	MaxFrame int

	// Closer is closed by [Client.Close], typically the peer's stdin, so the
	// peer sees EOF and can exit on its own.
	Closer io.Closer
}

// Client is the core's end of the protocol. It writes requests, correlates
// responses by id, and enforces deadlines.
//
// Correlation is by the exact id text the request carried, held in a map that
// is only ever mutated under one mutex. A response whose id has no waiter is
// counted as a violation and dropped: it can never be handed to a different
// call.
type Client struct {
	enc     *Encoder
	dec     *Decoder
	diag    *Diagnostics
	schemas *SchemaSet
	closer  io.Closer

	cancelGrace time.Duration

	nextID atomic.Int64

	mu      sync.Mutex
	pending map[string]chan *Message
	closed  bool
	fatal   error

	readDone chan struct{}
	closeOne sync.Once
}

// NewClient starts reading frames from r and writes requests to w. The returned
// client owns a goroutine until the stream ends or [Client.Close] runs.
func NewClient(r io.Reader, w io.Writer, opt ClientOptions) (*Client, error) {
	schemas, err := Schemas()
	if err != nil {
		return nil, err
	}
	diag := opt.Diagnostics
	if diag == nil {
		diag = NewDiagnostics()
	}
	grace := opt.CancelGrace
	if grace <= 0 {
		grace = DefaultCancelGrace
	}

	c := &Client{
		enc:         NewEncoder(w),
		dec:         NewDecoder(r),
		diag:        diag,
		schemas:     schemas,
		closer:      opt.Closer,
		cancelGrace: grace,
		pending:     make(map[string]chan *Message),
		readDone:    make(chan struct{}),
	}
	if opt.MaxFrame > 0 {
		c.dec.SetMaxFrame(opt.MaxFrame)
		c.enc.SetMaxFrame(opt.MaxFrame)
	}
	go c.read()
	return c, nil
}

// Diagnostics returns the buffer holding this peer's stderr and violations.
func (c *Client) Diagnostics() *Diagnostics { return c.diag }

func (c *Client) read() {
	defer close(c.readDone)
	for {
		msg, err := c.dec.Decode()
		if err != nil {
			if Recoverable(err) {
				// The line was framed but unusable. stdout is supposed to carry
				// frames only, so this is a contract break worth reporting —
				// and the stream is still aligned, so reading continues.
				c.diag.RecordViolation(err)
				continue
			}
			c.fail(err)
			return
		}
		c.deliver(msg)
	}
}

func (c *Client) deliver(msg *Message) {
	if !msg.IsResponse() {
		// The core is the client; an adapter sends responses. Anything else is
		// recorded and ignored rather than dispatched, so a rogue adapter
		// cannot drive the core.
		c.diag.RecordViolation(Errorf(CodeInvalidRequest,
			"adapter sent %q, which the core does not serve", msg.Method))
		return
	}

	c.mu.Lock()
	ch, ok := c.pending[msg.ID.raw]
	if ok {
		delete(c.pending, msg.ID.raw)
	}
	c.mu.Unlock()

	if !ok {
		// A duplicate or late response. Dropping it is what keeps correlation
		// sound: there is no waiter it could belong to.
		c.diag.RecordViolation(Errorf(CodeInvalidRequest,
			"response for unknown id %s", msg.ID))
		return
	}
	ch <- msg
}

// fail ends the session. Every waiter wakes with the same reason, so a stream
// that died mid-request never leaves a call blocked forever.
func (c *Client) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fatal == nil {
		c.fatal = err
	}
	c.closed = true
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
}

func (c *Client) register(id ID) (chan *Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, c.errClosedLocked()
	}
	ch := make(chan *Message, 1)
	c.pending[id.raw] = ch
	return ch, nil
}

func (c *Client) unregister(id ID) {
	c.mu.Lock()
	delete(c.pending, id.raw)
	c.mu.Unlock()
}

func (c *Client) errClosedLocked() error {
	if c.fatal != nil && !errors.Is(c.fatal, io.EOF) {
		return fmt.Errorf("%w: %w", ErrClosed, c.fatal)
	}
	return ErrClosed
}

func (c *Client) errClosed() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.errClosedLocked()
}

// Call sends a request and waits for its response.
//
// On a deadline it sends the advisory recall/cancel notification, waits the
// configured grace, and returns [CallTimeout]. It never returns a zero result
// with a nil error: a caller that ignores the error still cannot mistake a
// timeout for an answer, because result is left untouched.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	raw, err := encodeParams(params)
	if err != nil {
		return err
	}
	if err := c.schemas.ValidateParams(method, raw); err != nil {
		return err
	}

	id := NumberID(c.nextID.Add(1))
	ch, err := c.register(id)
	if err != nil {
		return err
	}
	defer c.unregister(id)

	if err := c.enc.Encode(NewRequest(id, method, raw)); err != nil {
		return fmt.Errorf("send %s: %w", method, err)
	}

	select {
	case msg := <-ch:
		return c.finish(method, msg, result)
	case <-ctx.Done():
	}

	// The deadline passed. Cancellation is advisory, so the answer to "is this
	// process still working?" is whether it responds at all within the grace.
	if err := c.Notify(MethodCancel, CancelParams{ID: id}); err != nil {
		c.diag.RecordViolation(err)
	}
	timer := time.NewTimer(c.cancelGrace)
	defer timer.Stop()
	select {
	case msg := <-ch:
		acknowledged := msg != nil
		return &CallTimeout{Method: method, ID: id, Acknowledged: acknowledged, Cause: ctx.Err()}
	case <-timer.C:
		return &CallTimeout{Method: method, ID: id, Cause: ctx.Err()}
	}
}

func (c *Client) finish(method string, msg *Message, result any) error {
	if msg == nil {
		return c.errClosed()
	}
	if msg.Error != nil {
		return msg.Error
	}
	if err := c.schemas.ValidateResult(method, msg.Result); err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(msg.Result, result); err != nil {
		return Errorf(CodeInternal, "%s result: %v", method, err)
	}
	return nil
}

// Notify sends a fire-and-forget message.
func (c *Client) Notify(method string, params any) error {
	raw, err := encodeParams(params)
	if err != nil {
		return err
	}
	if _, ok := payloadNames[method]; ok {
		if err := c.schemas.ValidateParams(method, raw); err != nil {
			return err
		}
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return c.errClosed()
	}
	return c.enc.Encode(NewNotification(method, raw))
}

// Close releases the write side so the peer sees EOF. It does not wait for the
// peer: a supervisor that must guarantee exit follows this with a signal.
func (c *Client) Close() error {
	var err error
	c.closeOne.Do(func() {
		c.mu.Lock()
		c.closed = true
		if c.fatal == nil {
			c.fatal = ErrClosed
		}
		for id, ch := range c.pending {
			delete(c.pending, id)
			close(ch)
		}
		c.mu.Unlock()
		if c.closer != nil {
			err = c.closer.Close()
		}
	})
	return err
}

// Wait blocks until the read loop has ended, which happens when the peer's
// stdout closes. Callers use it to join the goroutine after the peer exits.
func (c *Client) Wait() {
	<-c.readDone
}

func encodeParams(v any) (json.RawMessage, error) {
	switch p := v.(type) {
	case nil:
		return json.RawMessage("{}"), nil
	case json.RawMessage:
		if len(p) == 0 {
			return json.RawMessage("{}"), nil
		}
		return p, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode params: %w", err)
	}
	return raw, nil
}
