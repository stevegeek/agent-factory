package stage

import (
	"fmt"
	"strings"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Public review reads everything a change to a public project would
// publish: every commit, message and patch, and every free text the
// factory sends to GitHub with it. Its report never leaves the board: it
// is not one of the texts a PR prints, and the worker is told its
// findings only as where and why.

// PublicText is one text a PR publishes, by the name a finding's where
// gives it. Fixed is a text the worker cannot change (the branch name, a
// PR title that is the ticket's, the issue line, the updates, the footer): a finding
// there goes to a person rather than back to work.
type PublicText struct {
	Name, Text string
	Fixed      bool
}

// PublicIn is what the pipeline gathered for a public review: the texts the
// PR would carry, in the order the prompt shows them, and the private
// values: the names this project must never carry.
type PublicIn struct {
	Texts   []PublicText
	Private []string
}

// placeOf reads a finding's where: in a commit (its message or a line it
// adds), or in one of the texts the review was shown, by its name, and
// whether that text is fixed. Anything else is not a place the review was
// shown (known false), and the report cannot be read as one.
func placeOf(where string, texts []PublicText) (fixed, known bool) {
	name := strings.TrimSuffix(textx.Lower(firstWord(where)), ":")
	if name == "commit" {
		return false, true
	}
	for _, x := range texts {
		if name == x.Name {
			return x.Fixed, true
		}
	}
	return false, false
}

func firstWord(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// fence is a Markdown code fence longer than any run of backticks in s, so
// that a text cannot close its own block.
func fence(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(3, longest+1))
}

func publicPrompt(in PromptIn) string {
	t, b := in.Ticket, base(in)
	head := t.HeadBeforePublic.Or("HEAD")
	p := "Public review of the change on this branch against `" + b + "`, at " + head + ".\n\n" +
		"Read every commit it adds, message and patch, in full: `git log -p --text --diff-merges=remerge " +
		"--format='commit %H%n%n%B' " + b + "..HEAD`. Every commit is published, not only the final diff. A merge " +
		"shows only what it resolved: what it brings in from `" + b + "` is published already."
	var pub PublicIn
	if in.Public != nil {
		pub = *in.Public
	}
	if len(pub.Texts) > 0 {
		p += "\n\nThe texts the factory sends to GitHub with the change, each as it is published:"
		for _, x := range pub.Texts {
			f := fence(x.Text)
			p += "\n\n### " + x.Name + "\n" + f + "\n" + x.Text + "\n" + f
		}
	}
	if len(pub.Private) > 0 {
		p += "\n\nPrivate values: never in this project's commits or text, in any spelling or case. Never copy one into your report:\n" +
			bullets(pub.Private)
	} else {
		p += "\n\nNo private values are listed for this project; every rule in your instructions still applies."
	}
	// the ticket's title is published only as the PR title, which is shown
	// above when it is; the task line is not a text to report on
	return p + "\n\nWhat the change was meant to do, from the ticket, for context only: this line is not published " +
		"(what is published of it is the pr-title above), so it is never a finding:\nTask: " + t.Title + UntrustedNote(t.Door)
}

// PublicFinding is one finding of a public review: where it is and why it
// is not safe, never the text itself.
type PublicFinding struct{ Where, Why string }

// PublicFindings are the findings of a public review's report, and false
// when the report cannot be read as one: a verdict that is neither pass
// nor block, findings that are not a list of objects each with a where, or
// a block with no findings. A pass that lists findings is a block.
func PublicFindings(v jsonx.Value) ([]PublicFinding, bool) {
	r := ReadReport(v)
	pass, block := r.Is("verdict", "pass"), r.Is("verdict", "block")
	if !pass && !block {
		return nil, false
	}
	var out []PublicFinding
	if fv, ok := r.Value("findings"); ok && fv.Kind() != jsonx.KindNull {
		list, isList := fv.Array()
		if !isList {
			return nil, false
		}
		for _, e := range list {
			o, isObj := e.Object()
			if !isObj {
				return nil, false
			}
			fr := Report{o: o}
			w, _ := fr.Value("where")
			where, isStr := w.Str()
			if where = textx.Strip(where); !isStr || where == "" {
				return nil, false
			}
			out = append(out, PublicFinding{Where: textx.Clip(where, 200), Why: textx.Clip(textx.Strip(fr.Str("why")), 500)})
		}
	}
	if block && len(out) == 0 {
		return nil, false
	}
	return out, true
}

// publicRuns counts the public review runs since the ticket last moved: a
// run whose report could not be read leaves the ticket where it is, so a
// second one is the second run in a row.
func publicRuns(t *ticket.Ticket) int {
	n := 0
	for _, e := range t.History {
		switch d := e.Data.(type) {
		case ticket.Moved:
			n = 0
		case ticket.Started:
			if d.Stage == string(model.StagePublic) {
				n++
			}
		}
	}
	return n
}

// lastReason is the reason of the ticket's last move, "" when it has none.
func lastReason(t *ticket.Ticket) model.Reason {
	for i := len(t.History) - 1; i >= 0; i-- {
		if m, ok := t.History[i].Data.(ticket.Moved); ok {
			return m.Reason
		}
	}
	return ""
}

// public: a pass records the head it read and goes to the gate. A block
// goes back to the worker once, with the findings as its notes; a second
// block, or a finding in a text the worker cannot change (the branch, the
// PR title, the ticket, the footer), goes to a person. A report that cannot
// be read is read again once, then goes to a person as a run that left no
// report, to read again or drop: it never passes, and it is no rework. A
// finding whose where is neither a commit nor a text the review was shown
// makes the report one that cannot be read: it cannot be routed.
func (f *finisher) public(r Report) {
	t := f.Ticket
	if f.committed(model.StagePublic) {
		return
	}
	var texts []PublicText
	if f.Public != nil {
		texts = f.Public.Texts
	}
	found, ok := PublicFindings(f.Report)
	fixed := 0
	for _, x := range found {
		isFixed, known := placeOf(x.Where, texts)
		ok = ok && known
		if isFixed {
			fixed++
		}
	}
	if !ok {
		if publicRuns(t) < 2 {
			ticket.Event(t, "error", "public review's report could not be read; it reads the change again", "",
				ticket.Errored{Text: "public review's report could not be read"}, f.Now)
			return
		}
		f.move(model.ColNeedsHuman, model.ReasonNoReport, "public review's report could not be read twice: "+textx.Prefix(r.Str("summary"), 150),
			"public review never passes a report it cannot read")
		return
	}
	if len(found) == 0 {
		t.PublicHead = ticket.Some(f.Facts.Head)
		f.pin()
		f.move(model.ColGate, model.ReasonPublicPassed, "", "")
		return
	}
	n := len(found)
	switch {
	case fixed > 0:
		f.move(model.ColNeedsHuman, model.ReasonPublicBlocked,
			fmt.Sprintf("public review blocked it: %d finding(s), %d in text the worker cannot change", n, fixed),
			"a finding in the branch, the PR title, the ticket, the updates or the footer goes to a person")
	case ticket.PublicRounds(t) >= 1:
		f.move(model.ColNeedsHuman, model.ReasonPublicBlocked,
			fmt.Sprintf("public review blocked it again after a rework: %d finding(s)", n),
			"public review sends a change back to work once")
	default:
		f.move(model.ColReady, model.ReasonPublicChanges, fmt.Sprintf("public review found %d finding(s); back to the worker", n), "")
	}
}

// publicRework is the work prompt's clause after public review sent the
// change back: where each finding is and why, and how to take the text out
// of every commit.
func publicRework(t *ticket.Ticket) string {
	if lastReason(t) != model.ReasonPublicChanges {
		return ""
	}
	v, _ := t.Reports.Lookup(string(model.StagePublic))
	found, _ := PublicFindings(v)
	if len(found) == 0 {
		return ""
	}
	lines := make([]string, len(found))
	for i, x := range found {
		lines[i] = x.Where + ": " + x.Why
	}
	return "\n\nThis project is public, and a public review of the previous attempt on this branch found text that must " +
		"not be published. Each finding says where it is and why:\n" + bullets(lines) +
		"\n\nEvery commit on the branch is published, so rewrite this branch's own commits (amend or squash them) until " +
		"no commit carries that text, then commit. Do not describe the removal in a commit message, your summary or your rework note."
}
