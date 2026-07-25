package cli

import (
	"testing"
	"time"
)

func TestDeriveHTTPDeadlinesBoundsEveryConnectionPhase(t *testing.T) {
	got := deriveHTTPDeadlines(2 * time.Minute)
	if got.header != 5*time.Second {
		t.Fatalf("header timeout = %s", got.header)
	}
	if got.read != 2*time.Minute {
		t.Fatalf("read timeout = %s", got.read)
	}
	if got.write != 2*time.Minute+5*time.Second {
		t.Fatalf("write timeout = %s", got.write)
	}
	if got.idle != 2*time.Minute {
		t.Fatalf("idle timeout = %s", got.idle)
	}

	short := deriveHTTPDeadlines(250 * time.Millisecond)
	if short.header != 250*time.Millisecond || short.read != 250*time.Millisecond ||
		short.write != 500*time.Millisecond || short.idle != 30*time.Second {
		t.Fatalf("short deadlines = %+v", short)
	}
}
