package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/paths"
)

// The Live tab's tail of a pull host's job is read from the mailbox, never
// over ssh.
func TestJobTailOfAPullHostReadsTheMailbox(t *testing.T) {
	p := paths.FromEnv(map[string]string{"FACTORY_ROOT": t.TempDir(), "HOME": t.TempDir()})
	dir := filepath.Join(p.Mailbox(), "desktop", "t0001-w1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "beat.log"), []byte("halfway"), 0o600); err != nil {
		t.Fatal(err)
	}
	ssh := func(context.Context, jobproto.Host, string) (string, error) {
		t.Error("ssh was used for a pull host")
		return "", nil
	}
	pull := config.HostCfg{Name: "desktop", Transport: config.TransportPull}
	out, err := jobTails{tail: ssh, paths: p}.Tail(context.Background(), pull, "t0001-w1")
	if err != nil || out != "halfway" {
		t.Errorf("tail %q %v", out, err)
	}
}
