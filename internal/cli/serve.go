package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/paraizo/delamain/internal/api"
	"github.com/paraizo/delamain/internal/board"
	"github.com/paraizo/delamain/internal/config"
	"github.com/paraizo/delamain/internal/control"
	"github.com/paraizo/delamain/internal/forge"
	"github.com/paraizo/delamain/internal/gitx"
	"github.com/paraizo/delamain/internal/notify"
	"github.com/paraizo/delamain/internal/pipeline"
)

// minTokenBytes is the shortest bearer token serve accepts; `serve token` makes 32 random bytes.
const minTokenBytes = 32

type serveOptions struct {
	addr     string
	interval time.Duration
	limit    int
	// config is the config file serve loaded, handed on to the runs and reviews it launches.
	config string
}

func newServeCmd(load func() (config.Config, error), configPath func() string) *cobra.Command {
	opts := serveOptions{}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the remote-control API on loopback, for a tunnel to forward to a phone",
		Long: `Serves the JSON API an app uses to watch and control the runs: status, runs, queue, logs,
report and a stream of changes, plus stop, start, recover, remove and the queue. It listens on
loopback only (api.addr); an HTTPS tunnel such as Cloudflare Tunnel or ngrok forwards to it and
terminates TLS.

Every route demands the bearer token held by the variable api.token_env names (DELAMAIN_API_TOKEN
by default; make one with delamain serve token). A client that fails to log in too often is
locked out, every mutation is written to the audit log under the state dir, and the token never
reaches the runs it starts.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if err := opts.validate(); err != nil {
				return err
			}
			cfg, err := load()
			if err != nil {
				return err
			}
			if opts.config, err = filepath.Abs(configPath()); err != nil {
				return err
			}
			return serve(c.Context(), c.OutOrStdout(), cfg, opts, os.Getenv)
		},
	}
	cmd.Flags().StringVar(&opts.addr, "addr", "", "loopback address to listen on (default api.addr)")
	cmd.Flags().DurationVar(&opts.interval, "interval", time.Second, "time between reloads while a stream is open")
	cmd.Flags().IntVar(&opts.limit, "limit", 20, "finished runs in the stream and the board projection")
	cmd.AddCommand(newServeTokenCmd(load))
	return cmd
}

func (o serveOptions) validate() error {
	if o.interval <= 0 {
		return errors.New("--interval must be greater than zero")
	}
	if o.limit < 0 {
		return errors.New("--limit must not be negative")
	}
	return nil
}

// serve runs the API until ctx ends. The api block is validated as if enabled: running serve
// is what enables it.
func serve(ctx context.Context, out io.Writer, cfg config.Config, opts serveOptions, getenv func(string) string) error {
	cfg.API.Enabled = true
	if opts.addr != "" {
		cfg.API.Addr = opts.addr
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	token, err := apiToken(cfg.API, getenv)
	if err != nil {
		return err
	}
	// Held in memory from here on: the recover serve runs in-process pushes and calls the forge,
	// and a git hook or forge CLI must not find the token in the environment.
	_ = os.Unsetenv(cfg.API.TokenEnv)
	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	in := &inspector{cfg: cfg, st: st, alive: pipeline.ProcessAlive, git: gitx.New()}
	acts := control.New(control.Deps{
		Cfg: cfg, Store: st, Context: ctx,
		NewRunner: func() (*pipeline.Runner, error) { return newRunner(cfg, st, notify.Options{}) },
		ForgeFor:  func(ctx context.Context, repo string) (forge.Forge, error) { return forgeFor(ctx, cfg, repo) },
		Launcher:  serveLauncher(cfg, opts),
	})
	defer func() { _ = acts.Close() }()
	audit, err := api.OpenAudit(cfg.API.AuditPath(cfg.Paths.StateDir))
	if err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	defer func() { _ = audit.Close() }()
	srv := api.New(apiSource{boardSource: boardSource{in: in}, acts: acts, cfg: cfg}, acts, audit, api.Options{
		Token: token, MaxStreams: cfg.API.MaxStreams, TrustProxy: cfg.API.TrustProxy,
		AuthFailures: cfg.API.RateLimit.AuthFailures, RequestsPerSecond: cfg.API.RateLimit.RequestsPerSecond,
		Interval: opts.interval, Limit: apiLimit(opts.limit), Version: Version,
		Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	ln, err := net.Listen("tcp", cfg.API.Addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "api at http://%s/ (ctrl+c stops it); audit log %s\n", ln.Addr(), cfg.API.AuditPath(cfg.Paths.StateDir))
	go srv.Run(ctx)
	return srv.Serve(ctx, ln)
}

// apiLimit is --limit as api.Options reads it: there 0 means the default, so --limit 0, no
// finished runs, is passed as a negative limit.
func apiLimit(limit int) int {
	if limit == 0 {
		return -1
	}
	return limit
}

// serveLauncher starts the runs and reviews the API asks for: with serve's own config file, so
// they share its settings and state dir, and without the API token in their environment.
func serveLauncher(cfg config.Config, opts serveOptions) *control.Launcher {
	return &control.Launcher{StateDir: cfg.Paths.StateDir, StripEnv: []string{cfg.API.TokenEnv}, Config: opts.config}
}

// apiToken reads the bearer token from the variable the config names; it is never printed.
func apiToken(a config.API, getenv func(string) string) ([]byte, error) {
	token := getenv(a.TokenEnv)
	if len(token) < minTokenBytes {
		return nil, fmt.Errorf("api.token_env: variable %s is empty or shorter than %d bytes; make a token with 'delamain serve token' and export it", a.TokenEnv, minTokenBytes)
	}
	return []byte(token), nil
}

func newServeTokenCmd(load func() (config.Config, error)) *cobra.Command {
	var quiet bool
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Print a new random bearer token for the API",
		Long: `Prints a new token of 32 random bytes, as the API wants it. delamain never stores it: put it
in the variable api.token_env names (DELAMAIN_API_TOKEN by default) in the environment of
delamain serve, and in the app; without --quiet it prints how to set that variable.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			token, err := newToken()
			if err != nil {
				return err
			}
			if quiet {
				fmt.Fprintln(c.OutOrStdout(), token)
				return nil
			}
			cfg, err := load() // the variable to set is the one api.token_env names
			if err != nil {
				return err
			}
			name := cfg.API.TokenEnv
			fmt.Fprintf(c.OutOrStdout(), "export %s=%s\n", name, token)
			fmt.Fprintf(c.OutOrStdout(), "# under systemd: put %s=%s in the EnvironmentFile of delamain serve\n", name, token)
			return nil
		},
	}
	cmd.Flags().BoolVar(&quiet, "quiet", false, "print the token alone")
	return cmd
}

