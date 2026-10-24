package backup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

type adoptWorld struct {
	dir   string // the restored archive
	fresh *testkit.Env
	p     paths.Paths
	board *store.Board
	svc   Service
	log   *lines
}

// newAdopt is a restored archive (with a project's repos/) and a fresh
// master's root: board/tickets and logs/ made but empty, as a new root
// has them.
func newAdopt(t *testing.T) *adoptWorld {
	t.Helper()
	w := newWorld(t)
	res, err := w.svc.Run(t.Context(), w.cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := w.restored(t, res.Key)
	if err := os.MkdirAll(filepath.Join(dir, "repos", "widgets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "repos", "widgets", "unpushed.bundle"), []byte("bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := testkit.Root(t)
	p := paths.FromEnv(fresh.Vars)
	board := &store.Board{Reader: &boardread.Reader{Paths: p}, Paths: p, Clock: testkit.Fixed{T: backupNow}}
	log := &lines{}
	return &adoptWorld{dir: dir, fresh: fresh, p: p, board: board, log: log,
		svc: Service{Paths: p, Lock: board, Clock: testkit.Fixed{T: backupNow}, Log: log}}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

// adopt moves the restored state into the fresh root by rename: the
// tickets, the memory stores and the token store land where Paths says,
// with their modes.
func TestAdoptMovesTheArchiveIntoAFreshRoot(t *testing.T) {
	a := newAdopt(t)
	ticketMode := mode(t, filepath.Join(a.dir, "board", "tickets", "T0001.json"))
	tokensMode := mode(t, filepath.Join(a.dir, "api-tokens.json"))
	got, err := a.svc.Adopt(t.Context(), a.dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"board", "memory", "context", "logs", "state.json", "pauses.json", "watches.json", "api-tokens.json",
		"foreman-state/foreman-users.json", "foreman-state/foreman-vapid.pem", "foreman-state/foreman.log",
		"foreman-state/foreman.log.20261001T000000Z.gz"}
	slices.Sort(want)
	moved := slices.Clone(got.Moved)
	slices.Sort(moved)
	if !slices.Equal(moved, want) {
		t.Errorf("moved %v\nwant %v", got.Moved, want)
	}
	ts, probs := (&boardread.Reader{Paths: a.p}).List()
	if len(ts) != 1 || ts[0].ID != "T0001" || len(probs) != 0 {
		t.Errorf("the board after adopt: %v %v", ts, probs)
	}
	if m := mode(t, a.p.TicketFile("T0001")); m != ticketMode {
		t.Errorf("ticket mode %v, was %v", m, ticketMode)
	}
	if m := mode(t, a.p.Tokens); m != tokensMode {
		t.Errorf("token store mode %v, was %v", m, tokensMode)
	}
	if b, err := os.ReadFile(filepath.Join(a.p.MemoryRepo("widgets", "worker"), "HEAD")); err != nil || string(b) != "ref: refs/heads/main\n" {
		t.Errorf("memory store: %q %v", b, err)
	}
	if fi, err := os.Stat(a.p.ForemanState()); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("foreman-state: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "board")); !os.IsNotExist(err) {
		t.Errorf("board is still in the restored directory: %v", err)
	}
	if len(a.log.got) != 1 || !strings.HasPrefix(a.log.got[0], "backup adopt:") {
		t.Errorf("log %q", a.log.got)
	}
}

// a root that already has a board is not a fresh master: adopt
// refuses and moves nothing.
func TestAdoptRefusesARootWithABoard(t *testing.T) {
	a := newAdopt(t)
	a.fresh.WriteFile(t, "board/tickets/T0009.json", "{}\n")
	_, err := a.svc.Adopt(t.Context(), a.dir)
	if err == nil || !strings.Contains(err.Error(), "this root already has a board: adopt only into a fresh master") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "board", "tickets", "T0001.json")); err != nil {
		t.Errorf("something moved: %v", err)
	}
}

// a tick running in the new root refuses the adopt.
func TestAdoptRefusesWhileTheTickLockIsHeld(t *testing.T) {
	a := newAdopt(t)
	held, err := a.board.TickLock(t.Context(), store.NoWait)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Unlock()
	if _, err := a.svc.Adopt(t.Context(), a.dir); err == nil || !strings.Contains(err.Error(), "a tick is running here") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "memory")); err != nil {
		t.Errorf("something moved: %v", err)
	}
}

