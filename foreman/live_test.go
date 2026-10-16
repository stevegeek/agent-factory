package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveShowsRunningAgentsAndTheirScreens(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	body := h.do("GET", "/live", nil, c).Body.String()
	for _, want := range []string{`id="T0011"`, `data-poll="/live/T0011/screen"`, `data-poll="/live/log"`, "Speed up widgets", `>Live <span class="n">1</span>`} {
		if !strings.Contains(body, want) {
			t.Errorf("live page lacks %q", want)
		}
	}
	// the screen comes from `factory look`, outside the answer queue
	w := h.do("GET", "/live/T0011/screen", nil, c)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("screen: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if got := h.argv(); len(got) != 1 || strings.Join(got[0], " ") != "look T0011 --lines 150" {
		t.Errorf("look argv: %v", got)
	}
	// cached for a few seconds: a second page costs nothing
	h.do("GET", "/live/T0011/screen", nil, c)
	if got := h.argv(); len(got) != 0 {
		t.Errorf("a second look within seconds ran the CLI again: %v", got)
	}
	// a ticket without an agent never runs the CLI
	if w := h.do("GET", "/live/T0001/screen", nil, c); !strings.Contains(w.Body.String(), "finished") {
		t.Errorf("no agent: %q", w.Body.String())
	}
	if got := h.argv(); len(got) != 0 {
		t.Errorf("look ran for a ticket with no agent: %v", got)
	}
	if w := h.do("GET", "/live/log", nil); w.Code != 303 {
		t.Errorf("the log needs a login: %d", w.Code)
	}
}

func TestAgentLogsOnlyFromTheTicketsOwnRuns(t *testing.T) {
	h := newHarness(t, false)
	c := h.login()
	root := t.TempDir()
	h.srv.runner.Root = root
	os.MkdirAll(filepath.Join(root, "logs", "T0001", "t0001-w1"), 0o755)
	os.WriteFile(filepath.Join(root, "logs", "T0001", "t0001-w1", "agent.log"), []byte("line one\nline two\n"), 0o644)
	os.WriteFile(filepath.Join(root, "logs", "factory.log"), []byte("2026 start T0001\n2026 gate pass\n"), 0o644)
	if w := h.do("GET", "/t/T0001/logs/t0001-w1", nil, c); w.Code != 200 || w.Body.String() != "line one\nline two" {
		t.Errorf("log: %d %q", w.Code, w.Body.String())
	}
	for _, bad := range []string{"/t/T0001/logs/t0002-w1", "/t/T0001/logs/..", "/t/T0001/logs/t0001-w1%2F..%2F..", "/t/T0001/logs/t0001-w9"} {
		if w := h.do("GET", bad, nil, c); w.Code == 200 {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := h.do("GET", "/live/log", nil, c); !strings.HasSuffix(w.Body.String(), "gate pass") {
		t.Errorf("factory log: %q", w.Body.String())
	}
	if body := h.do("GET", "/t/T0001", nil, c).Body.String(); !strings.Contains(body, `href="/t/T0001/logs/t0001-w1"`) {
		t.Errorf("the ticket page should link its agent logs")
	}
}

func TestTailFileKeepsWholeLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	os.WriteFile(p, []byte("aaaa\nbbbb\ncccc\n"), 0o644)
	if got := tailFile(p, 8, 0); got != "cccc" {
		t.Errorf("tail by bytes: %q", got)
	}
	if got := tailFile(p, 100, 2); got != "bbbb\ncccc" {
		t.Errorf("tail by lines: %q", got)
	}
}
