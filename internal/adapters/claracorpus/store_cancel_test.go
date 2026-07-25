package claracorpus

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
)

// cancellingFragmentReader produces one buffer-sized fragment of a runaway
// JSONL line, then cancels the operation. A line may be arbitrarily larger
// than maxLineBytes, so cancellation has to be checked between ReadSlice
// fragments rather than only between logical records.
type cancellingFragmentReader struct {
	cancel context.CancelFunc
	read   bool
}

func (r *cancellingFragmentReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, errors.New("read continued after cancellation")
	}
	r.read = true
	for i := range p {
		p[i] = 'x'
	}
	r.cancel()
	return len(p), nil
}

func TestReadBoundedLineCancelsBetweenOversizedFragments(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	reader := bufio.NewReaderSize(&cancellingFragmentReader{cancel: cancel}, 32)

	_, _, _, err := readBoundedLine(ctx, reader, sha256.New())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readBoundedLine error = %v, want context.Canceled before a second fragment read", err)
	}
}