// every target is checked before anything moves; one that holds a
// file (a helper's rebuild log) refuses the whole adopt, naming it.
func TestAdoptRefusesANonEmptyTargetBeforeMovingAnything(t *testing.T) {
	a := newAdopt(t)
	a.fresh.WriteFile(t, "logs/rebuild-claude.log", "a helper's rebuild\n")
	_, err := a.svc.Adopt(t.Context(), a.dir)
	if err == nil || !strings.Contains(err.Error(), a.p.Logs) {
		t.Fatalf("err %v", err)
	}
	for _, rel := range []string{"board", "memory", "context", "state.json", "api-tokens.json", "foreman-state/foreman-users.json"} {
		if _, err := os.Stat(filepath.Join(a.dir, rel)); err != nil {
			t.Errorf("%s moved: %v", rel, err)
		}
	}
	if _, err := os.Stat(a.p.Tokens); !os.IsNotExist(err) {
		t.Errorf("the token store moved: %v", err)
	}
}

// the login secret already in the root (copied by hand; no archive
// holds it) stays, and is listed as kept; the rest moves in beside it.
func TestAdoptKeepsAHandCopiedSecret(t *testing.T) {
	a := newAdopt(t)
	if err := os.MkdirAll(a.p.ForemanState(), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(a.p.ForemanState(), "foreman-secret")
	os.WriteFile(secret, []byte("HAND COPIED\n"), 0o600)
	got, err := a.svc.Adopt(t.Context(), a.dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(secret); string(b) != "HAND COPIED\n" {
		t.Errorf("secret %q", b)
	}
	if !slices.Equal(got.Kept, []string{"foreman-state/foreman-secret"}) {
		t.Errorf("kept %v", got.Kept)
	}
	if b, err := os.ReadFile(filepath.Join(a.p.ForemanState(), "foreman-vapid.pem")); err != nil || string(b) != "PUSH KEY\n" {
		t.Errorf("the push key did not move in: %q %v", b, err)
	}
}

// any other foreman-state file in both the root and the archive is
// Foreman's own from a start here (a push key it made unbinds every
// device): adopt refuses, naming it, before anything moves.
func TestAdoptRefusesForemanStateTheRootMadeItself(t *testing.T) {
	for _, name := range []string{"foreman-vapid.pem", "foreman-users.json"} {
		a := newAdopt(t)
		if err := os.MkdirAll(a.p.ForemanState(), 0o700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(a.p.ForemanState(), "foreman-secret"), []byte("HAND COPIED\n"), 0o600)
		mine := filepath.Join(a.p.ForemanState(), name)
		os.WriteFile(mine, []byte("FOREMAN MADE THIS\n"), 0o600)
		got, err := a.svc.Adopt(t.Context(), a.dir)
		if err == nil || !strings.Contains(err.Error(), mine) || !strings.Contains(err.Error(), "Foreman has run in this root") {
			t.Fatalf("%s: err %v", name, err)
		}
		if len(got.Moved) != 0 {
			t.Errorf("%s: moved %v", name, got.Moved)
		}
		if _, err := os.Stat(filepath.Join(a.dir, "board")); err != nil {
			t.Errorf("%s: the board moved: %v", name, err)
		}
		if b, _ := os.ReadFile(mine); string(b) != "FOREMAN MADE THIS\n" {
			t.Errorf("%s: %q", name, b)
		}
	}
}

// a foreman-state step that fails after the whole items moved names
// what is still in DIR, for moving by hand (a rerun refuses the board).
func TestAdoptHalfDoneSaysWhatIsLeft(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory's mode")
	}
	a := newAdopt(t)
	state := filepath.Join(a.dir, "foreman-state")
	if err := os.Chmod(state, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(state, 0o700) })
	got, err := a.svc.Adopt(t.Context(), a.dir)
	if err == nil {
		t.Fatal("adopt passed with an unwritable foreman-state")
	}
	if !slices.Contains(got.Moved, "board") {
		t.Errorf("moved %v", got.Moved)
	}
	for _, want := range []string{"moved so far: board", "still in " + a.dir + ": foreman-state/", "by hand"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %v\nwant %q", err, want)
		}
	}
}

