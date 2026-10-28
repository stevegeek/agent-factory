package gate

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func TestCannotRun(t *testing.T) {
	p := config.Project{Install: "go version", Test: "go vet ./... && GOFLAGS=-mod=mod go test ./...",
		Lint: `out=$(gofmt -l .); [ -z "$out" ] || { echo "gofmt: $out"; exit 1; }`}
	none := ticket.Opt[int]{}
	for _, c := range []struct {
		name   string
		exit   ticket.Opt[int]
		output string
		want   string
		ok     bool
	}{
		{"dash, exit 127", ticket.Some(127), "checks.sh: 1: go: not found\n", "go: not found", true},
		{"bash, exit 127", ticket.Some(127), "bash: line 1: go: command not found\n", "go: command not found", true},
		{"exit 126, no line", ticket.Some(126), "permission denied\n", "the checks exited 126: a command could not run", true},
		{"exit 127, any command", ticket.Some(127), "sh: 1: bundle: not found\n", "bundle: not found", true},
		{"own tool, no exit", none, "ok\nsh: 1: gofmt: not found\n", "gofmt: not found", true},
		{"own tool by path, exit 2", ticket.Some(2), "sh: 1: /usr/local/go/bin/go: not found", "/usr/local/go/bin/go: not found", true},
		{"another tool, exit 1", ticket.Some(1), "sh: 1: sqlite3: not found\n", "", false},
		{"failing tests", ticket.Some(1), "--- FAIL: TestX (0.00s)\nFAIL\tp\t0.1s\n", "", false},
		{"a test's own words", ticket.Some(1), "want: file: not found\n", "", false},
	} {
		got, ok := CannotRun(p, c.exit, c.output)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: %q %v, want %q %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestFailTail(t *testing.T) {
	var lines []string
	for i := range 14 {
		lines = append(lines, strings.Repeat("x", i), "")
	}
	lines = append(lines, strings.Repeat("y", OutputLineMax+5)+"  ")
	got := strings.Split(FailTail(strings.Join(lines, "\n")), "\n")
	if len(got) != OutputLines || got[0] != strings.Repeat("x", 5) || got[OutputLines-1] != strings.Repeat("y", OutputLineMax) {
		t.Fatalf("%q", got)
	}
	if FailTail("\n \n") != "" {
		t.Fatal("blank output kept")
	}
}
