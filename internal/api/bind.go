package api

import (
	"fmt"
	"net"
)

// DefaultAddr is where `recall serve` listens when nothing says otherwise.
//
// The literal 127.0.0.1 is chosen over "localhost": a hostname is resolved, and
// what it resolves to is not this program's decision. A hosts file, a search
// domain, or an IPv6-first resolver can turn "localhost:8765" into a bind on an
// address the user never intended, and a bind is not something to leave to name
// resolution.
const DefaultAddr = "127.0.0.1:8765"

// ErrNonLoopback reports a bind address that is not on the loopback interface.
var ErrNonLoopback = fmt.Errorf("refusing to bind a non-loopback address")

// ErrAuthenticationRequired reports a non-loopback bind without authentication.
var ErrAuthenticationRequired = fmt.Errorf("non-loopback access requires authentication")

// BindPolicy states the security properties of the server that will own a
// listener.
type BindPolicy struct {
	// Authenticated means every request must present a secret that is not
	// carried in the URL. It is the minimum condition for a non-loopback bind.
	Authenticated bool
}

// CheckBind refuses an unsafe address.
//
// Loopback is always permitted. A non-loopback literal is permitted only when
// the handler authenticates every request; selecting the address is the
// explicit configuration the spec requires, and supplying a token is the
// authentication half. Hostnames are refused even with authentication because
// their bind target can change outside this process.
//
// An empty host — ":8765" — binds every interface, so it is refused for the
// same reason as an explicit external address, and is called out separately
// because it is the form people reach for without meaning to.
func CheckBind(addr string, policy BindPolicy) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("address %q: want host:port, such as %s", addr, DefaultAddr)
	}
	if port == "" {
		return fmt.Errorf("address %q: want host:port, such as %s", addr, DefaultAddr)
	}
	if host == "" {
		return fmt.Errorf("%w: %q binds every interface; name one IP literal explicitly", ErrNonLoopback, addr)
	}

	ip := net.ParseIP(host)
	if ip == nil {
		// A hostname could resolve anywhere, and could resolve somewhere else
		// tomorrow. Refusing it keeps the check a statement about the socket
		// rather than about whatever the resolver said at startup.
		return fmt.Errorf("%w: %q is a hostname, and what it resolves to is not this program's decision. Use a loopback literal such as %s",
			ErrNonLoopback, host, DefaultAddr)
	}
	if !ip.IsLoopback() {
		if !policy.Authenticated {
			return fmt.Errorf("%w: %w for %q; configure --auth-token-env or use %s",
				ErrNonLoopback, ErrAuthenticationRequired, host, DefaultAddr)
		}
	}
	return nil
}
