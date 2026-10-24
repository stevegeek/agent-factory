package usage

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
)

// line is one assistant line of a transcript, as Claude Code writes it.
func line(id, model string, in, out, cw, cr int64) string {
	return fmt.Sprintf(`{"type":"assistant","sessionId":"s","message":{"id":%q,"model":%q,"role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d}}}`,
		id, model, in, out, cw, cr)
}

func writeFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func projects(home string) string { return filepath.Join(home, ".claude", "projects") }

// A message written several times while it streams counts once, with its
// final (largest) usage; models are kept apart; other lines are not counted.
func TestReadCountsEachMessageOnceWithItsLastUsage(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(projects(home), "-repo", "a.jsonl"),
		`{"type":"user","message":{"role":"user","content":"do it"}}`,
		line("m1", "claude-opus", 10, 1, 100, 0),
		line("m1", "claude-opus", 10, 50, 100, 0),
		line("m2", "claude-opus", 2, 20, 0, 300),
		line("m3", "claude-haiku", 5, 5, 0, 0),
		`{"type":"summary","summary":"x"}`,
	)
	// a subagent's transcript, one level deeper, and the first message again
	writeFile(t, filepath.Join(projects(home), "-repo", "a", "subagents", "agent-1.jsonl"),
		line("m4", "claude-haiku", 1, 2, 3, 4),
		line("m1", "claude-opus", 10, 50, 100, 0),
	)
	u, err := Read(home)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := u.Models["claude-opus"], (Tokens{Input: 12, Output: 70, CacheWrite: 100, CacheRead: 300, Messages: 2}); got != want {
		t.Errorf("opus %+v, want %+v", got, want)
	}
	if got, want := u.Models["claude-haiku"], (Tokens{Input: 6, Output: 7, CacheWrite: 3, CacheRead: 4, Messages: 2}); got != want {
		t.Errorf("haiku %+v, want %+v", got, want)
	}
	if len(u.Models) != 2 || u.Partial {
		t.Errorf("usage %+v", u)
	}
	if s := u.Sum(); s.Total() != 12+70+100+300+6+7+3+4 || s.Messages != 4 {
		t.Errorf("sum %+v total %d", s, s.Total())
	}
}

// No home, no transcript (a Pi agent, a crash before the first message):
// zero usage and no error.
func TestReadWithNoTranscriptIsZero(t *testing.T) {
	for _, home := range []string{filepath.Join(t.TempDir(), "gone"), t.TempDir()} {
		u, err := Read(home)
		if err != nil || !u.Empty() {
			t.Errorf("%s: %+v %v", home, u, err)
		}
	}
}

// Malformed lines are skipped, numbers are clamped to sane non-negative
// values, odd model names are cleaned, and a line with no id is not counted.
func TestReadSkipsMalformedAndClampsNumbers(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(projects(home), "p", "s.jsonl"),
		`{"type":"assistant","message":{"id":"x","usage":`, // cut off
		`not json at all`,
		`{"type":"assistant","message":{"id":"neg","model":"m","usage":{"input_tokens":-5,"output_tokens":1.5,"cache_creation_input_tokens":"7","cache_read_input_tokens":1e300}}}`,
		`{"type":"assistant","message":{"id":"big","model":"m","usage":{"input_tokens":99999999999999999999999,"output_tokens":3}}}`,
		`{"type":"assistant","message":{"model":"m","usage":{"input_tokens":5}}}`,
		`{"type":"assistant","message":{"id":"odd","model":"bad\u0000name<script>`+strings.Repeat("y", 200)+`","usage":{"input_tokens":1}}}`,
		`{"type":"assistant","message":{"id":"none","usage":{"input_tokens":1}}}`,
		`{"type":"assistant","message":{"id":"syn","model":"<synthetic>","usage":{"input_tokens":0}}}`,
	)
	u, err := Read(home)
	if err != nil {
		t.Fatal(err)
	}
	m := u.Models["m"]
	if m.Input != MaxMessageInput || m.Output != 3+1 || m.CacheWrite != 0 || m.CacheRead != MaxMessageInput || m.Messages != 2 {
		t.Errorf("m %+v", m)
	}
	for name, tk := range u.Models {
		if name == "m" {
			continue
		}
		if len(name) > MaxModelName || strings.ContainsAny(name, "\x00<>") {
			t.Errorf("model name %q kept", name)
		}
		if tk.Messages != 1 || tk.Input != 1 {
			t.Errorf("%q %+v", name, tk)
		}
	}
	if _, ok := u.Models[Unknown]; !ok {
		t.Errorf("no %q model in %v", Unknown, u.Models)
	}
	if _, ok := u.Models["<synthetic>"]; ok {
		t.Error("a synthetic message is counted")
	}
}

