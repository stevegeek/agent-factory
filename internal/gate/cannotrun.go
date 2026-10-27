package gate

import (
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// OutputLines is how many of the failed checks' last output lines the
// ticket keeps (ticket.Gate.Output), and OutputLineMax how long each may be,
// in code points.
const (
	OutputLines   = 10
	OutputLineMax = 300
)

// FailTail is what the ticket keeps of failed checks' output: its last
// OutputLines lines that are not blank, each cut to OutputLineMax.
func FailTail(output string) string {
	var keep []string
	lines := textx.SplitLines(output)
	for i := len(lines) - 1; i >= 0 && len(keep) < OutputLines; i-- {
		if l := textx.RStrip(lines[i]); textx.Strip(l) != "" {
			keep = append(keep, textx.Prefix(l, OutputLineMax))
		}
	}
	slices.Reverse(keep)
	return strings.Join(keep, "\n")
}

// CannotRun reports checks that did not run because the host lacks a
// command, and what it lacks ("go: not found"): that is the host's
// problem, not the change's. An exit of 126 or 127 (the shell's "cannot
// execute" and "not found") is one whatever the command; a "not found"
// line without that exit is one only for a command the project's own
// install, test or lint starts. The output comes from another machine: it
// is matched, never followed.
func CannotRun(p config.Project, exit ticket.Opt[int], output string) (string, bool) {
	code, hasExit := exit.Get()
	byExit := hasExit && (code == 126 || code == 127)
	tools := projectTools(p)
	lines := textx.SplitLines(output)
	for i := len(lines) - 1; i >= 0; i-- {
		tool, what, ok := notFound(lines[i])
		if ok && (byExit || slices.Contains(tools, tool)) {
			return what, true
		}
	}
	if byExit {
		return fmt.Sprintf("the checks exited %d: a command could not run", code), true
	}
	return "", false
}

// notFound reads a shell's line for a missing command ("sh: 1: go: not
// found", "bash: line 1: go: command not found"): the command's base name,
// and the line from the command on.
func notFound(line string) (tool, what string, ok bool) {
	line = textx.Strip(line)
	for _, suffix := range []string{": command not found", ": not found"} {
		before, found := strings.CutSuffix(line, suffix)
		if !found {
			continue
		}
		cmd := before[strings.LastIndex(before, ": ")+1:]
		cmd = textx.Strip(cmd)
		if cmd == "" || strings.ContainsFunc(cmd, textx.IsSpace) {
			return "", "", false
		}
		return path.Base(cmd), cmd + suffix, true
	}
	return "", "", false
}

// projectTools are the base names of the commands the project's install,
// test and lint start: the first word of each simple command, after any
// VAR=value assignments.
func projectTools(p config.Project) []string {
	var out []string
	for _, c := range []string{p.Install, p.Test, p.Lint} {
		for _, seg := range strings.FieldsFunc(c, func(r rune) bool { return strings.ContainsRune(";&|(){}`\n", r) }) {
			for _, w := range strings.Fields(seg) {
				if strings.Contains(w, "=") || w == "exec" || w == "env" || w == "command" || w == "time" || w == "!" {
					continue
				}
				if t := path.Base(w); !slices.Contains(out, t) {
					out = append(out, t)
				}
				break
			}
		}
	}
	return out
}