// newToken is 32 random bytes as base64url, 43 characters.
func newToken() (string, error) {
	b := make([]byte, minTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// apiSource is what the API reads: the board's source plus the repositories and the run pages.
type apiSource struct {
	boardSource
	acts *control.Actions
	cfg  config.Config
}

// Repos are the repositories the actions allow, each with its forge when it has one.
func (s apiSource) Repos(ctx context.Context) ([]api.Repo, error) {
	var out []api.Repo
	for _, path := range s.acts.AllowedRepos() {
		r := api.Repo{Path: path, Name: filepath.Base(path)}
		if repo, ok, err := forgeRepo(ctx, s.cfg, path); err == nil && ok {
			r.Forge = &api.ForgeRef{Kind: string(repo.Kind), Host: repo.Host, Path: repo.Path}
		}
		out = append(out, r)
	}
	return out, nil
}

// RunsBefore pages the runs as the store does, each read as the board reads them.
func (s apiSource) RunsBefore(ctx context.Context, before int64, limit int, filter string) ([]board.Run, error) {
	recs, err := s.in.st.RunsBefore(ctx, before, limit, filter)
	if err != nil {
		return nil, err
	}
	out := make([]board.Run, 0, len(recs))
	for _, rec := range recs {
		v, err := s.in.view(ctx, rec)
		if err != nil {
			return nil, err
		}
		out = append(out, s.run(v))
	}
	return out, nil
}

// apiProbe is doctor's APIProbe: whether something answers /healthz at addr.
func apiProbe(ctx context.Context, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz answered %s", res.Status)
	}
	return nil
}