// A link anywhere (the agent can write its own home) is never followed, a
// fifo never stalls the read, and the files and bytes read are capped.
func TestReadFollowsNoLinkAndIsCapped(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "projects", "p", "s.jsonl"), line("o1", "m", 1000, 0, 0, 0))
	writeFile(t, filepath.Join(outside, "s.jsonl"), line("o2", "m", 1000, 0, 0, 0))

	home := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, ".claude")); err != nil {
		t.Fatal(err)
	}
	if u, err := Read(home); err != nil || !u.Empty() {
		t.Errorf("linked .claude: %+v %v", u, err)
	}

	home = t.TempDir()
	writeFile(t, filepath.Join(projects(home), "p", "real.jsonl"), line("r1", "m", 1, 0, 0, 0))
	for link, target := range map[string]string{
		filepath.Join(projects(home), "p", "link.jsonl"): filepath.Join(outside, "s.jsonl"),
		filepath.Join(projects(home), "linkdir"):         filepath.Join(outside, "projects", "p"),
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(projects(home), "p", "fifo.jsonl"), 0o644); err != nil {
		t.Fatal(err)
	}
	u, err := Read(home)
	if err != nil || u.Models["m"].Input != 1 || u.Partial {
		t.Errorf("links: %+v %v", u, err)
	}

	home = t.TempDir()
	for i := range 5 {
		writeFile(t, filepath.Join(projects(home), "p", fmt.Sprintf("s%d.jsonl", i)), line(fmt.Sprint("c", i), "m", 1, 0, 0, 0))
	}
	u, err = read(home, limits{files: 3, dirs: 100, bytes: 1 << 20, line: 1 << 10, messages: 100, dirNames: 100, names: 1000})
	if err != nil || u.Models["m"].Messages != 3 || !u.Partial {
		t.Errorf("file cap: %+v %v", u, err)
	}
	u, err = read(home, limits{files: 100, dirs: 100, bytes: 1 << 20, line: 1 << 10, messages: 2, dirNames: 100, names: 1000})
	if err != nil || u.Models["m"].Messages != 2 || !u.Partial {
		t.Errorf("message cap: %+v %v", u, err)
	}
	// a line over the line cap is skipped, and the next one still counts
	home = t.TempDir()
	writeFile(t, filepath.Join(projects(home), "p", "s.jsonl"),
		line("long", "m", 1, 0, 0, 0)[:20]+strings.Repeat("z", 5000)+`"}`, line("short", "m", 2, 0, 0, 0))
	u, err = read(home, limits{files: 10, dirs: 10, bytes: 1 << 20, line: 1 << 10, messages: 100, dirNames: 100, names: 1000})
	if err != nil || u.Models["m"].Input != 2 || u.Models["m"].Messages != 1 {
		t.Errorf("line cap: %+v %v", u, err)
	}
	u, err = read(home, limits{files: 10, dirs: 10, bytes: 100, line: 1 << 10, messages: 100, dirNames: 100, names: 1000})
	if err != nil || !u.Partial {
		t.Errorf("byte cap: %+v %v", u, err)
	}
}

// A message id read again later with smaller numbers (a resumed or copied
// session holding an earlier streaming line) keeps the largest of each,
// whatever order the files are read in.
func TestReadKeepsTheLargestUsageOfAnID(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(projects(home), "p", "a.jsonl"), line("m1", "claude-opus", 10, 500, 100, 7))
	writeFile(t, filepath.Join(projects(home), "p", "b.jsonl"), line("m1", "claude-opus", 10, 1, 100, 9))
	u, err := Read(home)
	if err != nil || u.Models["claude-opus"] != (Tokens{Input: 10, Output: 500, CacheWrite: 100, CacheRead: 9, Messages: 1}) {
		t.Errorf("usage %+v %v", u, err)
	}
}

