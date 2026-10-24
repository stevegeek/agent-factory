package web

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/agent-factory/internal/auth"
	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/buildinfo"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/push"
)

// Wiring is what the foreman binary's wiring needs to build the services:
// the factory's paths, and for the token APIs the Host names, whether
// Foreman listens beyond loopback, the token store and the slot count.
type Wiring struct {
	Paths          paths.Paths
	Hosts          []string
	Open           bool
	Tokens         string
	API            bool // serve /api/v1 and /mcp
	APIConcurrency int
	Clock          clock.Clock
	Log            Logger
}

// Wire builds the services web calls (ops, ticketapi, config.Source,
// api, mcp). It may be nil: the desk is then read-only.
type Wire func(ctx context.Context, w Wiring) (Services, error)

// listFlag is a repeatable string flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// config is Main's parsed command line.
type mainConfig struct {
	paths          paths.Paths
	listen         string
	secretFile     string
	trustTS        bool
	tsUsers        listFlag
	hosts          listFlag
	projects       listFlag
	revoke         bool
	push           bool
	api            bool
	apiTokens      string
	apiConcurrency int
	runtimeDir     string // $XDG_RUNTIME_DIR: where the user manager's sockets are
}

// parseFlags reads foreman's flags. The flags deploy renders from config
// (--project, --host, --trust-tailscale, --tailscale-user) and the unit's
// own (--root, --listen) are as before; --factory is accepted and ignored,
// since answers no longer run the CLI.
func parseFlags(args []string, env map[string]string, stderr io.Writer) (*mainConfig, error) {
	base := paths.FromEnv(env)
	c := &mainConfig{}
	fs := flag.NewFlagSet("foreman", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", base.Root, "factory root (FACTORY_ROOT)")
	fs.String("factory", "", "unused: answers run in-process (kept so older units still start)")
	fs.StringVar(&c.listen, "listen", "127.0.0.1:8790", "address to listen on")
	fs.StringVar(&c.secretFile, "secret-file", "", "login secret; created when missing (default <root>/foreman-state/foreman-secret, or the older ~/.config/factory/foreman-secret while only that exists)")
	fs.BoolVar(&c.trustTS, "trust-tailscale", false, "accept Tailscale-User-Login from loopback (tailscale serve) for the --tailscale-user logins")
	fs.Var(&c.tsUsers, "tailscale-user", "a Tailscale login that counts as the maintainer (repeatable; needed with --trust-tailscale)")
	fs.Var(&c.projects, "project", "a project a new ticket may name (repeatable; deploy passes [projects])")
	fs.Var(&c.hosts, "host", "a host name to answer to besides IP addresses and localhost, e.g. the ts.net name (repeatable)")
	fs.BoolVar(&c.revoke, "revoke-sessions", false, "log out every browser, then exit")
	fs.BoolVar(&c.push, "push", true, "send Web Push notifications to subscribed devices (keys beside the secret)")
	fs.BoolVar(&c.api, "api", true, "serve the ticket API at /api/v1, and MCP at /mcp, to tokens made with factory tokens add")
	fs.StringVar(&c.apiTokens, "api-tokens", "", "the dispatcher's API token store (default api-tokens.json beside --secret-file)")
	fs.IntVar(&c.apiConcurrency, "api-concurrency", 2, "ticket verbs the API and MCP may run at once")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q (foreman boards and foreman tokens were removed with the project boards)", fs.Arg(0))
	}
	if c.trustTS && len(c.tsUsers) == 0 {
		return nil, errors.New("--trust-tailscale needs at least one --tailscale-user: the login(s) let in")
	}
	if c.apiConcurrency < 1 {
		return nil, errors.New("--api-concurrency must be at least 1")
	}
	envRoot := map[string]string{}
	for k, v := range env {
		envRoot[k] = v
	}
	envRoot["FACTORY_ROOT"] = *root
	envRoot["HOME"] = base.Home
	c.paths = paths.FromEnv(envRoot)
	c.runtimeDir = env["XDG_RUNTIME_DIR"]
	if c.apiTokens == "" {
		if c.secretFile == "" {
			c.apiTokens = c.paths.Tokens
		} else {
			c.apiTokens = filepath.Join(filepath.Dir(c.secretFile), "api-tokens.json")
		}
	}
	if c.secretFile == "" {
		c.secretFile = defaultSecretFile(c.paths)
	}
	return c, nil
}

// defaultSecretFile is Foreman's login secret: in <root>/foreman-state,
// where deploy moves it so the sandboxed unit can write it. A host whose
// files have not moved yet keeps using ~/.config/factory/foreman-secret.
// Users, sessions and the push keys live beside the secret, so this one path
// decides where all of Foreman's state is.
func defaultSecretFile(p paths.Paths) string {
	moved := filepath.Join(p.Root, "foreman-state", "foreman-secret")
	old := filepath.Join(p.Home, ".config", "factory", "foreman-secret")
	if _, err := os.Stat(moved); err != nil {
		if _, err := os.Stat(old); err == nil {
			return old
		}
	}
	return moved
}

// stateDir makes the directory Foreman's state files live in (0700, as
// deploy makes foreman-state), for the commands that write beside the secret
// before LoadSecret has run.
func stateDir(secretFile string) error {
	if err := os.MkdirAll(filepath.Dir(secretFile), 0o700); err != nil {
		return fmt.Errorf("foreman state: %w", err)
	}
	return nil
}

// open reports a listen address beyond loopback.
func (c *mainConfig) open() bool {
	h, _, err := net.SplitHostPort(c.listen)
	return err == nil && !auth.IsLoopback(h) && h != "localhost"
}

