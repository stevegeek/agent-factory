package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Answer struct {
	Value, Label, Sub string
	Primary           bool
	Href              string
	NeedsNote         bool
	NoteFor           string // "to send it back": names the answer the note is needed for
	Danger            bool   // closes the ticket or the PR: shown last, in red
}

// mergeable: the gate found the change auto-merge eligible and its PR is open and not a draft,
// so the factory may merge it once the maintainer says so.
func mergeable(t *Ticket) bool {
	return t.Column == "pr_open" && t.Gate != nil && t.Gate.AutoEligible && t.PR != nil && t.PR.Number > 0 && !t.PR.Draft
}

// Answers offered for an ask; Href ones are plain links, the rest submit the ticket's form.
func Answers(t *Ticket, a *Ask) []Answer {
	if a == nil {
		if t.Column == "ready_for_pr" {
			if t.PR != nil && t.PR.Number > 0 {
				return []Answer{{Value: "open_pr", Label: fmt.Sprintf("Update PR #%d", t.PR.Number), Sub: "push the reworked branch; the same PR updates", Primary: true}}
			}
			return []Answer{{Value: "open_pr", Label: "Open the PR", Sub: "push the branch and open it on GitHub", Primary: true}}
		}
		return nil
	}
	// the factory never closes an issue it was given; say so where dropping might suggest it
	issueStays := ""
	if t.Source != nil && t.Source.Issue != 0 {
		issueStays = "the issue stays open on GitHub"
	}
	switch a.Kind {
	case "review":
		var out []Answer
		u := ""
		if t.PR != nil {
			u = safeURL(t.PR.URL)
		}
		if u != "" {
			if mergeable(t) {
				out = append(out, Answer{Value: "merge", Label: "Merge it", Sub: fmt.Sprintf("the factory merges PR #%d and closes the ticket", t.PR.Number), Primary: true},
					Answer{Label: "Read it on GitHub", Sub: "the diff and the checks, before you decide", Href: u})
			} else {
				out = append(out, Answer{Label: fmt.Sprintf("Open PR #%d on GitHub", t.PR.Number),
					Sub: "approve it there and the factory merges it; or request changes for a rework", Primary: true, Href: u})
			}
		}
		out = append(out, Answer{Value: "send_back", Label: "Send back with a note", Sub: "a worker reworks it; the PR is a draft meanwhile", NeedsNote: true, NoteFor: "to send it back"})
		if u != "" {
			out = append(out, Answer{Label: "Close it on GitHub", Sub: "the factory sees it closed and drops the ticket", Href: u, Danger: true})
		}
		return out
	case "open":
		open := Answer{Value: "open_pr", Label: "Open the PR", Sub: "push the branch and open it on GitHub", Primary: true}
		if t.PR != nil && t.PR.Number > 0 {
			open.Label, open.Sub = fmt.Sprintf("Update PR #%d", t.PR.Number), "push the reworked branch; the same PR updates"
		}
		return []Answer{open, {Value: "send_back", Label: "Send back with a note", Sub: "a worker changes it first", NeedsNote: true, NoteFor: "to send it back"}}
	case "start":
		return []Answer{{Value: "start", Label: "Start", Sub: "a worker picks it up on the next tick", Primary: true},
			{Value: "not_now", Label: "Not now", Sub: orSub(issueStays, "the ticket closes"), Danger: true}}
	case "decide":
		return []Answer{{Value: "start", Label: "Start anyway", Sub: "a worker picks it up on the next tick", Primary: true},
			{Value: "reply", Label: "Ask triage again", Sub: "triage reruns with your note", NeedsNote: true, NoteFor: "to ask triage again"},
			{Value: "drop", Label: "Drop", Sub: orSub(issueStays, "the ticket closes"), Danger: true}}
	case "design":
		return []Answer{{Value: "approve", Label: "Approve the ticked ones", Sub: "each goes to Ready on its own", Primary: true},
			{Value: "redirect", Label: "Redirect with a note", Sub: "the designer tries again", NeedsNote: true, NoteFor: "to redirect"}}
	case "proposal":
		return []Answer{{Value: "file", Label: "File it", Sub: "becomes a change ticket in Ready", Primary: true},
			{Value: "drop", Label: "Drop", Sub: "the proposal closes; nothing is filed", Danger: true}}
	case "unstick":
		var out []Answer
		if a.RetryColumn != "" {
			out = append(out, Answer{Value: "retry", Label: "Retry", Sub: "back to " + columnLabel(a.RetryColumn), Primary: true},
				Answer{Value: "retry_note", Label: "Retry with a note", NeedsNote: true, NoteFor: "to retry with it"})
		}
		return append(out, Answer{Value: "drop", Label: "Drop", Sub: "the ticket closes", Danger: true})
	case "notice":
		return []Answer{{Value: "done", Label: "Done", Sub: "close it; nothing else happens", Primary: true}}
	case "question":
		return []Answer{{Value: "answer", Label: "Send the answer", Primary: true, NeedsNote: true}}
	}
	return nil
}

