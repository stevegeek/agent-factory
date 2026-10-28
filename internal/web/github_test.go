package web

import (
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/sysinfo"
)

func (h *harness) writeGitHub(g sysinfo.GitHub) {
	h.t.Helper()
	if err := sysinfo.WriteGitHub(sysinfo.GitHubFile(h.paths), g); err != nil {
		h.t.Fatal(err)
	}
}

func TestTheBoardSaysGitHubIsConnected(t *testing.T) {
	h := newHarness(t, false)
	h.writeGitHub(sysinfo.GitHub{LastOK: testNow.Add(-3 * time.Minute)})
	page := h.do("GET", "/", nil, h.login()).Body.String()
	if !strings.Contains(page, `<p class="github" role="status">GitHub: connected · last poll `) {
		t.Errorf("page:\n%s", page)
	}
}

func TestTheBoardSaysGitHubIsUnreachable(t *testing.T) {
	h := newHarness(t, false)
	h.writeGitHub(sysinfo.GitHub{LastOK: testNow.Add(-20 * time.Minute), Failed: 3,
		FailingSince: testNow.Add(-15 * time.Minute), LastError: "no such host", Down: true})
	page := h.do("GET", "/", nil, h.login()).Body.String()
	if !strings.Contains(page, `<p class="github down" role="status">GitHub: unreachable since `) ||
		!strings.Contains(page, "(last error: no such host) · last good poll ") {
		t.Errorf("page:\n%s", page)
	}
}

func TestTheBoardHasNoGitHubLineBeforeTheTickRecordsOne(t *testing.T) {
	h := newHarness(t, false)
	if page := h.do("GET", "/", nil, h.login()).Body.String(); strings.Contains(page, `class="github`) {
		t.Errorf("page:\n%s", page)
	}
}
