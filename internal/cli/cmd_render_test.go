package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCmd(f func([]string, *bytes.Buffer, *bytes.Buffer) int, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := f(args, &out, &errb)
	return code, out.String(), errb.String()
}

func renderCmd(args []string, out, errb *bytes.Buffer) int { return runRender(args, out, errb) }
func patchCmd(args []string, out, errb *bytes.Buffer) int  { return runPatchScion(args, out, errb) }

func TestRenderCommand(t *testing.T) {
	cfgDir := filepath.Join("..", "..", "examples", "config")
	out := filepath.Join(t.TempDir(), "out")
	code, stdout, stderr := runCmd(renderCmd, cfgDir, out)
	if code != 0 || stdout != out+"\n" {
		t.Fatalf("render = %d %q %q", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "systemd", "foreman.service")); err != nil {
		t.Error(err)
	}
	// Options may come anywhere; -- ends them.
	code, stdout, _ = runCmd(renderCmd, "--", cfgDir, out)
	if code != 0 || stdout != out+"\n" {
		t.Errorf("render -- = %d %q", code, stdout)
	}
}

func TestRenderCommandUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"a"}, {"a", "b", "c"}, {"--bogus", "a", "b"}} {
		code, stdout, stderr := runCmd(renderCmd, args...)
		if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "usage: factory render CONFIG_DIR OUT_DIR\n") {
			t.Errorf("render %q = %d %q %q", args, code, stdout, stderr)
		}
	}
	code, stdout, stderr := runCmd(renderCmd, "--help")
	if code != 0 || !strings.HasPrefix(stdout, "usage: factory render") || stderr != "" {
		t.Errorf("--help = %d %q %q", code, stdout, stderr)
	}
}

func TestRenderCommandRefusal(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("[host]\nfactory_uid = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCmd(renderCmd, cfgDir, filepath.Join(t.TempDir(), "out"))
	want := "config.toml is missing: git_name, git_email, pr_footer (see [identity] and [host])\n"
	if code != 1 || stdout != "" || stderr != want {
		t.Errorf("= %d %q %q", code, stdout, stderr)
	}
	// Other errors carry the factory: prefix.
	code, _, stderr = runCmd(renderCmd, filepath.Join(t.TempDir(), "none"), filepath.Join(t.TempDir(), "out"))
	if code != 1 || !strings.HasPrefix(stderr, "factory: ") {
		t.Errorf("missing config = %d %q", code, stderr)
	}
}

func TestPatchScionCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provision.py")
	anchor := "    new_projects: dict[str, Any] = {ctx.workspace: project_settings}\n"
	if err := os.WriteFile(path, []byte(anchor), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"patched\n", "already patched\n"} {
		code, stdout, stderr := runCmd(patchCmd, path)
		if code != 0 || stdout != want || stderr != "" {
			t.Errorf("patch-scion = %d %q %q, want %q", code, stdout, stderr, want)
		}
	}
	if err := os.WriteFile(path, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCmd(patchCmd, path)
	if code != 1 || stdout != "" || stderr != "anchor not found; provision.py changed upstream, patch by hand\n" {
		t.Errorf("no anchor = %d %q %q", code, stdout, stderr)
	}
	for _, args := range [][]string{{}, {"a", "b"}} {
		if code, _, stderr := runCmd(patchCmd, args...); code != 2 || !strings.HasPrefix(stderr, "usage: factory patch-scion PROVISION_PY\n") {
			t.Errorf("patch-scion %q = %d %q", args, code, stderr)
		}
	}
}
