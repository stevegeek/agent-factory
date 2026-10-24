package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	base := t.TempDir()
	env := []string{"HOME=" + base, "FACTORY_ROOT=" + filepath.Join(base, "factory")}
	var out, errb bytes.Buffer
	if code := run(t.Context(), []string{"factory-remote", "version"}, env, nil, &out, &errb); code != 0 || out.String() != "dev dev\n" {
		t.Fatalf("version: exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
	out.Reset()
	errb.Reset()
	ssh := append(env, "SSH_ORIGINAL_COMMAND=run t0001-w1")
	if code := run(t.Context(), []string{"factory-remote"}, ssh, nil, &out, &errb); code != 2 || out.Len() != 0 ||
		!strings.HasPrefix(errb.String(), "Runs dispatcher stages") {
		t.Fatalf("run over ssh: exit %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
	out.Reset()
	list := append(env, "SSH_ORIGINAL_COMMAND=list")
	if code := run(t.Context(), []string{"factory-remote"}, list, nil, &out, &errb); code != 0 || out.Len() != 0 {
		t.Fatalf("list: exit %d, stdout %q", code, out.String())
	}
	if fi, err := os.Stat(filepath.Join(base, "factory", "jobs")); err != nil || !fi.IsDir() {
		t.Fatalf("FACTORY_ROOT/jobs was not made: %v", err)
	}
	errb.Reset()
	if code := run(t.Context(), []string{filepath.Join(base, "bin", "factory"), "tick"}, env, nil, &out, &errb); code != 2 ||
		!strings.Contains(errb.String(), "rebuild") {
		t.Fatalf("factory tick: exit %d, stderr %q", code, errb.String())
	}
}