// One message can spend no more than a context window takes in and an
// output limit lets out: a forged number is cut to that, per message.
func TestReadClampsEachMessageToWhatOneCanSpend(t *testing.T) {
	home := t.TempDir()
	var lines []string
	for i := range 3 {
		lines = append(lines, line(fmt.Sprint("f", i), "m", 1<<40, 1<<40, 1<<40, 1<<40))
	}
	writeFile(t, filepath.Join(projects(home), "p", "s.jsonl"), lines...)
	u, err := Read(home)
	want := Tokens{Input: 3 * MaxMessageInput, Output: 3 * MaxMessageOutput, CacheWrite: 3 * MaxMessageInput, CacheRead: 3 * MaxMessageInput, Messages: 3}
	if err != nil || u.Models["m"] != want {
		t.Errorf("usage %+v %v, want %+v", u, err, want)
	}
}

// What the reader keeps per message is small whatever the agent wrote: an
// id over MaxID is skipped, and a long model name is kept cleaned (cut to
// MaxModelName), never whole.
func TestReadKeepsNoOverlongIDOrModel(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(projects(home), "p", "s.jsonl"),
		line(strings.Repeat("i", MaxID+1), "m", 5, 0, 0, 0),
		line(strings.Repeat("j", MaxID), "m", 1, 0, 0, 0),
		line("k", strings.Repeat("n", 1<<20), 2, 0, 0, 0))
	r := &reader{lim: defaultLimits(), msgs: map[string]message{}}
	if err := fsx.WalkNoFollowMax(home, r.lim.dirNames, r.dir); err != nil {
		t.Fatal(err)
	}
	if len(r.msgs) != 2 {
		t.Errorf("messages kept %d, want 2", len(r.msgs))
	}
	for id, m := range r.msgs {
		if len(id) > MaxID || len(m.model) > MaxModelName {
			t.Errorf("kept an id of %d bytes, a model of %d", len(id), len(m.model))
		}
	}
}

// A directory of more names than the walk lists, or more names over the
// walk than it takes, stops the read short and marks it partial.
func TestReadCapsTheNamesListed(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(projects(home), "p")
	writeFile(t, filepath.Join(dir, "a.jsonl"), line("m1", "m", 1, 0, 0, 0))
	for i := range 30 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("junk%d", i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lim := defaultLimits()
	if u, err := read(home, lim); err != nil || u.Partial || u.Models["m"].Messages != 1 {
		t.Errorf("within the caps: %+v %v", u, err)
	}
	lim.dirNames = 10
	if u, err := read(home, lim); err != nil || !u.Partial {
		t.Errorf("a directory over its cap: %+v %v", u, err)
	}
	lim = defaultLimits()
	lim.names = 20
	if u, err := read(home, lim); err != nil || !u.Partial {
		t.Errorf("a walk over its cap: %+v %v", u, err)
	}
}

// usage.json round-trips, and one from elsewhere (a helper's) is cleaned
// as a transcript is.
func TestParseCleansAndRoundTrips(t *testing.T) {
	u := Usage{Models: map[string]Tokens{"claude-opus": {Input: 1, Output: 2, CacheWrite: 3, CacheRead: 4, Messages: 1}}}
	b := u.JSON()
	if !bytes.HasSuffix(b, []byte("\n")) || !bytes.Contains(b, []byte(`"cache_write": 3`)) {
		t.Errorf("json %s", b)
	}
	back, err := Parse(b)
	if err != nil || back.Models["claude-opus"] != u.Models["claude-opus"] {
		t.Errorf("parse %+v %v", back, err)
	}
	got, err := Parse([]byte(`{"models": {"a\u0001b": {"input": -3, "output": 99999999999999}, "": {"input": 1}}}`))
	if err != nil || got.Models["ab"].Input != 0 || got.Models["ab"].Output != MaxTokens || got.Models[Unknown].Input != 1 {
		t.Errorf("hostile %+v %v", got, err)
	}
	if _, err := Parse([]byte(`[1]`)); err == nil {
		t.Error("an array parsed")
	}
}

// Concurrent appends from several finishing runs leave one whole line
// each; a corrupt line in the log is skipped by readers.
func TestAppendIsWholeLinesUnderConcurrency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := Record{At: "2026-01-02T03:04:05+00:00", Ticket: fmt.Sprintf("T%04d", i), Project: "gem", Stage: "work", Run: "t-w1", Host: "local",
				Models: map[string]Tokens{"m": {Input: int64(i), Output: 1, Messages: 1}}}
			if err := Append(path, r); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{\"at\": broken\n")
	f.Close()
	recs, err := Load(path, time.Time{})
	if err != nil || len(recs) != 20 {
		t.Fatalf("%d records, %v", len(recs), err)
	}
	seen := map[string]bool{}
	for _, r := range recs {
		seen[r.Ticket] = true
	}
	if len(seen) != 20 {
		t.Errorf("tickets %v", seen)
	}
	if recs, err := Load(filepath.Join(t.TempDir(), "none"), time.Time{}); err != nil || recs != nil {
		t.Errorf("missing log: %v %v", recs, err)
	}
}

