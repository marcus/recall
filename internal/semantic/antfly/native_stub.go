//go:build !cgo || !darwin || !arm64

package antfly

import "fmt"

func openNative(string, bool) (database, error) {
	return nil, fmt.Errorf("%w: requires cgo on macOS arm64 and Antfly v0.3.1 (see package README)", ErrUnavailable)
}
