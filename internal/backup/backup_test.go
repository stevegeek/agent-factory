package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/testkit"
)

var backupNow = time.Date(2026, 10, 18, 4, 30, 0, 0, time.UTC) // a Sunday

type notices struct{ titles, bodies []string }

func (n *notices) Notice(_ context.Context, title, body string) error {
	n.titles, n.bodies = append(n.titles, title), append(n.bodies, body)
	return nil
}

type lines struct{ got []string }

func (l *lines) Printf(format string, args ...any) {
	l.got = append(l.got, strings.TrimSpace(strings.ReplaceAll(format, "%", "")))
}

type world struct {
	env    *testkit.Env
	p      paths.Paths
	board  *store.Board
	s3     *fakeS3
	svc    Service
	cfg    config.Backup
	notice *notices
}

// newWorld is a factory root with one of everything a backup holds, and
// every credential it must never hold.
func newWorld(t *testing.T) *world {
	t.Helper()
	env := testkit.Root(t)
	p := paths.FromEnv(env.Vars)
	board := &store.Board{Reader: &boardread.Reader{Paths: p}, Paths: p, Clock: testkit.Fixed{T: backupNow}}
	env.WriteFile(t, "board/tickets/T0001.json", `{
  "id": "T0001",
  "project": "widgets",
  "title": "Tidy the README",
  "body": "b",
  "door": "human",
  "by": "maintainer",
  "column": "ready",
  "branch": null,
  "agent": null,
  "reports": {},
  "created": "2026-10-18T04:00:00+00:00",
  "history": [],
  "ask": null,
  "updated": "2026-10-18T04:00:00+00:00",
  "notes": []
}
`)
	env.WriteFile(t, "board/tickets/.last-id", "1\n")
	env.WriteConfig(t, "[projects.widgets]\npath = \"/nonexistent\"\nmode = \"board\"\n")
	for rel, body := range map[string]string{
		"memory/widgets/worker.git/HEAD":                "ref: refs/heads/main\n",
		"memory/widgets/worker.git/config":              "[core]\n\tbare = true\n",
		"context/widgets/INDEX.md":                      "# widgets\n",
		"state.json":                                    "{}\n",
		"pauses.json":                                   "{}\n",
		"watches.json":                                  "{}\n",
		"images.json":                                   "{\"claude\": {\"version\": \"2.1.302\"}}\n",
		"usage.jsonl":                                   "{\"at\": \"2026-10-18T00:00:00+00:00\", \"ticket\": \"T0001\"}\n",
		"usage.jsonl.lock":                              "",
		"logs/factory.log":                              "2026-10-18T00:00:00+00:00 a line\n",
		"logs/factory.log.20261001T000000Z.gz":          "gz",
		"logs/T0001/work-1.log":                         "agent output\n",
		"foreman-state/foreman-users.json":              "{}\n",
		"foreman-state/foreman.log":                     "2026-10-18T00:00:00+00:00 T0001 answered\n",
		"foreman-state/foreman.log.20261001T000000Z.gz": "gz",
		"foreman-state/.log.lock":                       "",
		"foreman-state/foreman-vapid.pem":               "PUSH KEY\n",
		"foreman-state/foreman-secret":                  "LOGIN SECRET\n",
		"foreman-state/foreman-sessions.json":           "SESSIONS\n",
		"foreman-state/wake":                            "stamp wake\n",
		"board/tickets/.locks/T0001":                    "",
		".tick.again":                                   "",
	} {
		env.WriteFile(t, rel, body)
	}
	home := func(rel, body string) {
		path := filepath.Join(env.Home, rel)
		os.MkdirAll(filepath.Dir(path), 0o700)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	home(".config/factory/api-tokens.json", "{\"version\": 1, \"tokens\": []}\n")
	home(".config/factory/github-app.pem", "APP KEY\n")
	home(".config/factory/claude-oauth-token", "CLAUDE TOKEN\n")
	home(".config/factory/anthropic-api-key", "ANTHROPIC KEY\n")
	home(".config/factory/r2-credentials", "access_key_id = \"AKIDTEST\"\nsecret_access_key = \"test-secret\"\n")
	home(".ssh/factory-remote", "SSH KEY\n")
	if _, err := Keygen(p.BackupKey(), rand.Reader); err != nil {
		t.Fatal(err)
	}
	// a symlink out of the root is never followed
	os.Symlink(filepath.Join(env.Home, ".config/factory/github-app.pem"), env.Path("context", "widgets", "key-link"))
	f := newFakeS3(t)
	n := &notices{}
	w := &world{env: env, p: p, board: board, s3: f, notice: n,
		cfg: config.Backup{Endpoint: "https://acct.r2.example", Bucket: "bkt", Prefix: "box/", IncludePushKey: true,
			Daily: 7, Weekly: 4, Monthly: 6}}
	w.svc = Service{Paths: p, Lock: board, Bucket: f.client(), Notices: n, Clock: testkit.Fixed{T: backupNow},
		Log: &lines{}, Host: "box", Chunk: 64}
	return w
}

func (w *world) restored(t *testing.T, key string) (string, []string) {
	t.Helper()
	k, err := ReadKey(w.p.BackupKey())
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "a.fbk")
	if err := os.WriteFile(local, w.s3.objects[key], 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "out")
	if _, err := RestoreFile(t.Context(), local, k, dir); err != nil {
		t.Fatal(err)
	}
	var files []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return err
	})
	sort.Strings(files)
	return dir, files
}

