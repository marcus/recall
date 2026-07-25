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
	server := &http.Server{
		Handler: api.NewHandler(api.ServerOptions{
			Core:           core,
			BearerToken:    token,
			RequestTimeout: *timeout,
			Log:            logger,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	if _, err := fmt.Fprintf(env.Stdout, "recall serve profile %s at http://%s\n", core.Profile(), listener.Addr()); err != nil {
		fail(env, err)
		return ExitError
	}

	stopShutdown := context.AfterFunc(ctx, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})
	defer stopShutdown()

	err = server.Serve(listener)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return ExitOK
	}
	fail(env, err)
	return ExitError
}
