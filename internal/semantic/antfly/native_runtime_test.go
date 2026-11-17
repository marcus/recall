//go:build cgo && darwin && arm64

package antfly

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeUnavailable(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "missing.dylib")} {
		t.Setenv("ANTFLY_LIBRARY", path)
		if err := loadLibrary(); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("path %q: %v", path, err)
		}
	}
	path := filepath.Join(t.TempDir(), "wrong.dylib")
	if err := os.WriteFile(path, []byte("not the pinned artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTFLY_LIBRARY", path)
	if err := loadLibrary(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("wrong runtime: %v", err)
	}
}
