package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// MaxFrameBytes bounds one line. Recall messages are compact by design —
// candidates are pointers and expansion is budget-bounded — so a frame this
// large is a runaway adapter, not a big result.
const MaxFrameBytes = 8 << 20

// ErrFrameTooLarge reports a line past the frame limit. The line is consumed to
// its newline before the error is returned, so the stream stays framed and the
// next Decode reads a real message.
var ErrFrameTooLarge = errors.New("protocol frame exceeds the line limit")

// FrameError is a line that was read whole but is not a protocol message. It is
// distinguished from a broken stream because the two need different reactions:
// a bad frame is recorded and skipped, a broken stream ends the session.
type FrameError struct {
	// Size is the length of the offending line. The line itself is not kept:
	// it may contain source content, which is not logged by default.
	Size int
	Err  error
}

func (e *FrameError) Error() string {
	return fmt.Sprintf("malformed protocol frame (%d bytes): %v", e.Size, e.Err)
}

func (e *FrameError) Unwrap() error { return e.Err }

// Recoverable reports whether a decode failure left the stream usable. A reader
// records these and continues; anything else ends the session.
func Recoverable(err error) bool {
	var frame *FrameError
	if errors.As(err, &frame) {
		return true
	}
	if errors.Is(err, ErrFrameTooLarge) {
		return true
	}
	// A frame that parsed as JSON but not as JSON-RPC is a protocol violation
	// in one message, not a framing failure.
	var rpc *Error
	return errors.As(err, &rpc) && rpc.Code == CodeInvalidRequest
}

// Decoder reads newline-delimited frames.
type Decoder struct {
	r   *bufio.Reader
	max int
}

// NewDecoder reads frames from r, rejecting any line longer than
// [MaxFrameBytes].
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: bufio.NewReader(r), max: MaxFrameBytes}
}

// SetMaxFrame lowers the frame limit. Tests use it to exercise the oversized
// path without allocating megabytes.
func (d *Decoder) SetMaxFrame(n int) {
	if n > 0 {
		d.max = n
	}
}

// Decode reads the next frame. Blank lines are skipped: they carry no message
// and dropping them costs nothing.
func (d *Decoder) Decode() (*Message, error) {
	for {
		line, err := d.readLine()
		if err != nil {
			return nil, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, &FrameError{Size: len(line), Err: err}
		}
		if err := m.Validate(); err != nil {
			return nil, err
		}
		return &m, nil
	}
}

// readLine returns the next newline-terminated line. An oversized line is
// consumed to its end and reported, so one runaway frame costs one message
// rather than desynchronizing everything after it.
func (d *Decoder) readLine() ([]byte, error) {
	var (
		buf  []byte
		over bool
	)
	for {
		chunk, err := d.r.ReadSlice('\n')
		switch {
		case over:
			// Already discarding this line; only its newline matters now.
		case len(buf)+len(chunk) > d.max:
			over = true
			buf = nil
		default:
			buf = append(buf, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if over {
			return nil, ErrFrameTooLarge
		}
		if err != nil {
			if len(buf) == 0 {
				return nil, err
			}
			// Trailing data with no newline: a truncated write, or a peer that
			// forgot the terminator. Hand it over and let JSON decide; the next
			// call reports the stream error.
			return buf, nil
		}
		return buf, nil
	}
}

// Encoder writes newline-delimited frames. It is safe for concurrent use: an
// adapter answers concurrent requests, and two half-written frames interleaved
// on stdout would be unrecoverable.
type Encoder struct {
	mu  sync.Mutex
	w   io.Writer
	max int
}

// NewEncoder writes frames to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w, max: MaxFrameBytes}
}

// SetMaxFrame lowers the frame limit, so this end refuses to write what the
// other end would refuse to read.
func (e *Encoder) SetMaxFrame(n int) {
	if n > 0 {
		e.mu.Lock()
		e.max = n
		e.mu.Unlock()
	}
}

// Encode writes one frame followed by a newline.
//
// encoding/json compacts embedded raw messages and escapes control characters
// inside strings, so the result cannot contain a raw newline. The check below
// asserts that rather than trusting it, because a frame carrying a newline
// would silently split into two unparseable messages at the peer.
func (e *Encoder) Encode(m *Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode frame: %w", err)
	}
	if i := bytes.IndexAny(b, "\r\n"); i >= 0 {
		return fmt.Errorf("encode frame: raw newline at byte %d", i)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if len(b)+1 > e.max {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(b))
	}
	if _, err := e.w.Write(append(b, '\n')); err != nil {
		return err
	}
	if f, ok := e.w.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}