// app is a built server and the background work that goes with it.
type app struct {
	srv    *Server
	pusher *push.Pusher
	board  *boardread.Snapshot
}

// build makes everything the server needs from the parsed flags.
func build(ctx context.Context, c *mainConfig, wire Wire, logger Logger, clk clock.Clock) (*app, error) {
	secret, err := auth.LoadSecret(c.secretFile, logger)
	if err != nil {
		return nil, fmt.Errorf("secret: %w", err)
	}
	users := auth.NewUsers(auth.UsersFile(c.secretFile), clk)
	a, err := auth.New(secret, auth.Options{SinceFile: auth.SinceFile(c.secretFile), Users: users, Hosts: c.hosts,
		TailscaleUsers: c.tsUsersIfTrusted(), Clock: clk, Log: logger})
	if err != nil {
		return nil, err
	}
	if c.trustTS {
		logger.Printf("trusting Tailscale-User-Login %s from loopback: any local process can send it", c.tsUsers.String())
	}
	if c.open() {
		logger.Printf("WARNING: listening on %s over plain HTTP: the secret and the session cookie cross the network "+
			"in the clear. Prefer loopback behind `tailscale serve` or another TLS proxy.", c.listen)
	}
	board := boardread.NewSnapshot(&boardread.Reader{Paths: c.paths, Log: logger})
	var svc Services
	if wire != nil {
		svc, err = wire(ctx, Wiring{Paths: c.paths, Hosts: c.hosts, Open: c.open(), Tokens: c.apiTokens, API: c.api,
			APIConcurrency: c.apiConcurrency, Clock: clk, Log: logger})
		if err != nil {
			return nil, fmt.Errorf("wiring: %w", err)
		}
	}
	if !c.api {
		svc.API, svc.MCP = nil, nil
	}
	var p *push.Pusher
	if c.push {
		// the VAPID contact: the page's own address when known (push
		// services want an https: or mailto: URL)
		subject := "mailto:foreman@localhost"
		if len(c.hosts) > 0 {
			subject = "https://" + c.hosts[0]
		} else {
			logger.Printf("push: no --host given; Apple's push service may refuse the placeholder contact %s", subject)
		}
		opt := push.Options{Audience: users, Clock: clk, Log: logger}
		if src := svc.Config; src != nil {
			// whose own doing a change was is told by the board name answers record
			opt.Maintainer = func() string {
				if cfg, err := src.Current(); err == nil {
					return cfg.Maintainer()
				}
				return model.DefaultPeople()[0]
			}
		}
		p, err = push.New(filepath.Dir(c.secretFile), subject, opt)
		if err != nil {
			return nil, err
		}
	}
	srv, err := New(Options{Paths: c.paths, Auth: a, Board: board, Services: svc, Push: p, Projects: c.projects,
		Sealed: c.paths.Sealed(), Bus: c.busSockets(), Clock: clk, Log: logger})
	if err != nil {
		return nil, err
	}
	return &app{srv: srv, pusher: p, board: board}, nil
}

// busSockets are the user manager's sockets, which the sandbox hides.
func (c *mainConfig) busSockets() []string {
	if c.runtimeDir == "" {
		return nil
	}
	return []string{filepath.Join(c.runtimeDir, "bus"), filepath.Join(c.runtimeDir, "systemd", "private")}
}

func (c *mainConfig) tsUsersIfTrusted() []string {
	if !c.trustTS {
		return nil
	}
	return c.tsUsers
}

// Main is the foreman binary: `foreman users …` manages other people's
// logins; otherwise it serves the desk until SIGINT or SIGTERM. It returns
// the exit status.
func Main(args []string, env map[string]string, stdout, stderr io.Writer, wire Wire) int {
	logger := log.New(stderr, "", log.LstdFlags|log.LUTC)
	clk := clock.System{}
	if len(args) > 0 && args[0] == "users" {
		secret := env["FOREMAN_SECRET_FILE"]
		if secret == "" {
			secret = defaultSecretFile(paths.FromEnv(env))
		}
		if err := stateDir(secret); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := auth.UsersCommand(secret, args[1:], stdout, clk); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	c, err := parseFlags(args, env, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stderr, "foreman:", err)
		return 2
	}
	if c.revoke {
		if err := stateDir(c.secretFile); err != nil {
			fmt.Fprintln(stderr, "foreman:", err)
			return 1
		}
		if err := auth.RevokeSessions(c.secretFile, clk.Now()); err != nil {
			fmt.Fprintln(stderr, "foreman:", err)
			return 1
		}
		fmt.Fprintln(stdout, "every session is revoked; log in again")
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	signal.Ignore(syscall.SIGHUP)
	// a small box: keep the heap tight rather than fast, and hand freed
	// pages back between visits
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(64 << 20)
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				debug.FreeOSMemory()
			}
		}
	}()
	a, err := build(ctx, c, wire, logger, clk)
	if err != nil {
		fmt.Fprintln(stderr, "foreman:", err)
		return 1
	}
	if a.pusher != nil {
		go a.pusher.Watch(ctx, a.board, 30*time.Second)
		logger.Printf("notifications on: %d device(s)", a.pusher.Count())
	}
	// no request runs long any more (slow work is an intent for the tick),
	// so the write timeout no longer stretches to a PR being opened
	srv := &http.Server{Addr: c.listen, Handler: a.srv.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	logger.Printf("foreman %s on http://%s (board %s)", buildinfo.Version, c.listen, c.paths.Root)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Printf("%v", err)
		return 1
	}
	<-done
	return 0
}