func orSub(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

const maxNote = 4000

var errNoteNeeded = errors.New("that answer needs a note")

// Plan maps an answer to the factory commands that carry it out, without the factory path.
func Plan(t *Ticket, a *Ask, answer, note string, picks []int) ([][]string, error) {
	id := t.ID
	if !validID(id) {
		return nil, errors.New("bad ticket id")
	}
	note = strings.TrimSpace(note)
	if len(note) > maxNote {
		return nil, fmt.Errorf("the note is longer than %d characters", maxNote)
	}
	for _, r := range note {
		if r < 0x20 && r != '\n' && r != '\t' && r != '\r' {
			return nil, errors.New("the note has control characters")
		}
	}
	move := func(col string) []string { return []string{"move", id, col} }
	var cmds [][]string
	need := false
	if answer == "ask" {
		if note == "" {
			return nil, errors.New("write the question first")
		}
		// the question is the whole argument; "--" keeps a leading "-" from reading as an option
		return [][]string{{"ask", id, "--", note}}, nil
	}
	if answer == "note" {
		if t.closed() {
			return nil, errors.New("closed tickets take no notes")
		}
		need = true
	} else if a == nil {
		if answer == "open_pr" && t.Column == "ready_for_pr" {
			return [][]string{{"pr", id}}, nil
		}
		return nil, errors.New("this ticket is not waiting for an answer")
	} else {
		switch a.Kind + ":" + answer {
		case "review:merge":
			if !mergeable(t) {
				return nil, errors.New("this PR is not one the factory may merge; review and merge it on GitHub")
			}
			return [][]string{{"merge", id}}, nil
		case "open:open_pr":
			return [][]string{{"pr", id}}, nil
		case "review:send_back", "open:send_back":
			need = true
			cmds = append(cmds, move("ready"))
		case "start:start", "decide:start":
			cmds = append(cmds, move("ready"))
		case "start:not_now", "decide:drop", "proposal:drop", "unstick:drop":
			cmds = append(cmds, move("rejected"))
		case "decide:reply":
			need = true
			cmds = append(cmds, move("triage"))
		case "question:answer":
			need = true
		case "design:approve":
			n := len(t.Proposed)
			seen := map[int]bool{}
			var only []string
			for _, p := range picks {
				if p < 1 || p > n {
					return nil, fmt.Errorf("pick numbers 1 to %d", n)
				}
				if !seen[p] {
					seen[p] = true
					only = append(only, strconv.Itoa(p))
				}
			}
			switch {
			case len(only) == 0:
				return nil, errors.New("tick at least one ticket to approve")
			case len(only) == n:
				cmds = append(cmds, []string{"approve", id})
			default:
				cmds = append(cmds, []string{"approve", id, "--only", strings.Join(only, ",")})
			}
		case "design:redirect":
			if note == "" {
				return nil, errNoteNeeded
			}
			// the note travels with the redirect itself
			return [][]string{append([]string{"redirect", id}, optArg("--note", note)...)}, nil
		case "proposal:file":
			cmds = append(cmds, []string{"approve", id})
		case "unstick:retry", "unstick:retry_note":
			if a.RetryColumn == "" || !validColumn(a.RetryColumn) {
				return nil, errors.New("there is nothing to retry from; drop it or add a note")
			}
			need = answer == "retry_note"
			cmds = append(cmds, move(a.RetryColumn))
		case "notice:done":
			cmds = append(cmds, move("done"))
		default:
			return nil, fmt.Errorf("%q is not an answer to this ask", answer)
		}
	}
	if need && note == "" {
		return nil, errNoteNeeded
	}
	if note != "" {
		cmds = append([][]string{noteCmd(id, note)}, cmds...)
	}
	return cmds, nil
}

var columns = []string{"triage", "ready", "working", "voting", "review", "security", "gate",
	"ready_for_pr", "pr_open", "needs_human", "done", "rejected"}

func validColumn(c string) bool {
	for _, x := range columns {
		if x == c {
			return true
		}
	}
	return false
}

// a leading "-" would read as an option to the factory's argument parser
func noteCmd(id, text string) []string {
	if strings.HasPrefix(text, "-") {
		return []string{"note", id, "--", text}
	}
	return []string{"note", id, text}
}

func optArg(flag, text string) []string {
	if strings.HasPrefix(text, "-") {
		return []string{flag + "=" + text}
	}
	return []string{flag, text}
}

// Runner runs factory commands one at a time: argv only, never a shell.
type Runner struct {
	Factory, Root string
	mu            sync.Mutex
}

func (r *Runner) Run(cmds [][]string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, args := range cmds {
		// past the CLI's own 60 s wait for a ticket the tick holds, so its "busy" message reaches the person
		limit := 90 * time.Second
		if args[0] == "pr" {
			limit = 10 * time.Minute
		}
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		cmd := exec.CommandContext(ctx, r.Factory, args...)
		cmd.Env = append(os.Environ(), "FACTORY_ROOT="+r.Root)
		cmd.WaitDelay = 5 * time.Second
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		timedOut := ctx.Err() == context.DeadlineExceeded
		cancel()
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			if timedOut {
				msg = fmt.Sprintf("timed out after %s", limit)
			}
			return strings.Join(out, "\n"), fmt.Errorf("factory %s: %s", args[0], tail(msg, 500))
		}
		if s := strings.TrimSpace(stdout.String()); s != "" {
			out = append(out, tail(s, 300))
		}
	}
	return strings.Join(out, "\n"), nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return "…" + s[i:]
}
