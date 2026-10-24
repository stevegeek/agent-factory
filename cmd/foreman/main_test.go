package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, nil, &out, &errb); code != 0 || out.String() != "dev dev\n" {
		t.Fatalf("version: exit %d, stdout %q", code, out.String())
	}
}

// Anything else is web.Main: a bad flag or a removed subcommand is a usage
// error, before anything is built.
func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"--no-such-flag"}, {"boards"}, {"--api-concurrency", "0"}} {
		var out, errb bytes.Buffer
		if code := run(args, map[string]string{"HOME": t.TempDir()}, &out, &errb); code != 2 || out.Len() != 0 ||
			!strings.Contains(errb.String(), "foreman") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q", args, code, out.String(), errb.String())
		}
	}
}

func TestEnviron(t *testing.T) {
	env := environ([]string{"A=1", "B=x=y", "A=2", "NOEQUALS"})
	if len(env) != 2 || env["A"] != "2" || env["B"] != "x=y" {
		t.Errorf("environ: %v", env)
	}
	if got := environList(env); strings.Join(got, ",") != "A=2,B=x=y" {
		t.Errorf("environList: %v", got)
	}
}