// A log over the read cap is read from its end: the newest runs count.
func TestLoadReadsTheEndOfALongLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	for i := range 50 {
		if err := Append(path, Record{At: "2026-01-02T03:04:05+00:00", Ticket: fmt.Sprintf("T%04d", i), Models: map[string]Tokens{"m": {Input: 1}}}); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := load(path, 1000, time.Time{})
	if err != nil || len(recs) == 0 || len(recs) >= 50 || recs[len(recs)-1].Ticket != "T0049" {
		t.Fatalf("%d records, %v", len(recs), err)
	}
}

// A cut that lands just after a newline drops nothing: the first line
// read is then whole and kept. One that lands inside a line drops only it.
func TestLoadCutOnALineBoundaryKeepsTheLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	var sizes []int64
	for i := range 3 {
		if err := Append(path, Record{At: "2026-01-02T03:04:05+00:00", Ticket: fmt.Sprintf("T%04d", i), Models: map[string]Tokens{"m": {Input: 1}}}); err != nil {
			t.Fatal(err)
		}
		st, _ := os.Stat(path)
		sizes = append(sizes, st.Size())
	}
	// the last two lines exactly
	recs, err := load(path, sizes[2]-sizes[0], time.Time{})
	if err != nil || len(recs) != 2 || recs[0].Ticket != "T0001" {
		t.Errorf("cut on a boundary: %+v %v", recs, err)
	}
	// one byte less: the cut is inside T0001's line
	recs, err = load(path, sizes[2]-sizes[0]-1, time.Time{})
	if err != nil || len(recs) != 1 || recs[0].Ticket != "T0002" {
		t.Errorf("cut inside a line: %+v %v", recs, err)
	}
}

// An append cut short (no newline at the end) costs only its own line:
// the next append starts a line of its own.
func TestAppendAfterATornLineKeepsTheNext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := Append(path, Record{At: "2026-01-02T03:04:05+00:00", Ticket: "T0001", Models: map[string]Tokens{"m": {Input: 1}}}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"at": "2026-01-02T03:04:05+00:00", "ticket": "T00`)
	f.Close()
	if err := Append(path, Record{At: "2026-01-02T03:04:06+00:00", Ticket: "T0003", Models: map[string]Tokens{"m": {Input: 3}}}); err != nil {
		t.Fatal(err)
	}
	recs, err := Load(path, time.Time{})
	if err != nil || len(recs) != 2 || recs[0].Ticket != "T0001" || recs[1].Ticket != "T0003" {
		t.Errorf("records %+v %v", recs, err)
	}
}

