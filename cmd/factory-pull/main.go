// Command factory-pull runs on a pull host (the desktop, through Lever):
// it claims the host's jobs from the dispatcher's mailbox over ssh, hands
// each to an idle Lever worker, beats while it runs and posts its results
// (internal/pull). It runs as a systemd user service; README "Desktop
// worker (Lever)" has the setup.
//
//	factory-pull [-config PATH] [-once]
//	factory-pull version
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/stevegeek/agent-factory/internal/buildinfo"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/pull"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stderr, buildinfo.String())
		return 0
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "factory-pull: %v\n", err)
		return 1
	}
	fl := flag.NewFlagSet("factory-pull", flag.ContinueOnError)
	fl.SetOutput(stderr)
	path := fl.String("config", filepath.Join(home, ".config", "factory-pull", "config.toml"), "the config file")
	once := fl.Bool("once", false, "one pass (claim, beat, post), then exit")
	if err := fl.Parse(args); err != nil || fl.NArg() != 0 {
		return 2
	}
	cfg, err := pull.LoadConfig(*path, home)
	if err != nil {
		fmt.Fprintf(stderr, "factory-pull: %s: %v\n", *path, err)
		return 2
	}
	logger := log.New(stderr, "", 0) // the journal stamps each line
	p := &pull.Puller{Cfg: cfg, Exec: execx.OS{}, Clock: clock.System{}, Log: logger.Printf, Home: home}
	if *once {
		if err := p.Step(ctx); err != nil {
			logger.Printf("%v", err)
			return 1
		}
		return 0
	}
	logger.Printf("factory-pull %s: %s for host %s, workers %v", buildinfo.String(), cfg.SSH, cfg.Host, cfg.Workers)
	if err := p.Run(ctx); err != nil {
		logger.Printf("%v", err)
		return 1
	}
	return 0
}