// a foreman-state file moves without replacing one that appeared at
// its target since the checks.
func TestMoveNoReplaceKeepsAnExistingTarget(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	os.WriteFile(src, []byte("archive's\n"), 0o600)
	os.WriteFile(dst, []byte("already here\n"), 0o600)
	if err := moveNoReplace(src, dst); err == nil {
		t.Fatal("moved over an existing file")
	}
	if b, _ := os.ReadFile(dst); string(b) != "already here\n" {
		t.Errorf("dst %q", b)
	}
	if b, _ := os.ReadFile(src); string(b) != "archive's\n" {
		t.Errorf("src %q", b)
	}
	os.Remove(dst)
	if err := moveNoReplace(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("src is still there: %v", err)
	}
}

// config.toml (deploy renders it) and images.json (this host's
// images) stay in the restored directory; the root's own are untouched.
func TestAdoptLeavesConfigAndImages(t *testing.T) {
	a := newAdopt(t)
	a.fresh.WriteConfig(t, "# this host's\n")
	if _, err := a.svc.Adopt(t.Context(), a.dir); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"config.toml", "images.json"} {
		if _, err := os.Stat(filepath.Join(a.dir, rel)); err != nil {
			t.Errorf("%s left the restored directory: %v", rel, err)
		}
	}
	if b, _ := os.ReadFile(a.p.Config); string(b) != "# this host's\n" {
		t.Errorf("the root's config.toml: %q", b)
	}
	if _, err := os.Stat(a.p.ImagesState()); !os.IsNotExist(err) {
		t.Errorf("images.json was adopted: %v", err)
	}
}

// each repos/<project> stays in the restored directory and is listed,
// for its bundle to be fetched where a clone lacks the commits.
func TestAdoptListsRepos(t *testing.T) {
	a := newAdopt(t)
	os.MkdirAll(filepath.Join(a.dir, "repos", "gadgets"), 0o700)
	got, err := a.svc.Adopt(t.Context(), a.dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Repos, []string{"gadgets", "widgets"}) {
		t.Errorf("repos %v", got.Repos)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "repos", "widgets", "unpushed.bundle")); err != nil {
		t.Errorf("the bundle moved: %v", err)
	}
}

// a directory that is not a restored archive (its config does not
// check) is refused.
func TestAdoptRefusesWhatIsNotARestoredArchive(t *testing.T) {
	a := newAdopt(t)
	if _, err := a.svc.Adopt(t.Context(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "config.toml") {
		t.Errorf("an empty directory: %v", err)
	}
}

// the live root, a restored archive inside it (by path, by a symlink
// into it, or under a name starting ".."), is refused as the root.
func TestAdoptRefusesTheRootAndWhatIsInsideIt(t *testing.T) {
	const want = "is the root or inside it"
	a := newAdopt(t)
	if _, err := a.svc.Adopt(t.Context(), a.p.Root); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("the live root: %v", err)
	}
	for _, name := range []string{"restored", "..restored"} {
		inside := filepath.Join(a.p.Root, name)
		if err := os.CopyFS(inside, os.DirFS(a.dir)); err != nil {
			t.Fatal(err)
		}
		if _, err := checkRestored(inside); err != nil {
			t.Fatalf("the copy is not a restored archive: %v", err)
		}
		if _, err := a.svc.Adopt(t.Context(), inside); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s inside the root: %v", name, err)
		}
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(inside, link); err != nil {
			t.Fatal(err)
		}
		if _, err := a.svc.Adopt(t.Context(), link); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("a symlink to %s inside the root: %v", name, err)
		}
	}
	// a sibling named like the root plus more is not inside it
	if in, err := within(a.p.Root, a.p.Root+"..x"); err != nil || in {
		t.Errorf("a sibling: %v %v", in, err)
	}
}

// an archive written after the move ([build] host = "self", built by
// the master's own factory-remote) adopts on any machine: the host check
// is factory check's, not the archive's.
func TestAdoptTakesASelfBuildArchiveOnAnotherHost(t *testing.T) {
	a := newAdopt(t)
	cfg := "[projects.widgets]\npath = \"/nonexistent\"\nmode = \"board\"\n\n[build]\nhost = \"self\"\n\n[remote.some-other-host]\nbuild = true\n"
	if err := os.WriteFile(filepath.Join(a.dir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.svc.Adopt(t.Context(), a.dir); err != nil {
		t.Fatal(err)
	}
}