// the archive's contents and exclusions.
func TestBackupHoldsTheStateAndNoCredential(t *testing.T) {
	w := newWorld(t)
	res, err := w.svc.Run(t.Context(), w.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Key != "box/factory-box-20261018T043000Z.tar.gz.fbk" {
		t.Errorf("key %q", res.Key)
	}
	if !slices.Equal(w.s3.keys(), []string{res.Key, res.Key + ".sha256"}) {
		t.Errorf("bucket %v", w.s3.keys())
	}
	if side := string(w.s3.objects[res.Key+".sha256"]); side != res.SHA256+" "+itoa(res.Size)+"\n" {
		t.Errorf("sidecar %q", side)
	}
	dir, files := w.restored(t, res.Key)
	want := []string{"api-tokens.json", "board/tickets/.last-id", "board/tickets/T0001.json",
		"config.toml", "context/widgets/INDEX.md", "foreman-state/foreman-users.json", "foreman-state/foreman-vapid.pem",
		"foreman-state/foreman.log", "foreman-state/foreman.log.20261001T000000Z.gz",
		"images.json", "logs/T0001/work-1.log", "logs/factory.log", "logs/factory.log.20261001T000000Z.gz",
		"memory/widgets/worker.git/HEAD", "memory/widgets/worker.git/config", "pauses.json", "state.json", "usage.jsonl", "watches.json"}
	if !slices.Equal(files, want) {
		t.Errorf("archive holds\n%v\nwant\n%v", files, want)
	}
	for _, f := range files {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		for _, secret := range []string{"APP KEY", "CLAUDE TOKEN", "ANTHROPIC KEY", "SSH KEY", "LOGIN SECRET", "SESSIONS", "test-secret"} {
			if strings.Contains(string(b), secret) {
				t.Errorf("%s holds %s", f, secret)
			}
		}
	}
	// the sealed archive holds no plaintext
	if strings.Contains(string(w.s3.objects[res.Key]), "Tidy the README") {
		t.Error("the uploaded archive is not encrypted")
	}

	// without include_push_key, the push key stays out
	w.cfg.IncludePushKey = false
	w.svc.Clock = testkit.Fixed{T: backupNow.Add(time.Hour)}
	res, err = w.svc.Run(t.Context(), w.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, files := w.restored(t, res.Key); slices.Contains(files, "foreman-state/foreman-vapid.pem") {
		t.Error("the push key went in with include_push_key = false")
	}
	// nothing staged is left behind on disk
	if entries, _ := os.ReadDir(w.p.Run); len(entries) != 0 {
		t.Errorf("run/ keeps %v", entries)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// the tick lock is held only while board/ and memory/ are copied:
// a held lock stops the backup (bounded by its context), and the lock is
// free again before the rest is copied.
func TestTheTickLockIsHeldOnlyWhileStaging(t *testing.T) {
	w := newWorld(t)
	held, err := w.board.TickLock(t.Context(), store.NoWait)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := w.svc.Run(ctx, w.cfg); err == nil || !strings.Contains(err.Error(), "tick lock") {
		t.Errorf("with the tick held: %v", err)
	}
	if len(w.s3.keys()) != 0 {
		t.Errorf("uploaded %v with the tick lock held", w.s3.keys())
	}
	held.Unlock()
	free := false
	w.svc.afterLocked = func() {
		if l, err := w.board.TickLock(t.Context(), store.NoWait); err == nil {
			free = true
			l.Unlock()
		}
	}
	if _, err := w.svc.Run(t.Context(), w.cfg); err != nil {
		t.Fatal(err)
	}
	if !free {
		t.Error("the tick lock was still held after board/ and memory/ were copied")
	}
	if _, err := w.board.TickLock(t.Context(), fsx.NoWait); err != nil {
		t.Errorf("the lock is left held: %v", err)
	}
}

// Each upload is read back; a mismatch fails the backup, files a notice,
// and retention deletes nothing.
func TestAReadBackMismatchFailsAndKeepsTheOldArchives(t *testing.T) {
	w := newWorld(t)
	old := "box/" + archiveName("box", backupNow.AddDate(0, -3, 0))
	w.s3.objects[old] = []byte("old")
	w.s3.objects[old+".sha256"] = []byte("x")
	key := "box/factory-box-20261018T043000Z.tar.gz.fbk"
	w.s3.corrupt[key] = true
	_, err := w.svc.Run(t.Context(), w.cfg)
	if err == nil || !strings.Contains(err.Error(), "read back") {
		t.Fatalf("err %v", err)
	}
	if _, ok := w.s3.objects[old]; !ok {
		t.Error("retention ran after a failed upload")
	}
	if len(w.notice.titles) != 1 || w.notice.titles[0] != "Daily backup failed" {
		t.Errorf("notices %v", w.notice.titles)
	}
}

// After a good upload, retention removes what it does not keep, sidecars
// with them, and never another host's archives.
func TestRetentionRunsAfterAGoodUpload(t *testing.T) {
	w := newWorld(t)
	var old []string
	for d := 1; d <= 20; d++ {
		k := "box/" + archiveName("box", backupNow.AddDate(0, 0, -d))
		w.s3.objects[k], w.s3.objects[k+".sha256"] = []byte("a"), []byte("b")
		old = append(old, k)
	}
	other := "box/" + archiveName("helper", backupNow.AddDate(0, -5, 0))
	w.s3.objects[other] = []byte("theirs")
	res, err := w.svc.Run(t.Context(), w.cfg)
	if err != nil {
		t.Fatal(err)
	}
	// kept: today and the 6 days before (7 days), Sundays 11 and 4 Oct
	// (and today), and 1 Oct
	if len(res.Deleted) != 20-6-2-1 {
		t.Errorf("deleted %d: %v", len(res.Deleted), res.Deleted)
	}
	for _, d := range res.Deleted {
		if _, ok := w.s3.objects[d+".sha256"]; ok {
			t.Errorf("sidecar of %s left", d)
		}
	}
	if _, ok := w.s3.objects[other]; !ok {
		t.Error("another host's archive was deleted")
	}
}

// Verify restores the newest archive into a temporary directory and
// checks it; a broken newest archive files a notice.
func TestVerifyRestoresTheNewest(t *testing.T) {
	w := newWorld(t)
	if _, err := w.svc.Run(t.Context(), w.cfg); err != nil {
		t.Fatal(err)
	}
	v, err := w.svc.Verify(t.Context(), w.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if v.Tickets != 1 || v.Files < 10 {
		t.Errorf("verified %+v", v)
	}
	// flip the last byte: setting it to 0 left it unchanged one time in 256
	obj := w.s3.objects[v.Key]
	w.s3.objects[v.Key] = append(obj[:len(obj)-1:len(obj)-1], obj[len(obj)-1]^0xff)
	if _, err := w.svc.Verify(t.Context(), w.cfg); err == nil {
		t.Error("a changed archive verified")
	}
	if len(w.notice.titles) != 1 || w.notice.titles[0] != "Backup check failed" {
		t.Errorf("notices %v", w.notice.titles)
	}
}

// a verify that passes files nothing, and writes its one
// factory.log line only on the month's first Sunday (day 1 to 7).
func TestVerifyLogsOnlyOnTheFirstSundayOfTheMonth(t *testing.T) {
	w := newWorld(t)
	if _, err := w.svc.Run(t.Context(), w.cfg); err != nil {
		t.Fatal(err)
	}
	said := func(at time.Time) bool {
		log := &lines{}
		svc := w.svc
		svc.Clock, svc.Log = testkit.Fixed{T: at}, log
		if _, err := svc.Verify(t.Context(), w.cfg); err != nil {
			t.Fatal(err)
		}
		for _, l := range log.got {
			if strings.HasPrefix(l, "backup: the newest backup restores") {
				return true
			}
		}
		return false
	}
	if said(backupNow) { // the 18th, the third Sunday
		t.Error("a later Sunday logged the monthly line")
	}
	if !said(time.Date(2026, 10, 4, 4, 30, 0, 0, time.UTC)) { // the first Sunday
		t.Error("the first Sunday did not log it")
	}
	if len(w.notice.titles) != 0 {
		t.Errorf("a passing verify filed %v", w.notice.titles)
	}
}

// restore refuses a non-empty directory and the live root (or a
// path inside it, by a symlink too), and fetches an archive named only by its object name.
func TestRestoreOnlyIntoAnEmptyDirectory(t *testing.T) {
	w := newWorld(t)
	res, err := w.svc.Run(t.Context(), w.cfg)
	if err != nil {
		t.Fatal(err)
	}
	full := t.TempDir()
	os.WriteFile(filepath.Join(full, "x"), nil, 0o600)
	if _, err := w.svc.Restore(t.Context(), w.cfg, filepath.Base(res.Key), full); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("non-empty: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(w.p.Root, link)
	for _, live := range []string{w.p.Root, filepath.Join(w.p.Root, "restored"), filepath.Join(w.p.Root, "..restored"), filepath.Join(link, "restored")} {
		if _, err := w.svc.Restore(t.Context(), w.cfg, filepath.Base(res.Key), live); err == nil || !strings.Contains(err.Error(), "live root") {
			t.Errorf("%s: %v", live, err)
		}
	}
	dir := filepath.Join(t.TempDir(), "new")
	n, err := w.svc.Restore(t.Context(), w.cfg, filepath.Base(res.Key), dir)
	if err != nil || n < 10 {
		t.Fatalf("restore: %d %v", n, err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "board", "tickets", "T0001.json"))
	if err != nil || !strings.Contains(string(b), "Tidy the README") {
		t.Errorf("restored ticket: %v", err)
	}
}

// A tar member that would leave the directory, or is not a plain file or
// directory, is refused.
func TestExtractRefusesEscapes(t *testing.T) {
	for _, h := range []tar.Header{
		{Name: "../evil", Typeflag: tar.TypeReg},
		{Name: "/abs", Typeflag: tar.TypeReg},
		{Name: "a/../../b", Typeflag: tar.TypeReg},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		{Name: "hard", Typeflag: tar.TypeLink, Linkname: "x"},
	} {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		h.Mode = 0o600
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		gz.Close()
		dir := t.TempDir()
		if _, err := extract(t.Context(), &buf, dir); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("%s: %v", h.Name, err)
		}
	}
	if _, err := extract(t.Context(), strings.NewReader("not gzip"), t.TempDir()); err == nil {
		t.Error("garbage extracted")
	}
}

func TestReadCredentials(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "r2")
	os.WriteFile(p, []byte("# R2\nAWS_ACCESS_KEY_ID=ak\nAWS_SECRET_ACCESS_KEY = 'sk'\n"), 0o600)
	c, err := ReadCredentials(p)
	if err != nil || c != (Credentials{AccessKeyID: "ak", SecretAccessKey: "sk"}) {
		t.Errorf("%+v %v", c, err)
	}
	os.Chmod(p, 0o640)
	if _, err := ReadCredentials(p); err == nil {
		t.Error("a group-readable credentials file was read")
	}
	if _, err := Open(config.Backup{}, paths.Paths{}); !errors.Is(err, errNotConfigured) {
		t.Errorf("no endpoint: %v", err)
	}
}

// with [backup] name = "master" (the CLI sets Host from it), the
// archive is on the master line, whichever machine writes it.
func TestRunNamesTheArchiveFromConfig(t *testing.T) {
	w := newWorld(t)
	w.svc.Host = "master"
	res, err := w.svc.Run(t.Context(), w.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Key, "box/factory-master-") {
		t.Errorf("key %q", res.Key)
	}
}

// verify takes the newest archive of the named line; another line's,
// even a newer one, is ignored.
func TestVerifyFindsTheNamedLine(t *testing.T) {
	w := newWorld(t)
	w.svc.Host = "master"
	res, err := w.svc.Run(t.Context(), w.cfg)
	if err != nil {
		t.Fatal(err)
	}
	other := "box/" + archiveName("hub", backupNow.Add(time.Hour))
	w.s3.objects[other], w.s3.objects[other+".sha256"] = []byte("not an archive"), []byte("x")
	v, err := w.svc.Verify(t.Context(), w.cfg)
	if err != nil || v.Key != res.Key {
		t.Errorf("verified %q (%v), want %q", v.Key, err, res.Key)
	}
}

// retention on the master line never removes the old machine's line.
func TestPruneKeepsOtherLines(t *testing.T) {
	w := newWorld(t)
	w.svc.Host = "master"
	var hub []string
	for d := 1; d <= 20; d++ {
		k := "box/" + archiveName("hub", backupNow.AddDate(0, 0, -d))
		w.s3.objects[k], w.s3.objects[k+".sha256"] = []byte("a"), []byte("b")
		hub = append(hub, k)
	}
	res, err := w.svc.Run(t.Context(), w.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deleted) != 0 {
		t.Errorf("deleted %v", res.Deleted)
	}
	for _, k := range hub {
		if _, ok := w.s3.objects[k]; !ok {
			t.Errorf("%s was deleted", k)
		}
	}
}
