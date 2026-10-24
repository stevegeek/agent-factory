package cli

import (
	"os"
	"slices"
	"syscall"
	"testing"
)

// the timers' long-running commands ignore SIGHUP; every command is
// cancelled by SIGINT and SIGTERM, and a short one by SIGHUP too.
func TestSignalsFor(t *testing.T) {
	for _, cmd := range []string{"tick", "clock", "sweep", "rebuild", "backup"} {
		sigs, ignore := signalsFor([]string{"factory", cmd})
		if !ignore || slices.Contains(sigs, os.Signal(syscall.SIGHUP)) || !slices.Contains(sigs, os.Signal(syscall.SIGTERM)) {
			t.Errorf("%s: %v ignore HUP %v", cmd, sigs, ignore)
		}
	}
	for _, args := range [][]string{{"factory", "ls"}, {"factory"}, {"factory", "ticket", "tick"}} {
		sigs, ignore := signalsFor(args)
		if ignore || !slices.Contains(sigs, os.Signal(syscall.SIGHUP)) || !slices.Contains(sigs, os.Signal(syscall.SIGINT)) {
			t.Errorf("%v: %v ignore HUP %v", args, sigs, ignore)
		}
	}
}
