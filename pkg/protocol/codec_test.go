package protocol_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/marcus/recall/pkg/protocol"
)

func TestDecodeFrames(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantErr bool
		check   func(t *testing.T, m *protocol.Message)
	}{
		{
			name: "request",
			line: `{"jsonrpc":"2.0","id":1,"method":"recall/health","params":{}}`,
			check: func(t *testing.T, m *protocol.Message) {
				if !m.IsRequest() {
					t.Errorf("not a request: %+v", m)
				}
				if m.ID.String() != "1" {
					t.Errorf("id = %s", m.ID)
				}
			},
		},
		{
			name: "notification has no id",
			line: `{"jsonrpc":"2.0","method":"recall/cancel","params":{"id":1}}`,
			check: func(t *testing.T, m *protocol.Message) {
				if !m.IsNotification() {
					t.Errorf("not a notification: %+v", m)
				}
			},
		},
		{
			name: "string id",
			line: `{"jsonrpc":"2.0","id":"a-1","result":{}}`,
			check: func(t *testing.T, m *protocol.Message) {
				if m.ID.String() != `"a-1"` {
					t.Errorf("id = %s, want the id text verbatim", m.ID)
				}
			},
		},
		{
			// A response must be answerable one way. Both fields set means the
			// peer contradicted itself and no caller should have to pick.
			name:    "result and error together",
			line:    `{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":-32000,"message":"x"}}`,
			wantErr: true,
		},
		{
			name:    "wrong jsonrpc version",
			line:    `{"jsonrpc":"1.0","id":1,"result":{}}`,
			wantErr: true,
		},
		{
			name:    "neither method nor id",
			line:    `{"jsonrpc":"2.0","result":{}}`,
			wantErr: true,
		},
		{
			// JSON-RPC allows only a string or a number. An object id could not
			// be used as a correlation key without inventing a canonical form.
			name:    "object id",
			line:    `{"jsonrpc":"2.0","id":{"n":1},"result":{}}`,
			wantErr: true,
		},
		{
			name:    "not json",
			line:    `{"jsonrpc":`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := protocol.NewDecoder(strings.NewReader(tt.line + "\n"))
			msg, err := dec.Decode()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected a decode failure")
				}
				if !protocol.Recoverable(err) {
					t.Errorf("a bad frame must leave the stream usable, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.check(t, msg)
		})
	}
}

// The stream is framed by newlines, so a bad line costs exactly one message.
// Anything else would let one confused write destroy a whole session.
func TestBadFrameDoesNotDesynchronizeTheStream(t *testing.T) {
	in := strings.Join([]string{
		`not json at all`,
		`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`,
		`{"jsonrpc":"9.9"}`,
		`{"jsonrpc":"2.0","id":2,"result":{"ok":true}}`,
		"",
	}, "\n")

	dec := protocol.NewDecoder(strings.NewReader(in))
	var got []string
	for {
		msg, err := dec.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if !protocol.Recoverable(err) {
				t.Fatalf("unrecoverable: %v", err)
			}
			continue
		}
		got = append(got, msg.ID.String())
	}
	if len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Fatalf("recovered ids = %v, want both good frames", got)
	}
}

// An oversized line is consumed to its newline before it is reported, so the
// frame after it still decodes.
func TestOversizedFrameIsSkippedNotFatal(t *testing.T) {
	huge := `{"jsonrpc":"2.0","id":1,"result":{"pad":"` + strings.Repeat("x", 4096) + `"}}`
	good := `{"jsonrpc":"2.0","id":2,"result":{}}`

	dec := protocol.NewDecoder(strings.NewReader(huge + "\n" + good + "\n"))
	dec.SetMaxFrame(512)

	_, err := dec.Decode()
	if !errors.Is(err, protocol.ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	if !protocol.Recoverable(err) {
		t.Error("an oversized frame leaves the stream framed")
	}
	msg, err := dec.Decode()
	if err != nil {
		t.Fatalf("stream did not resynchronize: %v", err)
	}
	if msg.ID.String() != "2" {
		t.Fatalf("id = %s, want the frame after the oversized one", msg.ID)
	}
}

// One message per line is the framing rule. A payload containing newlines must
// still emit exactly one line, or it would split into unparseable halves.
func TestEncodedFrameNeverContainsARawNewline(t *testing.T) {
	var buf bytes.Buffer
	enc := protocol.NewEncoder(&buf)

	pretty := json.RawMessage("{\n  \"content\": \"line one\\nline two\",\n  \"n\": 1\n}")
	if err := enc.Encode(protocol.NewResult(protocol.NumberID(1), pretty)); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if n := strings.Count(out, "\n"); n != 1 {
		t.Fatalf("frame contains %d newlines, want the terminator only:\n%s", n, out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatal("frame is not newline-terminated")
	}

	dec := protocol.NewDecoder(strings.NewReader(out))
	msg, err := dec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(msg.Result, &got); err != nil {
		t.Fatal(err)
	}
	if got.Content != "line one\nline two" {
		t.Fatalf("content = %q, want the newline preserved inside the string", got.Content)
	}
}

// An adapter answers concurrent requests. Two replies interleaved mid-line
// would be unrecoverable, so the encoder serializes writes.
func TestConcurrentEncodesProduceWholeLines(t *testing.T) {
	var buf bytes.Buffer
	enc := protocol.NewEncoder(&buf)

	const n = 64
	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pad := json.RawMessage(`{"pad":"` + strings.Repeat("y", 512) + `"}`)
			if err := enc.Encode(protocol.NewResult(protocol.NumberID(int64(i)), pad)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	dec := protocol.NewDecoder(bytes.NewReader(buf.Bytes()))
	seen := map[string]bool{}
	for {
		msg, err := dec.Decode()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("interleaved write corrupted the stream: %v", err)
		}
		seen[msg.ID.String()] = true
	}
	if len(seen) != n {
		t.Fatalf("decoded %d distinct frames, want %d", len(seen), n)
	}
}

func TestEncoderRefusesOversizedFrames(t *testing.T) {
	var buf bytes.Buffer
	enc := protocol.NewEncoder(&buf)
	enc.SetMaxFrame(128)

	pad := json.RawMessage(`{"pad":"` + strings.Repeat("z", 1024) + `"}`)
	err := enc.Encode(protocol.NewResult(protocol.NumberID(1), pad))
	if !errors.Is(err, protocol.ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	if buf.Len() != 0 {
		t.Fatal("a rejected frame must not be partially written")
	}
}

// The id is echoed verbatim so correlation cannot drift. A large integer that
// went through a float would come back as a different id.
func TestLargeNumericIDSurvivesRoundTrip(t *testing.T) {
	const raw = `{"jsonrpc":"2.0","id":9007199254740993,"result":{}}`
	dec := protocol.NewDecoder(strings.NewReader(raw + "\n"))
	msg, err := dec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if msg.ID.String() != "9007199254740993" {
		t.Fatalf("id = %s, want the digits unchanged", msg.ID)
	}
	var buf bytes.Buffer
	if err := protocol.NewEncoder(&buf).Encode(msg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"id":9007199254740993`) {
		t.Fatalf("re-encoded frame lost the id: %s", buf.String())
	}
}
