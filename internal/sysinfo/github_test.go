package sysinfo

import (
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/paths"
)

var ghNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

const dnsErr = "<urlopen error [Errno -2] Name or service not known>"

func TestOneFailedPassDoesNotFlipGitHubToUnreachable(t *testing.T) {
	g := GitHub{LastOK: ghNow.Add(-5 * time.Minute)}
	if line := g.Note(ghNow, false, dnsErr); line != "" || g.Down || g.Failed != 1 {
		t.Fatalf("line %q, state %+v", line, g)
	}
	if got := g.Line(time.UTC); got != "GitHub: connected · last poll 1 Mar 11:55" {
		t.Errorf("line = %q", got)
	}
}

func TestFailedPassesInARowFlipGitHubToUnreachableOnce(t *testing.T) {
	g := GitHub{LastOK: ghNow.Add(-5 * time.Minute)}
	var lines []string
	for i := range 4 {
		if line := g.Note(ghNow.Add(time.Duration(i)*5*time.Minute), false, dnsErr); line != "" {
			lines = append(lines, line)
		}
	}
	want := "github: unreachable since 12:00 (last error: " + dnsErr + ")"
	if len(lines) != 1 || lines[0] != want || !g.Down || g.Failed != 4 {
		t.Fatalf("lines %q, state %+v", lines, g)
	}
	if got := g.Line(time.UTC); got != "GitHub: unreachable since 12:00 (last error: "+dnsErr+") · last good poll 1 Mar 11:55" {
		t.Errorf("line = %q", got)
	}
}

func TestTheFirstAnswerClearsUnreachableAndSaysSoOnce(t *testing.T) {
	g := GitHub{}
	for i := range 3 {
		g.Note(ghNow.Add(time.Duration(i)*5*time.Minute), false, dnsErr)
	}
	back := ghNow.Add(15 * time.Minute)
	if line := g.Note(back, true, ""); line != "github: reachable again after 3 failed passes" {
		t.Fatalf("line = %q", line)
	}
	if g != (GitHub{LastOK: back}) {
		t.Fatalf("state %+v", g)
	}
	if line := g.Note(back.Add(5*time.Minute), true, ""); line != "" {
		t.Errorf("second answer said %q", line)
	}
	if got := g.Line(time.UTC); got != "GitHub: connected · last poll 1 Mar 12:20" {
		t.Errorf("line = %q", got)
	}
}

func TestAFailureThatDidNotFlipIsForgottenOnAnAnswer(t *testing.T) {
	g := GitHub{}
	g.Note(ghNow, false, dnsErr)
	if line := g.Note(ghNow.Add(5*time.Minute), true, ""); line != "" || g.Failed != 0 || g.LastError != "" {
		t.Fatalf("line %q, state %+v", line, g)
	}
}

func TestGitHubRecordKeepsTheStateAcrossTicks(t *testing.T) {
	p := paths.Paths{Root: t.TempDir()}
	if got, down := GitHubStatus(p); got != "" || down {
		t.Fatalf("status before any tick = %q", got)
	}
	r := GitHubRecord{Paths: p}
	var lines []string
	for i, ok := range []bool{true, false, false, false, true} {
		line, err := r.Note(ghNow.Add(time.Duration(i)*5*time.Minute), ok, dnsErr)
		if err != nil {
			t.Fatal(err)
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "github: unreachable since 12:05 ") || lines[1] != "github: reachable again after 3 failed passes" {
		t.Fatalf("lines = %q", lines)
	}
	g, err := ReadGitHub(GitHubFile(p))
	if err != nil || g.Down || !g.LastOK.Equal(ghNow.Add(20*time.Minute)) {
		t.Fatalf("state %+v, %v", g, err)
	}
	if got, down := GitHubStatus(p); down || !strings.HasPrefix(got, "GitHub: connected · last poll ") {
		t.Errorf("status = %q", got)
	}
}

func TestGitHubStatusIsEmptyWithoutARoot(t *testing.T) {
	if got, _ := GitHubStatus(paths.Paths{}); got != "" {
		t.Errorf("status = %q", got)
	}
}
