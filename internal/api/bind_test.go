package api

import (
	"errors"
	"testing"
)

func TestCheckBindSafety(t *testing.T) {
	tests := []struct {
		name   string
		addr   string
		auth   bool
		want   error
		wantOK bool
	}{
		{"default loopback", DefaultAddr, false, nil, true},
		{"IPv6 loopback", "[::1]:8765", false, nil, true},
		{"external needs authentication", "192.0.2.10:8765", false, ErrAuthenticationRequired, false},
		{"external with authentication", "192.0.2.10:8765", true, nil, true},
		{"wildcard needs explicit interface", ":8765", false, ErrNonLoopback, false},
		{"authenticated wildcard still ambiguous", ":8765", true, ErrNonLoopback, false},
		{"hostname is not a stable bind target", "localhost:8765", true, ErrNonLoopback, false},
		{"missing port", "127.0.0.1", false, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckBind(tc.addr, BindPolicy{Authenticated: tc.auth})
			if tc.wantOK {
				if err != nil {
					t.Fatalf("CheckBind: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("CheckBind unexpectedly accepted address")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("CheckBind error %v does not wrap %v", err, tc.want)
			}
		})
	}
}
