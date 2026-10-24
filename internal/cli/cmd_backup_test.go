package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/backup"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

// §15.2: on a rebuilt machine there is no factory root, no config.toml and
// no ~/.config/factory yet. keygen makes the key's directory (0700), and a
// restore of a local archive needs only the key; a backup run still needs
// the config.
func TestBackupKeygenAndRestoreWithNoConfig(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"HOME": home, "FACTORY_ROOT": filepath.Join(home, "factory")}
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(append([]string{"factory"}, args...), Env{Stdout: &out, Stderr: &errb, Environ: env,
			Clock: testkit.Fixed{T: testkit.Stamp()}})
		return code, out.String(), errb.String()
	}
	code, out, errs := run("backup", "keygen")
	keyFile := filepath.Join(home, ".config", "factory", "backup-key")
	if code != 0 || !strings.Contains(out, "wrote "+keyFile) {
		t.Fatalf("keygen with no config: %d %q %q", code, out, errs)
	}
	if fi, err := os.Stat(filepath.Dir(keyFile)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the key's directory: %v %v", fi, err)
	}
	key, err := backup.ReadKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	// an archive as factory backup writes one: tar.gz, then encrypted
	var plain bytes.Buffer
	gz := gzip.NewWriter(&plain)
	tw := tar.NewWriter(gz)
	body := []byte(`{"id": "T0001"}` + "\n")
	tw.WriteHeader(&tar.Header{Name: "board/tickets/T0001.json", Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()
	archive := filepath.Join(home, "factory-hub-20261018T043000Z.tar.gz.fbk")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	w, err := backup.Encrypt(f, key, 1<<16, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(plain.Bytes())
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	to := filepath.Join(home, "restored")
	code, out, errs = run("backup", "restore", archive, "--to", to)
	if code != 0 || out != "restored 1 files into "+to+"\n" {
		t.Fatalf("restore with no config: %d %q %q", code, out, errs)
	}
	if got, _ := os.ReadFile(filepath.Join(to, "board", "tickets", "T0001.json")); !bytes.Equal(got, body) {
		t.Errorf("restored %q", got)
	}
	if code, _, errs := run("backup"); code != 1 || !strings.Contains(errs, "config") {
		t.Errorf("a backup run with no config: %d %q", code, errs)
	}
}

// §15.2: `factory backup` and its verbs. deploy.sh installs the timers only
// once `factory backup --help` answers.
func TestBackupCommand(t *testing.T) {
	root := testkit.Root(t)
	root.WriteConfig(t, "[projects.g]\ngithub = \"o/g\"\n")
	run := func(args ...string) (int, string, string) {
		var out, errb bytes.Buffer
		code := Main(append([]string{"factory"}, args...), Env{Stdout: &out, Stderr: &errb, Environ: root.Vars,
			Clock: testkit.Fixed{T: testkit.Stamp()}})
		return code, out.String(), errb.String()
	}
	if code, out, _ := run("backup", "--help"); code != 0 || !strings.Contains(out, "keygen") {
		t.Errorf("--help: %d %q", code, out)
	}
	code, out, errs := run("backup", "keygen")
	key := filepath.Join(root.Home, ".config", "factory", "backup-key")
	if code != 0 || !strings.Contains(out, "wrote "+key) {
		t.Fatalf("keygen: %d %q %q", code, out, errs)
	}
	b, err := os.ReadFile(key)
	if err != nil || !strings.Contains(out, strings.TrimSpace(string(b))) {
		t.Errorf("keygen printed another key than it wrote: %v", err)
	}
	if code, _, errs := run("backup", "keygen"); code != 1 || !strings.Contains(errs, "exists") {
		t.Errorf("a second keygen: %d %q", code, errs)
	}
	if code, _, errs := run("backup", "restore", "a.fbk"); code != 2 || !strings.Contains(errs, "--to DIR") {
		t.Errorf("restore without --to: %d %q", code, errs)
	}
	// no [backup] table: the run fails, with a notice for the maintainer
	if code, _, errs := run("backup"); code != 1 || !strings.Contains(errs, "[backup] needs endpoint and bucket") {
		t.Errorf("unconfigured: %d %q", code, errs)
	}
	e := &Env{Environ: root.Vars}
	e.init()
	ts, _ := e.Board().Reader.List()
	if len(ts) != 1 || ts[0].Title != "Daily backup failed" || ts[0].Project != "factory" {
		t.Errorf("notice: %+v", ts)
	}
}

// P6: [backup] name is the archive line's name; without it the backup
// names itself after the machine.
func TestBackupServiceTakesTheNameFromConfig(t *testing.T) {
	root := testkit.Root(t)
	root.WriteConfig(t, "[backup]\nname = \"master\"\n")
	e := &Env{Environ: root.Vars}
	e.init()
	cfg, err := e.Config()
	if err != nil {
		t.Fatal(err)
	}
	if got := backupService(e, cfg).Host; got != "master" {
		t.Errorf("Host = %q", got)
	}
}

// P6: `factory backup adopt DIR` moves a restored archive into a fresh
// root and says what moved, what stayed, and how to take a project's
// unpushed commits.
func TestBackupAdoptPrintsWhatMoved(t *testing.T) {
	root := testkit.Root(t)
	dir := filepath.Join(root.Home, "restore")
	for rel, body := range map[string]string{
		"config.toml":                      "[projects.widgets]\npath = \"/nonexistent\"\nmode = \"board\"\n",
		"images.json":                      "{}\n",
		"state.json":                       "{}\n",
		"api-tokens.json":                  "{\"version\": 1, \"tokens\": []}\n",
		"foreman-state/foreman-users.json": "{}\n",
		"repos/widgets/unpushed.bundle":    "bundle",
		"board/tickets/T0001.json": `{"id": "T0001", "project": "widgets", "title": "t", "body": "b", "door": "human", "by": "maintainer",
"column": "ready", "branch": null, "agent": null, "reports": {}, "created": "2026-10-18T04:00:00+00:00", "history": [],
"ask": null, "updated": "2026-10-18T04:00:00+00:00", "notes": []}
`,
	} {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root.WriteFile(t, "foreman-state/foreman-secret", "HAND COPIED\n")
	var out, errb bytes.Buffer
	code := Main([]string{"factory", "backup", "adopt", dir}, Env{Stdout: &out, Stderr: &errb, Environ: root.Vars,
		Clock: testkit.Fixed{T: testkit.Stamp()}})
	if code != 0 {
		t.Fatalf("adopt: %d %q %q", code, out.String(), errb.String())
	}
	for _, want := range []string{
		"moved into " + root.Root + ": board, state.json, api-tokens.json, foreman-state/foreman-users.json\n",
		"kept (already here): foreman-state/foreman-secret\n",
		"left in " + dir + ": config.toml",
		"repos/widgets: where its clone lacks them, git -C <clone of widgets> fetch " + filepath.Join(dir, "repos", "widgets", "unpushed.bundle") + " 'refs/heads/*:refs/heads/*'\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(filepath.Join(root.Root, "board", "tickets", "T0001.json")); err != nil {
		t.Errorf("the ticket: %v", err)
	}
	// a second adopt finds the board and refuses
	code = Main([]string{"factory", "backup", "adopt", dir}, Env{Stdout: &out, Stderr: &errb, Environ: root.Vars,
		Clock: testkit.Fixed{T: testkit.Stamp()}})
	if code != 1 || !strings.Contains(errb.String(), "already has a board") {
		t.Errorf("a second adopt: %d %q", code, errb.String())
	}
	if code := Main([]string{"factory", "backup", "adopt"}, Env{Stdout: &out, Stderr: &errb, Environ: root.Vars}); code != 2 {
		t.Errorf("adopt without DIR: %d", code)
	}
}
