//go:build !windows

package main

import (
	"os"
	"syscall"
	"testing"
)

func TestTerminationSignalsIncludeInterruptAndTerminate(t *testing.T) {
	got := terminationSignals()
	has := func(want os.Signal) bool {
		for _, signal := range got {
			if signal == want {
				return true
			}
		}
		return false
	}
	if !has(os.Interrupt) || !has(syscall.SIGTERM) {
		t.Fatalf("termination signals = %v, want interrupt and SIGTERM", got)
	}
}
