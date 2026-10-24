package helper

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/mailbox"
)

// mail runs the forced command of a pull host's key, `factory-remote
// mailbox <host>`, with the verb line in SSH_ORIGINAL_COMMAND.
func (r *rig) mail(t *testing.T, host, command string, stdin []byte) int {
	t.Helper()
	r.reset()
	r.Stdin = bytes.NewReader(stdin)
	r.Environ = append(slices.Clip(r.Environ), "SSH_ORIGINAL_COMMAND="+command)
	defer func() { r.Environ = r.Environ[:len(r.Environ)-1] }()
	return r.Serve(t.Context(), []string{r.Self, jobproto.MailMode, host})
}

// queue puts a job in the mailbox, as the dispatcher's start does.
func (r *rig) queue(t *testing.T, host, job string) []byte {
	t.Helper()
	tar, err := jobproto.TarFlat([]jobproto.File{{Name: "prompt.md", Data: []byte("p")},
		{Name: "meta.json", Data: jobproto.Meta{Branch: "task", Start: "none", TimeoutMinutes: 30}.Encode()}}, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	box := mailbox.Box{Dir: filepath.Join(r.Root, "mailbox"), Clock: r.Clock, ClaimWindow: 30 * time.Minute, BeatWindow: 10 * time.Minute}
	if err := box.Start(host, job, tar); err != nil {
		t.Fatal(err)
	}
	return tar
}

func TestMailboxModeClaimsBeatsAndPosts(t *testing.T) {
	r := newRig(t)
	if code := r.mail(t, "desktop", "claim desktop", nil); code != jobproto.ExitNoJob || r.stdout.Len() != 0 {
		t.Fatalf("empty claim: exit %d %q %q", code, r.stdout, r.stderr)
	}
	tar := r.queue(t, "desktop", "t0001-w1")
	if code := r.mail(t, "desktop", "claim desktop", nil); code != 0 || r.stdout.String() != "t0001-w1\n"+string(tar) {
		t.Fatalf("claim: exit %d stderr %q", code, r.stderr)
	}
	if code := r.mail(t, "desktop", "beat desktop t0001-w1", []byte("halfway")); code != 0 || r.stdout.String() != "ok\n" {
		t.Fatalf("beat: exit %d %q %q", code, r.stdout, r.stderr)
	}
	posted, _ := jobproto.PackCollect(map[string][]byte{"report.json": []byte(`{"status": "done"}`)})
	if code := r.mail(t, "desktop", "done desktop t0001-w1", posted); code != 0 || r.stdout.String() != "ok\n" {
		t.Fatalf("done: exit %d %q", code, r.stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(r.Root, "mailbox", "desktop", "t0001-w1", "done.tar")); !bytes.Equal(b, posted) {
		t.Error("done.tar is not what was posted")
	}
	if code := r.mail(t, "desktop", "release desktop t0001-w1", nil); code != 2 || !strings.Contains(r.stderr.String(), "already done") {
		t.Errorf("release after done: exit %d %q", code, r.stderr)
	}
	if r.exec.ran("systemd-run") || r.exec.ran("podman") {
		t.Error("mailbox mode ran a command")
	}
	if onDisk(filepath.Join(r.Root, "jobs", "t0001-w1")) {
		t.Error("mailbox mode touched the helper's jobs")
	}
}

// The key is for one host: the forced command names it, and every verb
// must name the same. Names are job names; nothing else gets through.
func TestMailboxModeRefuses(t *testing.T) {
	r := newRig(t)
	r.queue(t, "desktop", "t0001-w1")
	r.queue(t, "other", "t0002-w1")
	for _, tc := range []struct {
		host, command string
	}{
		{"desktop", "claim other"},
		{"desktop", "beat other t0002-w1"},
		{"desktop", "claim"},
		{"desktop", "claim desktop extra"},
		{"desktop", "beat desktop"},
		{"desktop", "beat desktop ../t0001-w1"},
		{"desktop", "beat desktop T0001"},
		{"desktop", "beat  desktop t0001-w1"},
		{"desktop", "beat desktop t0001-w1 "},
		{"desktop", "beat desktop 't0001-w1'"},
		{"desktop", "start t0001-w1"},
		{"desktop", "status t0001-w1"},
		{"desktop", "collect desktop t0001-w1"},
		{"desktop", "run t0001-w1"},
		{"desktop", "rebuild"},
		{"desktop", "claim desktop; rm -rf ~"},
		{"desktop", "claim desktop\nclaim other"},
		{"desktop", strings.Repeat("a", 200)},
		{"../x", "claim ../x"},
		{"", "claim "},
	} {
		if code := r.mail(t, tc.host, tc.command, nil); code != 2 {
			t.Errorf("%q as %q: exit %d, stdout %q", tc.command, tc.host, code, r.stdout)
		}
		if r.stdout.Len() != 0 {
			t.Errorf("%q: answered %q", tc.command, r.stdout)
		}
	}
	// still queued and unclaimed, and no call counted as the host calling in
	for _, p := range []string{"desktop/t0001-w1/claimed", "other/t0002-w1/claimed", "desktop/last_seen", "other/last_seen"} {
		if onDisk(filepath.Join(r.Root, "mailbox", p)) {
			t.Errorf("%s was written", p)
		}
	}
}

func TestMailboxModeCapsItsInput(t *testing.T) {
	r := newRig(t)
	r.queue(t, "desktop", "t0001-w1")
	r.mail(t, "desktop", "claim desktop", nil)
	if code := r.mail(t, "desktop", "beat desktop t0001-w1", make([]byte, jobproto.MaxBeatInput+1)); code != 2 {
		t.Errorf("oversized beat: exit %d", code)
	}
	big := make([]byte, jobproto.MaxCollectTar+1)
	if code := r.mail(t, "desktop", "done desktop t0001-w1", big); code != 2 || !strings.Contains(r.stderr.String(), "input over") {
		t.Errorf("oversized done: exit %d %q", code, r.stderr)
	}
	if onDisk(filepath.Join(r.Root, "mailbox", "desktop", "t0001-w1", "done.tar")) {
		t.Error("an oversized done was stored")
	}
}

// Mailbox mode is for the forced command only: run by hand, without
// SSH_ORIGINAL_COMMAND, or with more than the host, it refuses; and the
// dispatcher's own key (no arguments) cannot reach it.
func TestMailboxModeOnlyAsAForcedCommand(t *testing.T) {
	r := newRig(t)
	r.queue(t, "desktop", "t0001-w1")
	if code := r.argv(t, jobproto.MailMode, "desktop"); code != 2 {
		t.Errorf("local run: exit %d", code)
	}
	r.reset()
	r.Environ = append(r.Environ, "SSH_ORIGINAL_COMMAND=claim desktop")
	if code := r.Serve(t.Context(), []string{r.Self, jobproto.MailMode, "desktop", "extra"}); code != 2 {
		t.Errorf("extra argv: exit %d", code)
	}
	r.reset()
	if code := r.Serve(t.Context(), []string{r.Self}); code != 2 || r.stdout.Len() != 0 {
		t.Errorf("the dispatcher's key reached the mailbox: exit %d %q", code, r.stdout)
	}
	r.reset()
	if code := r.Serve(t.Context(), []string{filepath.Join(filepath.Dir(r.Self), "factory"), jobproto.MailMode, "desktop"}); code != 2 {
		t.Errorf("as factory: exit %d", code)
	}
}
