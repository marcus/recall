package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/marcus/recall/internal/api"
)

const serveHelp = `usage: recall serve [flags]

Run the versioned HTTP API over one long-lived application core. The default
address is loopback-only. A non-loopback IP literal is accepted only when every
request is authenticated with a bearer token read from an environment variable.

flags:
  --profile NAME         profile served for this process lifetime
  --addr HOST:PORT       listen address (default 127.0.0.1:8765)
  --auth-token-env ENV   require the bearer token stored in ENV
  --request-timeout DUR  end-to-end request timeout (default 2m)
  --log-requests         log method, path, status, and duration to stderr
`

func runServe(ctx context.Context, env Env, args []string) int {
	fs := newFlagSet("serve")
	var (
		profile    = fs.String("profile", "", "profile served for this process lifetime")
		addr       = fs.String("addr", api.DefaultAddr, "listen address")
		tokenEnv   = fs.String("auth-token-env", "", "environment variable containing the bearer token")
		timeout    = fs.Duration("request-timeout", api.DefaultRequestTimeout, "end-to-end request timeout")
		logRequest = fs.Bool("log-requests", false, "log request metadata")
	)
	if ok, code := parse(env, fs, serveHelp, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageErr(env, serveHelp, errors.New("serve takes no arguments"))
	}
	if *timeout <= 0 {
		return usageErr(env, serveHelp, errors.New("--request-timeout must be greater than zero"))
	}

	token, err := tokenFromEnv(env, *tokenEnv)
	if err != nil {
		fail(env, err)
		return ExitError
	}
	if err := api.CheckBind(*addr, api.BindPolicy{Authenticated: token != ""}); err != nil {
		fail(env, err)
		return ExitError
	}

	core, closeCore, err := openCore(env, *profile, 0, remoteFlags{})
	if err != nil {
		fail(env, err)
		return ExitError
	}
	defer func() { _ = closeCore() }()

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		fail(env, fmt.Errorf("listen %s: %w", *addr, err))
		return ExitError
	}
	defer func() { _ = listener.Close() }()

	var logger func(string)
	if *logRequest {
		logger = func(line string) { _, _ = fmt.Fprintln(env.Stderr, line) }
	}
	deadlines := deriveHTTPDeadlines(*timeout)
	server := &http.Server{
		Handler: api.NewHandler(api.ServerOptions{
			Core:           core,
			BearerToken:    token,
			RequestTimeout: *timeout,
			Log:            logger,
		}),
		ReadHeaderTimeout: deadlines.header,
		ReadTimeout:       deadlines.read,
		WriteTimeout:      deadlines.write,
		IdleTimeout:       deadlines.idle,
		MaxHeaderBytes:    1 << 20,
	}

	if _, err := fmt.Fprintf(env.Stdout, "recall serve profile %s at http://%s\n", core.Profile(), listener.Addr()); err != nil {
		fail(env, err)
		return ExitError
	}

	shutdownDone := make(chan struct{})
	stopShutdown := context.AfterFunc(ctx, func() {
		defer close(shutdownDone)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			// Shutdown stops accepting immediately but waits for active
			// connections. A handler that ignored cancellation must not keep the
			// process and its adapter pool alive past the fallback bound.
			_ = server.Close()
		}
	})
	defer stopShutdown()

	err = server.Serve(listener)
	if ctx.Err() != nil {
		// Serve returns as soon as Shutdown closes the listener. The drain (or
		// forced-Close fallback) is still running and owns active handlers, so
		// do not close the application core or let main call os.Exit until it
		// has actually completed.
		<-shutdownDone
	} else if !stopShutdown() {
		// The callback won the race with an unrelated Serve return.
		<-shutdownDone
	}
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return ExitOK
	}
	fail(env, err)
	return ExitError
}

type httpDeadlines struct {
	header time.Duration
	read   time.Duration
	write  time.Duration
	idle   time.Duration
}

func deriveHTTPDeadlines(request time.Duration) httpDeadlines {
	header := min(request, 5*time.Second)
	idle := max(request, 30*time.Second)
	return httpDeadlines{
		header: header,
		// ReadTimeout begins when the connection is accepted, so it bounds
		// headers and a drip-fed body together.
		read: request,
		// WriteTimeout also begins before the handler. Give it the bounded
		// header allowance plus the advertised handler timeout so a legitimate
		// request does not lose its response budget to header parsing.
		write: request + header,
		idle:  idle,
	}
}
