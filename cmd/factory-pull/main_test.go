package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	var errb bytes.Buffer
	if code := run(context.Background(), []string{"version"}, &errb); code != 0 || errb.String() != "dev dev\n" {
		t.Fatalf("version: exit %d, %q", code, errb.String())
	}
}

func TestABadConfigIsAUsageError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	os.WriteFile(p, []byte("ssh = \"hub\"\n"), 0o600)
	var errb bytes.Buffer
	if code := run(context.Background(), []string{"-config", p}, &errb); code != 2 || !strings.Contains(errb.String(), "host:") {
		t.Errorf("exit %d, %q", code, errb.String())
	}
	errb.Reset()
	if code := run(context.Background(), []string{"-config", p, "extra"}, &errb); code != 2 {
		t.Errorf("extra argument: exit %d", code)
	}
}
