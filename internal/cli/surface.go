package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"

	"github.com/marcus/recall/internal/api"
	"github.com/marcus/recall/internal/recall"
)

// surfaceCore is the one in-process implementation every non-config CLI
// surface uses. The HTTP client satisfies the same interface, making
// --server a substitution rather than a second retrieval path.
type surfaceCore struct {
	rt *runtime
}

var _ api.Core = (*surfaceCore)(nil)

func (s *surfaceCore) Query(ctx context.Context, req recall.QueryRequest) (recall.QueryResponse, error) {
	if req.Profile == "" {
		req.Profile = s.rt.profile
	}
	return s.rt.app.Query(ctx, req)
}

func (s *surfaceCore) Expand(ctx context.Context, req recall.ExpandRequest) (recall.ExpandResponse, error) {
	return s.rt.app.Expand(ctx, req, s.rt.profile)
}

func (s *surfaceCore) Sources(ctx context.Context) (api.Listing, error) {
	listing, err := s.rt.listSources(ctx)
	if err != nil {
		return api.Listing{}, err
	}
	status := api.StatusOK
	if sourcesExit(listing) == ExitDegraded {
		status = api.StatusDegraded
	}
	return api.Listing{Payload: listing, Status: status}, nil
}

func (s *surfaceCore) Doctor(ctx context.Context) (api.Listing, error) {
	d := diagnoseRuntime(ctx, s.rt.cfg, s.rt)
	status := api.StatusOK
	switch {
	case d.Failed > 0:
		status = api.StatusFailed
	case d.Degraded > 0:
		status = api.StatusDegraded
	}
	return api.Listing{Payload: d, Status: status}, nil
}

func (s *surfaceCore) Profile() string { return s.rt.profile }

type remoteFlags struct {
	server       *string
	tokenEnvName *string
}

func addRemoteFlags(fs *flag.FlagSet) remoteFlags {
	return remoteFlags{
		server:       fs.String("server", "", "dispatch to recall serve at this base URL"),
		tokenEnvName: fs.String("auth-token-env", "", "environment variable containing the server bearer token"),
	}
}

// openCore selects one implementation of the same surface contract.
//
// The returned close function owns only a locally built runtime. A remote
// client and an injected core have no process-lifetime resource for the CLI to
// close.
func openCore(env Env, profile string, limit int, remote remoteFlags) (api.Core, func() error, error) {
	if remote.server != nil && *remote.server != "" {
		token, err := tokenFromEnv(env, deref(remote.tokenEnvName))
		if err != nil {
			return nil, nil, err
		}
		return api.NewClient(api.ClientOptions{
			BaseURL:     *remote.server,
			Profile:     profile,
			BearerToken: token,
		}), noClose, nil
	}
	if remote.tokenEnvName != nil && *remote.tokenEnvName != "" {
		return nil, nil, errors.New("--auth-token-env requires --server")
	}
	if env.Core != nil {
		if profile != "" && env.Core.Profile() != "" && profile != env.Core.Profile() {
			return nil, nil, fmt.Errorf("core serves profile %q, not %q", env.Core.Profile(), profile)
		}
		return env.Core, noClose, nil
	}

	cfg, err := env.load()
	if err != nil {
		return nil, nil, err
	}
	rt, err := newRuntime(env, cfg, profile, limit)
	if err != nil {
		return nil, nil, err
	}
	return &surfaceCore{rt: rt}, rt.close, nil
}

func noClose() error { return nil }

func tokenFromEnv(env Env, name string) (string, error) {
	if name == "" {
		return "", nil
	}
	token, ok := env.lookupEnv(name)
	if !ok || token == "" {
		return "", fmt.Errorf("environment variable %s is unset or empty", name)
	}
	return token, nil
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// listingInto recovers the CLI's concrete listing without assuming which Core
// implementation produced it. Local cores return the struct itself; HTTP
// clients return raw JSON so newly added fields survive old clients.
func listingInto(payload any, out any) error {
	switch p := payload.(type) {
	case json.RawMessage:
		if err := json.Unmarshal(p, out); err != nil {
			return fmt.Errorf("decoding server listing: %w", err)
		}
		return nil
	default:
		raw, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("encoding in-process listing: %w", err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decoding in-process listing: %w", err)
		}
		return nil
	}
}
