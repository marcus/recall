package protocol_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcus/recall/pkg/protocol"
	"github.com/marcus/recall/pkg/recall"
)

// FuzzDecoder feeds arbitrary bytes through the framing layer.
//
// The decoder sits directly on a subprocess's stdout, so its input is whatever
// a third-party adapter wrote — including partial writes, binary noise, and
// enormous lines. Two properties must hold for any input: it never panics, and
// every message it does produce re-encodes and re-decodes to the same thing. A
// frame that changed on the way back out could be correlated or dispatched
// differently by the two ends of the connection.
func FuzzDecoder(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("\n\n\n"))
	f.Add([]byte("{"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":"a","method":"recall/search","params":{"query":"x"}}` + "\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"recall/cancel","params":{"id":1}}` + "\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"x"}}` + "\n"))
	f.Add([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n"))
	f.Add(bytes.Repeat([]byte("A"), 5000))
	f.Add([]byte("\x00\xff\xfe{\"jsonrpc\":\"2.0\"}\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":9007199254740993,"result":{}}` + "\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		dec := protocol.NewDecoder(bytes.NewReader(data))
		dec.SetMaxFrame(4096)

		for range 512 {
			msg, err := dec.Decode()
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return
				}
				if !protocol.Recoverable(err) {
					t.Fatalf("unrecoverable decode error on framed input: %v", err)
				}
				continue
			}

			var buf bytes.Buffer
			enc := protocol.NewEncoder(&buf)
			enc.SetMaxFrame(1 << 20)
			if err := enc.Encode(msg); err != nil {
				t.Fatalf("a decoded frame failed to re-encode: %v", err)
			}
			if n := bytes.Count(buf.Bytes(), []byte("\n")); n != 1 {
				t.Fatalf("re-encoded frame spans %d lines", n)
			}

			round := protocol.NewDecoder(bytes.NewReader(buf.Bytes()))
			round.SetMaxFrame(1 << 20)
			again, err := round.Decode()
			if err != nil {
				t.Fatalf("re-encoded frame failed to decode: %v", err)
			}
			if !sameFrame(msg, again) {
				t.Fatalf("frame changed through a round trip:\n%+v\n%+v", msg, again)
			}
		}
	})
}

func sameFrame(a, b *protocol.Message) bool {
	if a.JSONRPC != b.JSONRPC || a.Method != b.Method {
		return false
	}
	if (a.ID == nil) != (b.ID == nil) {
		return false
	}
	if a.ID != nil && a.ID.String() != b.ID.String() {
		return false
	}
	if (a.Error == nil) != (b.Error == nil) {
		return false
	}
	if a.Error != nil && (a.Error.Code != b.Error.Code || a.Error.Message != b.Error.Message) {
		return false
	}
	return jsonEqual(a.Params, b.Params) && jsonEqual(a.Result, b.Result)
}

func jsonEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return false
	}
	ab, _ := json.Marshal(x)
	bb, _ := json.Marshal(y)
	return bytes.Equal(ab, bb)
}

// FuzzClientCorrelation drives a live client against a hostile peer.
//
// Three concurrent requests are answered out of order, each preceded by
// fuzz-derived noise and by a response for an id nobody is waiting for. Every
// call must receive the answer to its own query or an error — never another
// call's answer, and never a zero value with a nil error.
func FuzzClientCorrelation(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("garbage"))
	f.Add([]byte("{\n}\n"))
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
	f.Add([]byte(`{"jsonrpc":"2.0","method":"recall/search","params":{}}`))
	f.Add(bytes.Repeat([]byte("Z"), 9000))

	f.Fuzz(func(t *testing.T, noise []byte) {
		clientReads, peerWrites := io.Pipe()
		peerReads, clientWrites := io.Pipe()

		diag := protocol.NewDiagnostics()
		c, err := protocol.NewClient(clientReads, clientWrites, protocol.ClientOptions{
			Diagnostics: diag,
			CancelGrace: 10 * time.Millisecond,
			Closer:      clientWrites,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = c.Close()
			_ = peerWrites.Close()
			_ = peerReads.Close()
			c.Wait()
		}()

		const n = 3
		queries := []string{"alpha", "beta", "gamma"}

		peerDone := make(chan struct{})
		go func() {
			defer close(peerDone)
			br := bufio.NewReaderSize(peerReads, 1<<16)

			type pending struct {
				id    protocol.ID
				query string
			}
			var got []pending
			for len(got) < n {
				line, err := br.ReadBytes('\n')
				if err != nil {
					return
				}
				var msg protocol.Message
				if err := json.Unmarshal(line, &msg); err != nil || msg.ID == nil {
					continue
				}
				var req recall.SearchRequest
				if err := json.Unmarshal(msg.Params, &req); err != nil {
					continue
				}
				got = append(got, pending{id: *msg.ID, query: req.Query})
			}

			// Answer in reverse, with noise and a decoy in between. A client
			// that matched on arrival order, or that trusted any well-formed
			// response, would hand back the wrong payload here.
			for i := len(got) - 1; i >= 0; i-- {
				writeNoise(peerWrites, noise)
				_, _ = peerWrites.Write([]byte(
					`{"jsonrpc":"2.0","id":424242,"result":{"candidates":[],"outcome":"failed"}}` + "\n"))

				body, _ := json.Marshal(recall.SearchResponse{
					Outcome:         recall.SearchSuccess,
					SourceWatermark: got[i].query,
				})
				frame, _ := json.Marshal(protocol.NewResult(got[i].id, body))
				if _, err := peerWrites.Write(append(frame, '\n')); err != nil {
					return
				}
			}
		}()

		var wg sync.WaitGroup
		problems := make([]string, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				var resp recall.SearchResponse
				err := c.Call(ctx, protocol.MethodSearch, searchReq(queries[i], time.Hour), &resp)
				switch {
				case err != nil:
					if resp.Outcome == recall.SearchSuccess {
						problems[i] = "a failed call left a successful response behind"
					}
				case resp.SourceWatermark != queries[i]:
					problems[i] = "asked " + queries[i] + ", answered for " + resp.SourceWatermark
				}
			}()
		}
		wg.Wait()
		<-peerDone

		for i, p := range problems {
			if p != "" {
				t.Fatalf("call %d: %s", i, p)
			}
		}
	})
}

// writeNoise injects fuzz bytes as raw lines, minus anything that would be a
// well-formed response. A forged-but-valid response is an adapter lying about
// what it was asked, which is a different failure from the correlation this
// target is about; letting it through would make the corpus report a pass as a
// bug.
func writeNoise(w io.Writer, noise []byte) {
	if len(noise) == 0 {
		return
	}
	for line := range strings.SplitSeq(string(noise), "\n") {
		var msg protocol.Message
		if err := json.Unmarshal([]byte(line), &msg); err == nil && msg.Validate() == nil && msg.IsResponse() {
			continue
		}
		_, _ = w.Write([]byte(line + "\n"))
	}
}
