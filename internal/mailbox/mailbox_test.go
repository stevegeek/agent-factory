package mailbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

var t0 = time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)

type rig struct {
	Box
	clk *clock.Step
}

func newRig(t *testing.T) *rig {
	t.Helper()
	clk := clock.NewStep(t0, 0)
	return &rig{Box{Dir: filepath.Join(t.TempDir(), "mailbox"), Clock: clk, ClaimWindow: 30 * time.Minute, BeatWindow: 10 * time.Minute}, clk}
}

func startTar(t *testing.T, prompt string) []byte {
	t.Helper()
	meta := jobproto.Meta{Branch: "bot/t0001", Start: "main", Model: "m", Bundle: true, TimeoutMinutes: 45}
	b, err := jobproto.TarFlat([]jobproto.File{
		{Name: jobproto.MemberPrompt, Data: []byte(prompt)},
		{Name: jobproto.MemberRole, Data: []byte("role")},
		{Name: jobproto.MemberMeta, Data: meta.Encode()},
		{Name: "memory/notes.md", Data: []byte("mem")},
	}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func collectTar(t *testing.T) []byte {
	t.Helper()
	b, err := jobproto.PackCollect(map[string][]byte{jobproto.MemberReport: []byte(`{"status": "done"}`), jobproto.MemberHead: []byte("abc\n")})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (r *rig) state(t *testing.T, job string) string {
	t.Helper()
	st, err := r.State("desktop", job)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// queued → claimed (running) → beats → done → collected → removed.
func TestAJobsLife(t *testing.T) {
	r := newRig(t)
	in := startTar(t, "do it")
	if err := r.Start("desktop", "t0001-w1", in); err != nil {
		t.Fatal(err)
	}
	if st := r.state(t, "t0001-w1"); st != jobproto.StateQueued {
		t.Fatalf("after start: %s", st)
	}
	if err := r.Start("desktop", "t0001-w1", in); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second start: %v", err)
	}
	job, tar, err := r.Claim("desktop")
	if err != nil || job != "t0001-w1" || string(tar) != string(in) {
		t.Fatalf("claim: %q %v", job, err)
	}
	if _, _, err := r.Claim("desktop"); !errors.Is(err, ErrNoJob) {
		t.Errorf("a claimed job was claimed again: %v", err)
	}
	if st := r.state(t, "t0001-w1"); st != jobproto.StateRunning {
		t.Fatalf("after claim: %s", st)
	}
	r.clk.Advance(8 * time.Minute)
	if ans, err := r.Beat("desktop", "t0001-w1", []byte("working")); ans != jobproto.BeatOK || err != nil {
		t.Fatalf("beat: %q %v", ans, err)
	}
	r.clk.Advance(8 * time.Minute) // 16 minutes after the claim, 8 after the beat
	if st := r.state(t, "t0001-w1"); st != jobproto.StateRunning {
		t.Errorf("a beat keeps it running: %s", st)
	}
	if s, _ := r.Tail(context.Background(), jobproto.Host{Name: "desktop"}, "t0001-w1"); s != "working" {
		t.Errorf("tail %q", s)
	}
	if err := r.Done("desktop", "t0001-w1", collectTar(t)); err != nil {
		t.Fatal(err)
	}
	if st := r.state(t, "t0001-w1"); st != jobproto.StateDone {
		t.Fatalf("after done: %s", st)
	}
	if err := r.Done("desktop", "t0001-w1", collectTar(t)); err == nil {
		t.Error("done twice")
	}
	out, err := r.Collect("desktop", "t0001-w1")
	if err != nil {
		t.Fatal(err)
	}
	files, err := jobproto.UntarFlat(out)
	if err != nil || string(files[jobproto.MemberHead]) != "abc\n" || len(files) != 2 {
		t.Errorf("collect: %q %v", files, err)
	}
	if err := r.Remove("desktop", "t0001-w1"); err != nil {
		t.Fatal(err)
	}
	if st := r.state(t, "t0001-w1"); st != jobproto.StateMissing {
		t.Errorf("after remove: %s", st)
	}
	if names, _ := r.Jobs("desktop"); len(names) != 0 {
		t.Errorf("left %q", names)
	}
	if entries, _ := os.ReadDir(filepath.Join(r.Dir, "desktop")); len(entries) != 1 || entries[0].Name() != jobproto.MailSeen {
		t.Errorf("host dir holds %v", entries)
	}
}

func TestClaimTakesTheEarliestDeadlineAndSkipsExpired(t *testing.T) {
	r := newRig(t)
	r.Start("desktop", "t0002-w1", startTar(t, "2"))
	r.clk.Advance(time.Minute)
	r.Start("desktop", "t0001-w1", startTar(t, "1"))
	r.Start("other", "t0003-w1", startTar(t, "3"))
	job, _, err := r.Claim("desktop")
	if err != nil || job != "t0002-w1" {
		t.Fatalf("first claim %q %v", job, err)
	}
	r.clk.Advance(31 * time.Minute) // t0001's claim window is over
	if st := r.state(t, "t0001-w1"); st != jobproto.StateMissing {
		t.Errorf("unclaimed past its deadline: %s", st)
	}
	if job, _, err := r.Claim("desktop"); !errors.Is(err, ErrNoJob) {
		t.Errorf("claimed %q past its deadline: %v", job, err)
	}
}

func TestAClaimedJobWithNoBeatGoesMissing(t *testing.T) {
	r := newRig(t)
	r.Start("desktop", "t0001-w1", startTar(t, "p"))
	r.Claim("desktop")
	r.clk.Advance(10 * time.Minute)
	if st := r.state(t, "t0001-w1"); st != jobproto.StateRunning {
		t.Errorf("at the limit: %s", st)
	}
	r.clk.Advance(time.Second)
	if st := r.state(t, "t0001-w1"); st != jobproto.StateMissing {
		t.Errorf("no beat for longer than Beat: %s", st)
	}
}

func TestReleaseRequeuesWithANewDeadline(t *testing.T) {
	r := newRig(t)
	r.Start("desktop", "t0001-w1", startTar(t, "p"))
	r.Claim("desktop")
	r.Beat("desktop", "t0001-w1", []byte("x"))
	r.clk.Advance(25 * time.Minute)
	r.Beat("desktop", "t0001-w1", []byte("y"))
	if err := r.Release("desktop", "t0001-w1"); err != nil {
		t.Fatal(err)
	}
	r.clk.Advance(20 * time.Minute) // 45 minutes after the start: the first deadline is gone
	if st := r.state(t, "t0001-w1"); st != jobproto.StateQueued {
		t.Errorf("after release: %s", st)
	}
	if s, _ := r.Tail(context.Background(), jobproto.Host{Name: "desktop"}, "t0001-w1"); s != "" {
		t.Errorf("a released job keeps its old log: %q", s)
	}
	if job, _, err := r.Claim("desktop"); err != nil || job != "t0001-w1" {
		t.Errorf("reclaim %q %v", job, err)
	}
	for _, f := range []func() error{
		func() error { return r.Release("desktop", "t0009-w1") },
		func() error { r.Done("desktop", "t0001-w1", collectTar(t)); return r.Release("desktop", "t0001-w1") },
	} {
		if err := f(); err == nil {
			t.Error("released a job that is gone or done")
		}
	}
}

// The factory removing a job (its timeout, a person) is the host's cancel.
func TestBeatAnswersCancelForARemovedOrUnclaimedJob(t *testing.T) {
	r := newRig(t)
	r.Start("desktop", "t0001-w1", startTar(t, "p"))
	if ans, _ := r.Beat("desktop", "t0001-w1", nil); ans != jobproto.BeatCancel {
		t.Errorf("unclaimed: %q", ans)
	}
	r.Claim("desktop")
	r.Remove("desktop", "t0001-w1")
	if ans, _ := r.Beat("desktop", "t0001-w1", []byte("x")); ans != jobproto.BeatCancel {
		t.Errorf("removed: %q", ans)
	}
	if err := r.Done("desktop", "t0001-w1", collectTar(t)); err == nil {
		t.Error("done for a removed job")
	}
}

func TestDoneRefusesABadTar(t *testing.T) {
	r := newRig(t)
	r.Start("desktop", "t0001-w1", startTar(t, "p"))
	if err := r.Done("desktop", "t0001-w1", collectTar(t)); err == nil {
		t.Error("done before a claim")
	}
	r.Claim("desktop")
	bad, _ := jobproto.TarFlat([]jobproto.File{{Name: "id_ed25519", Data: []byte("k")}}, t0)
	if err := r.Done("desktop", "t0001-w1", bad); err == nil || !strings.Contains(err.Error(), "not a collect member") {
		t.Errorf("bad tar: %v", err)
	}
	if st := r.state(t, "t0001-w1"); st != jobproto.StateRunning {
		t.Errorf("a refused done changed the job: %s", st)
	}
}

func TestStartRefusesBadInput(t *testing.T) {
	r := newRig(t)
	noMeta, _ := jobproto.TarFlat([]jobproto.File{{Name: "prompt.md", Data: []byte("p")}}, t0)
	badMeta, _ := jobproto.TarFlat([]jobproto.File{{Name: "meta.json", Data: []byte(`{"branch": "../x", "start": "main"}`)}}, t0)
	for name, in := range map[string][]byte{"no meta": noMeta, "bad meta": badMeta, "not a tar": []byte("x"),
		"too large": make([]byte, jobproto.MaxInputBytes+1)} {
		if err := r.Start("desktop", "t0001-w1", in); err == nil {
			t.Errorf("%s: started", name)
		}
	}
	for _, names := range [][2]string{{"../x", "t0001-w1"}, {"desktop", "T0001"}, {"desktop", ".gone"}} {
		if err := r.Start(names[0], names[1], startTar(t, "p")); err == nil {
			t.Errorf("%q started", names)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(r.Dir, "desktop")); len(entries) != 0 {
		t.Errorf("a refused start left %v", entries)
	}
}

func TestAwayUntilTheHostCallsIn(t *testing.T) {
	r := newRig(t)
	if !r.Away("desktop") {
		t.Error("never seen, not away")
	}
	r.Claim("desktop")
	if r.Away("desktop") {
		t.Error("just claimed, away")
	}
	r.clk.Advance(11 * time.Minute)
	if !r.Away("desktop") {
		t.Error("silent for longer than Beat, not away")
	}
}

// Do answers the helper's job verbs as factory-remote does, and every
// failure is a RemoteError (the tick's "try another host").
func TestDoSpeaksTheHelperProtocol(t *testing.T) {
	r := newRig(t)
	h := jobproto.Host{Name: "desktop"}
	ctx := context.Background()
	out, err := r.Do(ctx, h, jobproto.VerbStart, "t0001-w1", startTar(t, "p"), 0)
	if err != nil || string(out) != "{\"started\": \"t0001-w1\"}\n" {
		t.Fatalf("start %q %v", out, err)
	}
	out, err = r.Do(ctx, h, jobproto.VerbStatus, "t0001-w1", nil, 0)
	if st, _ := jobproto.ParseStatus(out); err != nil || st != jobproto.StateQueued {
		t.Errorf("status %q %v", out, err)
	}
	out, err = r.Do(ctx, h, jobproto.VerbList, "", nil, 0)
	if err != nil || string(out) != "t0001-w1\n" {
		t.Errorf("list %q %v", out, err)
	}
	if names, err := r.List(ctx, h); err != nil || !reflect.DeepEqual(names, []string{"t0001-w1"}) {
		t.Errorf("List %q %v", names, err)
	}
	_, err = r.Do(ctx, h, jobproto.VerbCollect, "t0001-w1", nil, 0)
	var re *jobproto.RemoteError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "not done") {
		t.Errorf("collect before done: %v", err)
	}
	if _, err := r.Do(ctx, h, jobproto.VerbStart, "t0001-w1", startTar(t, "p"), 0); !errors.As(err, &re) {
		t.Errorf("start twice: %v", err)
	}
	if _, err := r.Do(ctx, jobproto.Host{Name: "../x"}, jobproto.VerbStatus, "t0001-w1", nil, 0); !errors.As(err, &re) {
		t.Errorf("bad host: %v", err)
	}
	out, err = r.Do(ctx, h, jobproto.VerbRemove, "t0001-w1", nil, 0)
	if err != nil || string(out) != "{\"removed\": \"t0001-w1\"}\n" {
		t.Errorf("remove %q %v", out, err)
	}
	out, _ = r.Do(ctx, h, jobproto.VerbStatus, "t0001-w1", nil, 0)
	if st, _ := jobproto.ParseStatus(out); st != jobproto.StateMissing {
		t.Errorf("status after remove %q", out)
	}
}

// A job directory or file planted as a link is never followed.
func TestLinksAreNotFollowed(t *testing.T) {
	r := newRig(t)
	r.Start("desktop", "t0001-w1", startTar(t, "p"))
	jd := filepath.Join(r.Dir, "desktop", "t0001-w1")
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("s"), 0o600)
	os.Remove(filepath.Join(jd, jobproto.MailStartTar))
	os.Symlink(secret, filepath.Join(jd, jobproto.MailStartTar))
	if job, tar, err := r.Claim("desktop"); err == nil {
		t.Errorf("claimed %s through a link: %q", job, tar)
	}
	os.Symlink(jd, filepath.Join(r.Dir, "desktop", "t0002-w1"))
	if st := r.state(t, "t0002-w1"); st != jobproto.StateMissing {
		t.Errorf("a linked job dir reads as %s", st)
	}
}
