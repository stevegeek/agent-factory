// Command factory-remote is the helper host's job runner and image rebuild
// (internal/helper). The dispatcher's SSH key is forced to it; a job's
// systemd unit runs it as `factory-remote run <job>`; the helper's
// bin/factory symlink runs it as `factory rebuild`.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/stevegeek/agent-factory/internal/helper"
)

func main() {
	// a job's run ends with its unit, not with a hang-up
	if len(os.Args) > 1 && os.Args[1] == "run" {
		signal.Ignore(syscall.SIGHUP)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args, os.Environ(), os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, argv, environ []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return helper.Main(ctx, argv, environ, stdin, stdout, stderr)
}
