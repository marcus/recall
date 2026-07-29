package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/marcus/recall/pkg/protocol"
)

// Player answers a live request stream from a recorded transcript.
//
// This is the second thing a recording is for. A conformance run drives an
// adapter and checks what it says; an evaluation run needs the opposite —
// model-backed and network-backed adapters replay their recorded responses
// instead of running live, so a benchmark is reproducible. Both directions read
// the same transcripts through [Load], which is why they are one package: a
// separate playback implementation would be free to disagree with the replayer
// about what a transcript means.
//
// Matching is by position and method, and by nothing else. Params legitimately
// differ between the recording and the run — the workdir is new, and every
// deadline is an absolute instant — so comparing them would refuse every replay.
// A method arriving out of the recorded order is refused rather than answered
// from the nearest recorded frame: a source that hands back evidence for a
// question nobody asked is worse than a source that is unavailable.
type Player struct {
	transcript *Transcript

	// exchanges are the recorded request/response pairs, in file order.
	// Notifications are absent: a recording holds no answer to one.
	exchanges []exchange

	served int
}

type exchange struct {
	method   string
	response *protocol.Message
}

// NewPlayer prepares a transcript for playback.
//
// The correlation between recorded requests and recorded responses is checked
// here rather than mid-stream, because a transcript missing an answer is a
// defect in the pack and the run that discovers it should never have started.
func NewPlayer(tr *Transcript) (*Player, error) {
	name := tr.Manifest.Case

	responses := make(map[string]*protocol.Message, len(tr.Recorded))
	for i, line := range tr.Recorded {
		var m protocol.Message
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("conformance: case %q: response %d is not JSON: %w", name, i+1, err)
		}
		if !m.IsResponse() {
			return nil, fmt.Errorf("conformance: case %q: response %d answers no request id", name, i+1)
		}
		responses[m.ID.String()] = &m
	}

	var exchanges []exchange
	for i, line := range tr.Requests {
		var m protocol.Message
		if err := json.Unmarshal(line, &m); err != nil {
			return nil, fmt.Errorf("conformance: case %q: request %d is not JSON: %w", name, i+1, err)
		}
		if m.IsNotification() {
			continue
		}
		if !m.IsRequest() {
			return nil, fmt.Errorf("conformance: case %q: request %d is neither a request nor a notification", name, i+1)
		}
		id := m.ID.String()
		response, ok := responses[id]
		if !ok {
			return nil, fmt.Errorf("conformance: case %q: nothing recorded answers request id %s", name, id)
		}
		delete(responses, id)
		exchanges = append(exchanges, exchange{method: m.Method, response: response})
	}
	for id := range responses {
		return nil, fmt.Errorf("conformance: case %q: response id %s answers no recorded request", name, id)
	}

	return &Player{transcript: tr, exchanges: exchanges}, nil
}

// Serve answers requests read from in by writing the recorded frames to out. It
// returns when the stream ends, or when the caller asks for something the
// recording cannot answer.
//
// Recorded ids are not echoed: the response carries the live request's id, so a
// caller numbering its own requests correlates them normally. Everything else —
// result, error code, timestamps — is the recording verbatim, which is the point
// of a fixture run.
func (p *Player) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	dec := protocol.NewDecoder(in)
	enc := protocol.NewEncoder(out)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		m, err := dec.Decode()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			// A malformed frame is one bad message, not a broken stream; the
			// caller is a live client and may recover.
			if protocol.Recoverable(err) {
				continue
			}
			return err
		}
		if m.IsNotification() {
			// recall/cancel is advisory and a recording holds no answer to it.
			continue
		}
		if !m.IsRequest() {
			return fmt.Errorf("conformance: case %q: playback received a response frame",
				p.transcript.Manifest.Case)
		}
		next, err := p.next(m.Method)
		if err != nil {
			return err
		}
		reply := *next
		id := *m.ID
		reply.ID = &id
		if err := enc.Encode(&reply); err != nil {
			return err
		}
	}
}

// next returns the recorded response for the request at this position.
func (p *Player) next(method string) (*protocol.Message, error) {
	name := p.transcript.Manifest.Case
	if p.served >= len(p.exchanges) {
		return nil, fmt.Errorf("conformance: case %q: %s asked after the recording's %d requests",
			name, method, len(p.exchanges))
	}
	want := p.exchanges[p.served]
	if want.method != method {
		return nil, fmt.Errorf("conformance: case %q: request %d is %s, the recording has %s",
			name, p.served+1, method, want.method)
	}
	p.served++
	return want.response, nil
}