// Load keeps only the window's records, skips an overlong line, and keeps
// a run's Partial, which Sum counts per row.
func TestLoadKeepsTheWindowAndPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	for _, r := range []Record{
		{At: "2026-01-01T00:00:00+00:00", Ticket: "T0001", Run: "a", Models: map[string]Tokens{"m": {Input: 1}}},
		{At: "2026-01-05T00:00:00+00:00", Ticket: "T0002", Run: "b", Models: map[string]Tokens{"m": {Input: 2}}, Partial: true},
		{At: "2026-01-06T00:00:00+00:00", Ticket: "T0003", Run: "c", Project: strings.Repeat("p", MaxLogLine), Models: map[string]Tokens{"m": {Input: 4}}},
		{At: "2026-01-06T00:00:00+00:00", Ticket: "T0004", Run: "d", Models: map[string]Tokens{"m": {Input: 8}}},
	} {
		if err := Append(path, r); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := Load(path, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || len(recs) != 2 || recs[0].Ticket != "T0002" || !recs[0].Partial || recs[1].Ticket != "T0004" {
		t.Fatalf("records %+v %v", recs, err)
	}
	rows, _ := Sum(recs, time.Time{}, ByStage)
	if len(rows) != 1 || rows[0].Runs != 2 || rows[0].Partial != 1 || PartialRuns(recs) != 1 {
		t.Errorf("rows %+v", rows)
	}
}

func TestSumByKeyLargestFirst(t *testing.T) {
	at := func(d int) string {
		return time.Date(2026, 1, 10-d, 12, 0, 0, 0, time.UTC).Format("2006-01-02T15:04:05+00:00")
	}
	recs := []Record{
		{At: at(0), Ticket: "T0001", Project: "gem", Stage: "work", Host: "local", Models: map[string]Tokens{"opus": {Input: 10, Output: 5, Messages: 2}}},
		{At: at(1), Ticket: "T0001", Project: "gem", Stage: "review", Host: "hostx", Models: map[string]Tokens{"opus": {Input: 100}, "haiku": {Output: 1}}},
		{At: at(2), Ticket: "T0002", Project: "app", Stage: "work", Host: "local", Models: map[string]Tokens{"haiku": {Input: 1000}}},
		{At: at(9), Ticket: "T0003", Project: "app", Stage: "work", Host: "local", Models: map[string]Tokens{"haiku": {Input: 5000}}},
		{At: "not a time", Ticket: "T0004", Project: "app", Models: map[string]Tokens{"haiku": {Input: 7}}},
	}
	since := time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC)
	rows, total := Sum(recs, since, ByProject)
	if len(rows) != 2 || rows[0].Key != "app" || rows[0].Tokens.Input != 1000 || rows[1].Key != "gem" || rows[1].Runs != 2 {
		t.Errorf("by project %+v", rows)
	}
	if total.Total() != 10+5+100+1+1000 || total.Messages != 2 {
		t.Errorf("total %+v", total)
	}
	rows, _ = Sum(recs, since, ByModel)
	if len(rows) != 2 || rows[0].Key != "haiku" || rows[0].Tokens.Total() != 1001 || rows[1].Runs != 2 {
		t.Errorf("by model %+v", rows)
	}
	rows, _ = Sum(recs, since, ByStage)
	if rows[0].Key != "work" || rows[0].Runs != 2 {
		t.Errorf("by stage %+v", rows)
	}
	if rows, _ := Sum(recs, since, ByHost); rows[0].Key != "local" {
		t.Errorf("by host %+v", rows)
	}
	if rows, _ := Sum(recs, since, ByTicket); rows[0].Key != "T0002" {
		t.Errorf("by ticket %+v", rows)
	}
	if !ValidBy("project") || ValidBy("cost") {
		t.Error("ValidBy")
	}
}

// A run recorded twice counts once, as its last line.
func TestLoadCountsARunOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	for _, r := range []Record{
		{At: "2026-01-02T03:04:05+00:00", Ticket: "T0001", Run: "t0001-w1", Models: map[string]Tokens{"m": {Input: 1}}},
		{At: "2026-01-02T03:04:05+00:00", Ticket: "T0001", Run: "t0001-r1", Models: map[string]Tokens{"m": {Input: 5}}},
		{At: "2026-01-02T03:09:05+00:00", Ticket: "T0001", Run: "t0001-w1", Models: map[string]Tokens{"m": {Input: 2}}},
	} {
		if err := Append(path, r); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := Load(path, time.Time{})
	if err != nil || len(recs) != 2 || recs[0].Run != "t0001-w1" || recs[0].Models["m"].Input != 2 || recs[1].Run != "t0001-r1" {
		t.Errorf("records %+v %v", recs, err)
	}
	if got := OfTicket(recs, "T0001"); len(got) != 2 {
		t.Errorf("of ticket %+v", got)
	}
}

func TestCommas(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -12345: "-12,345"} {
		if got := Commas(n); got != want {
			t.Errorf("Commas(%d) = %q, want %q", n, got, want)
		}
	}
}
