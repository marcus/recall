package sdkcheck

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/marcus/recall/pkg/adapter"
	"github.com/marcus/recall/pkg/conformance"
)

func TestRecordedHandshake(t *testing.T) {
	target := func(ctx context.Context) (*conformance.Process, error) {
		requests, adapterInput := io.Pipe()
		adapterOutput, responses := io.Pipe()
		go func() {
			_ = adapter.Serve(ctx, requests, responses, Notes{})
			_ = responses.Close()
		}()
		return &conformance.Process{
			Stdin:  adapterInput,
			Stdout: adapterOutput,
			Stop: func() {
				_ = adapterInput.Close()
				_ = adapterOutput.Close()
			},
		}, nil
	}

	results, err := conformance.Verify(
		context.Background(),
		filepath.Join("conformance"),
		target,
		conformance.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if !result.OK() {
			t.Fatal(result.Report())
		}
	}
}
