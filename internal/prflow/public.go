package prflow

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The names of the free texts a PR publishes beside its commits, as public
// review's prompt and template name them in a finding's where.
const (
	TextBranch  = "branch"   // the branch name
	TextTitle   = "pr-title" // the PR title
	TextTicket  = "ticket"   // the "Fixes #N" line of the issue the ticket came from
	TextSummary = "summary"  // the work summary (the PR body and a rework's comment)
	TextTests   = "tests"    // how the worker tested it
	TextLint    = "lint"     // the worker's lint
	TextReview  = "review"   // the review's verdict and runtime-behaviour answer
	TextFooter  = "footer"   // [identity] pr_footer
)

// PublicText is one free text a PR publishes, by its name. Fixed is a text
// the worker cannot change: the branch, a title that is the ticket's own,
// the issue line and the footer.
type PublicText struct {
	Name, Text string
	Fixed      bool
}

// PublicTexts are the free texts OpenPR would send to GitHub for t, as it
// sends them (PRTitle, the texts PRBody prints, the footer; a rework's
// comment is the summary and the footer), in the order public review is
// shown them; an empty one is left out. The rest of the PR body is fixed
// text and the diff's file names, which the commits carry.
func PublicTexts(cfg *config.Config, p config.Project, t *ticket.Ticket) []PublicText {
	texts := gate.PublicTexts(t)
	review := ""
	if v, ok := texts["the review verdict"]; ok {
		review = "verdict: " + v
	}
	if v, ok := texts["the review runtime_behaviour"]; ok {
		review = textx.Strip(review + "\nchanges runtime behaviour for users: " + v)
	}
	issue := ""
	if src, ok := t.Source.Get(); ok && src.Issue != 0 {
		issue = "Fixes #" + strconv.FormatInt(src.Issue, 10)
	}
	// a title made from the work summary is the worker's to change
	title := PRTitle(cfg, p, t)
	var out []PublicText
	for _, x := range []PublicText{
		{TextBranch, t.Branch.Or(""), true},
		{TextTitle, title, title == t.Title},
		{TextTicket, issue, true},
		{TextSummary, texts["the work summary"], false},
		{TextTests, texts["the work tests"], false},
		{TextLint, texts["the work lint"], false},
		{TextReview, review, false},
		{TextFooter, cfg.Identity.PRFooter, true},
	} {
		if textx.Strip(x.Text) != "" {
			out = append(out, x)
		}
	}
	return out
}

// PublicDigest names texts by their content: the same names and texts in
// the same order give the same digest, any change another (Fixed is not
// part of it).
func PublicDigest(texts []PublicText) string {
	h := sha256.New()
	for _, x := range texts {
		h.Write([]byte(x.Name + "\x00" + x.Text + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// PublicPassed reports a change public review passed as it would be
// published at head: its pass names head, and the texts it read beside the
// commits are the ones OpenPR would send now. A text that changed with no
// new commit (a rerun's summary) is read again.
func PublicPassed(cfg *config.Config, p config.Project, t *ticket.Ticket, head string) bool {
	return head != "" && t.PublicHead.Or("") == head &&
		t.TextsBeforePublic.Or("") == PublicDigest(PublicTexts(cfg, p, t))
}
