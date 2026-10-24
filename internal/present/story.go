package present

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Step is one line of "How it got here".
type Step struct {
	At, End time.Time
	When    string
	Who     string
	What    string
	Quotes  []Quote
	Tone    string // "now", "flag" or ""
}

// Quote is one voter's score and reason.
type Quote struct {
	Score int
	Text  string
	Who   string
}

// Story is "How it got here": the ticket's history, reports, votes and gate
// as a few lines for a person.
type Story struct {
	Steps  []Step
	Caveat string
}

// recentSteps is how many of the latest steps show before "earlier steps"
// folds the rest away.
const recentSteps = 5

// Earlier are the steps folded away, so the page opens on what happened
// last; nil when the story is short.
func (s Story) Earlier() []Step {
	if len(s.Steps) <= recentSteps+1 {
		return nil
	}
	return s.Steps[:len(s.Steps)-recentSteps]
}

// Recent are the steps that show.
func (s Story) Recent() []Step {
	if len(s.Steps) <= recentSteps+1 {
		return s.Steps
	}
	return s.Steps[len(s.Steps)-recentSteps:]
}

// BuildStory tells how t got where it is. all holds every ticket: a
// proposal's prospect, an asked question's task.
func BuildStory(t *ticket.Ticket, all map[ticket.ID]*ticket.Ticket) Story {
	return new(reader).story(t, all)
}

func (r *reader) story(t *ticket.Ticket, all map[ticket.ID]*ticket.Ticket) Story {
	var s Story
	if t.KindName() == string(model.KindProposal) {
		if p := all[parentOf(t)]; p != nil {
			s.Steps = append(s.Steps, r.prospectOrigin(p)...)
		}
	}
	h := t.History
	used := make([]bool, len(h))
	lastStart := map[string]int{}
	for i, e := range h {
		if e.Event == "start" {
			if stage, _, _, ok := r.runOf(e); ok {
				lastStart[stage] = i
			}
		}
	}
	for i, e := range h {
		if used[i] {
			continue
		}
		var st *Step
		switch e.Event {
		case "created":
			st = createdStep(t, e)
		case "start":
			stage, agent, host, ok := r.runOf(e)
			if !ok {
				break
			}
			if stage == string(model.StageVote) {
				st = r.voteGroup(t, h, i, used)
				break
			}
			st = &Step{At: ParseTime(e.At), Who: RoleLabel(stage)}
			if host != "" {
				st.Who += " on " + host
			}
			if j := r.finishedAt(h, i, stage, agent); j >= 0 {
				used[j] = true
				st.End = ParseTime(h[j].At)
				if lastStart[stage] == i {
					st.What = stageWhat(t, stage)
				} else {
					st.What = "An earlier attempt."
				}
			} else if a, ok := agentOf(t); ok && a.Name == agent {
				st.What, st.Tone = "Running now.", "now"
			} else {
				st.What, st.Tone = "Stopped without a report.", "flag"
			}
		case "move":
			st = r.moveStep(t, e)
		case "error":
			n := 1
			for j := i + 1; j < len(h) && h[j].Event == "error" && h[j].Detail == e.Detail; j++ {
				used[j] = true
				n++
			}
			st = &Step{At: ParseTime(e.At), Who: "Dispatcher", What: sentence("error: " + Clip(e.Detail, 300)), Tone: "flag"}
			if n > 1 {
				st.What += fmt.Sprintf(" (%d times)", n)
			}
		case "note":
			st = &Step{At: ParseTime(e.At), Who: "You", What: "Note: " + Clip(e.Detail, 400)}
		case "comment":
			st = &Step{At: ParseTime(e.At), Who: firstNonEmpty(byOf(e), "Agent"), What: "Comment: " + Clip(commentText(t, e), 400)}
		case "question":
			q := boardQuestionOf(t, e)
			what := "Asked: " + e.Detail
			if q != nil {
				what = "Asked " + q.Recipient + ": " + q.Text
			}
			st = &Step{At: ParseTime(e.At), Who: firstNonEmpty(byOf(e), "Agent"), What: Clip(what, 400)}
		case "answer":
			what := e.Detail
			if q := boardQuestionOf(t, e); q != nil && q.Answer.Present() {
				what = "Answered: " + q.Answer.Or("")
			}
			st = &Step{At: ParseTime(e.At), Who: firstNonEmpty(byOf(e), "Someone"), What: Clip(what, 400)}
		case "memory":
			st = r.memoryStep(e)
			if e.Rule != "" {
				st.What += " " + sentence(e.Rule)
				st.Tone = "flag"
			}
		case "asked":
			s.Steps = append(s.Steps, r.askSteps(e, all)...)
			continue
		case "panel":
			s.Steps = append(s.Steps, r.panelSteps(t, all, e)...)
			continue
		}
		if st == nil && e.Rule != "" && e.Event != "move" {
			st = &Step{At: ParseTime(e.At), Who: "Dispatcher"}
		}
		if st == nil {
			continue
		}
		if e.Rule != "" && (st.Who == "Dispatcher" || st.Who == "Gate") {
			st.What = sentence(e.Rule)
		}
		s.Steps = append(s.Steps, *st)
	}
	if a := AskFor(t); a != nil {
		s.Steps = append(s.Steps, Step{Who: "Waiting for you", What: WaitingWhat(t, a), Tone: "now", When: "now"})
	}
	if v := panelVotes(t, all); len(v) > 1 {
		host := v[0].Host
		for _, x := range v {
			if x.Host != host {
				host = ""
			}
		}
		if host != "" {
			s.Caveat = fmt.Sprintf("All %d voters ran on %s, so matching scores say less than independent setups agreeing would.", len(v), host)
		}
	}
	formatWhen(s.Steps)
	return s
}

func (r *reader) finishedAt(h []ticket.Entry, from int, stage, agent string) int {
	for j := from + 1; j < len(h); j++ {
		if h[j].Event != "finished" {
			continue
		}
		if s, a, _, ok := r.runOf(h[j]); ok && s == stage && a == agent {
			return j
		}
	}
	return -1
}

// memoryStep is what an agent will remember next time, a store over its size
// limit (a warning), or a run whose notes never came back.
func (r *reader) memoryStep(e ticket.Entry) *Step {
	st := &Step{At: ParseTime(e.At), Who: "Memory", What: sentence(e.Detail)}
	if m, ok := e.Data.(ticket.Memory); ok {
		switch {
		case m.Commit != "":
			st.What = CapFirst(m.Role) + " remembered: " + strings.Join(m.Files, ", ") + "."
		case m.LostOn == "":
			st.Tone = "flag"
		}
		return st
	}
	// "worker memory 1a2b3c4: knowledge/x.md, tickets/T0042.md": the files
	// as written, with any "…" the dispatcher added
	if m, ok := r.interpret(e).(ticket.Memory); ok && m.Commit != "" {
		_, files, _ := strings.Cut(e.Detail, ": ")
		st.What = CapFirst(m.Role) + " remembered: " + files + "."
	} else if strings.Contains(e.Detail, "over its limit") {
		st.Tone = "flag"
	}
	return st
}

func createdStep(t *ticket.Ticket, e ticket.Entry) *Step {
	st := &Step{At: ParseTime(e.At)}
	kind := t.KindName()
	switch {
	case kind == string(model.KindProposal):
		return nil
	case t.Door == string(model.DoorFinding):
		st.Who = "You"
		if t.By == "factory" {
			st.Who = "Dispatcher"
		}
		if r, ok := t.FoundIn.Get(); ok {
			st.What = fmt.Sprintf("Filed finding %d of %s (verified).", r.N, r.Ticket)
		} else {
			st.What = "Filed it from a verified finding."
		}
	case t.Door == string(model.DoorAgent):
		st.Who = "Agent"
		st.What = "Filed by " + firstNonEmpty(t.By, "an agent") + "."
	case t.Door == string(model.DoorGitHub):
		st.Who = "GitHub sweep"
		if n := issueOf(t); n > 0 {
			st.What = fmt.Sprintf("Filed issue #%d for triage.", n)
		} else {
			st.What = "Filed an issue for triage."
		}
	case t.Door == string(model.DoorClock):
		st.Who = "Clock"
		switch {
		case kind == string(model.KindProspect):
			st.What = "Started a prospecting run: " + t.Title
		case t.Project == model.NoticeProject:
			st.What = "Filed a notice: " + t.Title
		default:
			st.What = "Filed a scheduled check: " + t.Title
		}
	case parentOf(t) != "":
		st.Who = "You"
		st.What = "Approved it in " + string(parentOf(t)) + "."
	case t.Door == string(model.DoorPeer):
		st.Who, st.What = "Peer", "Filed it."
	default:
		st.Who = "You"
		switch kind {
		case string(model.KindIntent):
			st.What = "Filed the idea."
		case string(model.KindTask):
			st.What = "Asked: " + t.Title
		default:
			st.What = "Filed it."
		}
	}
	return st
}

func stageWhat(t *ticket.Ticket, stage string) string {
	r := reportOf(t, stage)
	switch model.Stage(stage) {
	case model.StageTriage:
		reason := Clip(r.str("reason"), 400)
		switch r.str("decision") {
		case "accept":
			return strings.TrimSpace("Accepted. " + reason)
		case "reject":
			return strings.TrimSpace("Advised closing it. " + reason)
		}
		return strings.TrimSpace("Could not decide. " + reason)
	case model.StageWork:
		if r.str("status") != "done" {
			return strings.TrimSpace("Blocked. " + Clip(r.str("summary"), 300))
		}
		return Clip(r.str("summary"), 400)
	case model.StageReview:
		v := "Asked for changes."
		if r.str("verdict") == "pass" {
			v = "Pass."
		}
		if b := r.str("runtime_behaviour"); b != "" {
			v += " Changes runtime behaviour: " + b + "."
		}
		return strings.TrimSpace(v + " " + Clip(r.str("summary"), 300))
	case model.StageSecurity:
		return SecurityLine(t)
	case model.StageTask:
		if r.str("status") != "done" {
			return strings.TrimSpace("Blocked. " + Clip(r.str("summary"), 300))
		}
		return strings.TrimSpace("Answered. " + Clip(r.str("summary"), 300))
	case model.StageDesign:
		return strings.TrimSpace(Clip(r.str("summary"), 300) + " " + sentence("proposed "+Plural(len(t.Proposed), "ticket")))
	case model.StageProspect:
		return sentence(fmt.Sprintf("proposed %s with evidence", Plural(len(t.Proposals), "change")))
	case model.StageVerify:
		if r.str("status") != "done" || len(t.Findings) == 0 {
			return strings.TrimSpace("Blocked. " + Clip(r.str("summary"), 300))
		}
		return verifiedLine(t)
	}
	return sentence(Clip(r.str("summary"), 300))
}

// prRef is a PR link in a move's words.
var prRef = regexp.MustCompile(`/pull/(\d+)`) // re: go

func (r *reader) moveStep(t *ticket.Ticket, e ticket.Entry) *Step {
	m := r.moveOf(e)
	to, detail := m.To, m.Text
	st := &Step{At: ParseTime(e.At), Who: "Dispatcher"}
	human := m.Human && (detail == "" || strings.HasPrefix(detail, "by human"))
	switch {
	case human:
		st.Who = "You"
		switch to {
		case string(model.ColDone):
			st.What = "Closed it."
		case string(model.ColRejected):
			st.What = "Dropped it."
		default:
			st.What = "Moved it to " + ColumnLabel(to) + "."
		}
	case to == string(model.ColPROpen):
		n := ""
		if m := prRef.FindStringSubmatch(detail); m != nil {
			n = " #" + m[1]
		}
		if strings.HasPrefix(detail, "draft") {
			st.What = "Opened draft PR" + n + " and asked for review."
		} else {
			st.What = "Opened PR" + n + "."
		}
	case m.Is(model.ReasonPRMerged, strings.HasPrefix(detail, "PR merged by ")):
		st.Who, st.What = "GitHub", "Merged by "+strings.TrimPrefix(detail, "PR merged by ")+"."
	case m.Is(model.ReasonPRClosed, strings.HasPrefix(detail, "PR closed")):
		st.Who, st.What, st.Tone = "GitHub", "The PR was closed without merging.", "flag"
	case m.Is(model.ReasonTriageAccepted, strings.HasPrefix(detail, "triage accepted")):
		switch {
		case to == string(model.ColReady):
			st.What = "Triage accepted it, so it went straight to Ready."
		case t.Door == string(model.DoorGitHub):
			st.What = "Issues from GitHub wait for your go-ahead after triage, so it stopped here."
		default:
			st.What = "Triage accepted it; it waits for you to start it."
		}
	case m.Is(model.ReasonTriageUnsure, strings.HasPrefix(detail, "triage unsure")),
		m.Is(model.ReasonDesignReady, strings.HasPrefix(detail, "design ready")),
		m.Is(model.ReasonTaskDone, strings.HasPrefix(detail, "task done")),
		m.Is(model.ReasonProposalsReady, strings.HasSuffix(detail, "blind panel next")):
		return nil
	case m.Is(model.ReasonTriageRejected, strings.HasPrefix(detail, "triage: ")):
		st.What, st.Tone = "Closed it, as triage advised.", "flag"
	case m.Is(model.ReasonGateFailed, strings.HasPrefix(detail, "gate:")):
		st.Who, st.What, st.Tone = "Gate", sentence(strings.TrimSpace(strings.TrimPrefix(detail, "gate:"))), "flag"
	case to == string(model.ColReadyForPR) && detail == "auto-merge eligible":
		st.Who, st.What = "Gate", "Tests and lint pass, no blockers: auto-merge eligible."
	case to == string(model.ColReadyForPR) && strings.HasPrefix(detail, "needs human review: "):
		// the same plain words as the draft callout: a list of paths becomes a count
		why := strings.Join(DraftReasons(strings.Split(strings.TrimPrefix(detail, "needs human review: "), "; ")), "; ")
		st.Who, st.What = "Gate", sentence("Tests and lint pass. Needs a maintainer: "+why)
	case m.Is(model.ReasonReviewChanges, detail == "review asked for changes"):
		st.Who, st.What = "Reviewer", "Asked for changes; back to the worker."
	case m.Is(model.ReasonRedirected, strings.HasPrefix(detail, "redirected: ")):
		st.Who, st.What = "You", "Sent the design back: "+strings.TrimPrefix(detail, "redirected: ")
	case m.Is(model.ReasonDesignApproved, strings.HasPrefix(detail, "design approved; filed ")):
		st.Who, st.What = "You", "Approved the design; filed "+strings.TrimPrefix(detail, "design approved; filed ")+"."
	case m.Is(model.ReasonProposalApproved, strings.HasPrefix(detail, "proposal approved; filed ")):
		st.Who, st.What = "You", "Filed it as a change: "+strings.TrimPrefix(detail, "proposal approved; filed ")+"."
	case m.Is(model.ReasonFindingsReported, false):
		st.What = "The task reported " + reportedCount(detail) + " findings; a verifier checks them before any becomes a ticket."
	case m.Is(model.ReasonVerifyRequested, false):
		st.Who, st.What = "You", "Sent its findings to the verifier."
	case m.Is(model.ReasonFindingsVerified, false):
		return nil // the verifier's own step says it
	case m.Is(model.ReasonFindingsFiled, false):
		if byOf(e) != "factory" {
			st.Who = "You"
		}
		st.What = "Filed the findings as " + strings.TrimPrefix(detail, "findings filed: ") + "."
	case m.Is(model.ReasonFindingsDismissed, false):
		st.Who, st.What = "You", "Closed it; no findings filed."
	case m.Is(model.ReasonDeployHeld, false):
		st.What = "Deploys are held, so this deploy of main waits for your go-ahead."
		if rev, ok := strings.CutPrefix(detail, "now "); ok {
			st.What = "Another merge offered a deploy; main is now " + rev + "."
		}
	case m.Is(model.ReasonDeployRequested, false):
		if byOf(e) != "factory" {
			st.Who = "You"
		}
		st.What = "Asked the factory to deploy main; the next tick starts it."
	case m.Is(model.ReasonOperatorSettled, false):
		if byOf(e) != "factory" {
			st.Who, st.What = "You", "Asked the factory to apply the manager's changes."
		} else {
			st.What = sentence(Clip(detail, 300))
		}
	case m.Is(model.ReasonDeployStarted, false):
		st.What = "Started the deploy of main."
	case m.Is(model.ReasonPanelDone, strings.HasPrefix(detail, "panel kept ")):
		st.What = "The panel kept " + strings.TrimPrefix(detail, "panel kept ") + "."
	case m.Is(model.ReasonProspectEmpty, strings.HasPrefix(detail, "prospecting found nothing")):
		st.Who, st.What = "Prospector", "Found nothing worth proposing."
	case m.Reason != "" && flaggedReason(m.Reason),
		m.Reason == "" && (strings.HasPrefix(detail, "task blocked") || strings.HasPrefix(detail, "agent ") || strings.HasPrefix(detail, "worker ") ||
			strings.HasPrefix(detail, "designer ") || strings.HasPrefix(detail, "security:") || strings.HasPrefix(detail, "review still") ||
			strings.Contains(detail, "changed the branch") || detail == "dispatcher error"):
		st.What, st.Tone = sentence(Clip(detail, 300)), "flag"
	case detail == "":
		if e.Rule == "" {
			return nil
		}
	case m.Reason != "" && byOf(e) == "factory":
		st.What = sentence(Clip(detail, 300))
	default:
		// a person's own words from `factory move --note`
		st.Who, st.What = "You", "Moved it to "+ColumnLabel(to)+": "+Clip(detail, 300)
	}
	return st
}

func (r *reader) voteGroup(t *ticket.Ticket, h []ticket.Entry, i int, used []bool) *Step {
	st := &Step{At: ParseTime(h[i].At), Who: "Panel"}
	end, n := i, 0
	for j := i; j < len(h); j++ {
		if r.stageOf(h[j]) == string(model.StageVote) || h[j].Event == "vote" {
			used[j] = true
			end = j
			if h[j].Event == "finished" {
				n++
			}
		}
	}
	st.End = ParseTime(h[end].At)
	st.What = sentence(fmt.Sprintf("%s scored each proposal 1–5, blind to each other", Plural(n, "voter")))
	if a, ok := agentOf(t); ok && a.Stage == string(model.StageVote) {
		st.Tone = "now"
		st.What += " Still voting."
	}
	return st
}

// prospectOrigin is the first steps of a proposal's story, from the prospect
// that found it.
func (r *reader) prospectOrigin(p *ticket.Ticket) []Step {
	var out []Step
	for i, e := range p.History {
		switch {
		case e.Event == "created":
			out = append(out, Step{At: ParseTime(e.At), Who: "Clock", What: "Started a prospecting run (" + string(p.ID) + ")."})
		case e.Event == "start" && r.stageOf(e) == string(model.StageProspect):
			st := Step{At: ParseTime(e.At), Who: "Prospector", What: stageWhat(p, string(model.StageProspect))}
			_, name, _, _ := r.runOf(e)
			if j := r.finishedAt(p.History, i, string(model.StageProspect), name); j >= 0 {
				st.End = ParseTime(p.History[j].At)
			}
			out = append(out, st)
		}
	}
	return out
}

func (r *reader) panelSteps(t *ticket.Ticket, all map[ticket.ID]*ticket.Ticket, e ticket.Entry) []Step {
	votes := panelVotes(t, all)
	at := ParseTime(e.At)
	panel := Step{At: at, Who: "Panel"}
	if p := all[parentOf(t)]; p != nil {
		// times come from the prospect: a proposal filed later (a migrated board) keeps the panel's own
		for _, x := range p.History {
			switch {
			case x.Event == "start" && r.stageOf(x) == string(model.StageVote) && panel.End.IsZero():
				panel.At, panel.End = ParseTime(x.At), at
			case x.Event == "finished" && r.stageOf(x) == string(model.StageVote):
				panel.End = ParseTime(x.At)
			case r.panelDecided(x):
				at = ParseTime(x.At)
			}
		}
	}
	var scores []string
	for _, v := range votes {
		scores = append(scores, strconv.Itoa(v.Score))
		panel.Quotes = append(panel.Quotes, Quote{Score: v.Score, Text: v.Reason, Who: v.Voter})
	}
	p, hasPanel := t.Panel.Get()
	if len(scores) == 0 && hasPanel {
		for _, s := range p.Scores {
			scores = append(scores, strconv.Itoa(s))
		}
	}
	panel.What = "Scored " + strings.Join(scores, " · ") + "."
	rule := e.Rule
	if rule == "" && hasPanel {
		rule = PanelRule(p.Scores)
	}
	verdict := Step{At: at, Who: "Dispatcher", What: sentence(rule)}
	if t.Column == string(model.ColRejected) && strings.HasPrefix(rule, "dropped") {
		verdict.Tone = "flag"
		verdict.What += " It never reached your desk."
	}
	return []Step{panel, verdict}
}

// panelTime is when the prospect's panel decided, or zero.
func (r *reader) panelTime(p *ticket.Ticket) time.Time {
	for _, x := range p.History {
		if r.panelDecided(x) {
			return ParseTime(x.At)
		}
	}
	return time.Time{}
}

// PanelRule is the dispatcher's keep rule in words: model.PanelKeenNeeded
// keen voters (model.PanelKeenScore or more), or a mean of
// model.PanelMeanNeeded.
func PanelRule(scores []int) string {
	if len(scores) == 0 {
		return "dropped: no scores"
	}
	keen, sum := 0, 0
	for _, s := range scores {
		sum += s
		if s >= model.PanelKeenScore {
			keen++
		}
	}
	mean := float64(sum) / float64(len(scores))
	tail := fmt.Sprintf("mean %.1f (%.1f needed)", mean, model.PanelMeanNeeded)
	if keen >= model.PanelKeenNeeded || mean >= model.PanelMeanNeeded {
		return fmt.Sprintf("kept: %d of %d voters scored %d or more (%d needed); %s", keen, len(scores), model.PanelKeenScore, model.PanelKeenNeeded, tail)
	}
	if keen == 0 {
		return fmt.Sprintf("dropped: no voter scored %d or more; %s", model.PanelKeenScore, tail)
	}
	return fmt.Sprintf("dropped: %d of %d voters scored %d or more (%d needed); %s", keen, len(scores), model.PanelKeenScore, model.PanelKeenNeeded, tail)
}

// panelVotes are the votes filed on a proposal, or, on older boards, read
// back from its prospect.
func panelVotes(t *ticket.Ticket, all map[ticket.ID]*ticket.Ticket) []ticket.PanelVote {
	if t.KindName() != string(model.KindProposal) {
		return nil
	}
	if p, ok := t.Panel.Get(); ok && len(p.Votes) > 0 {
		return p.Votes
	}
	p := all[parentOf(t)]
	prop, ok := t.Proposal.Get()
	if p == nil || !ok {
		return nil
	}
	return votesFor(p, proposalIndex(p, prop.Title))
}

func proposalIndex(p *ticket.Ticket, title string) int {
	for i, x := range p.Proposals {
		if x.Title == title {
			return i + 1
		}
	}
	return 0
}

// votesFor is each voter's score for proposal idx (1-based) of prospect p.
func votesFor(p *ticket.Ticket, idx int) []ticket.PanelVote {
	if idx == 0 {
		return nil
	}
	var out []ticket.PanelVote
	for _, v := range p.Votes {
		s, ok := v.Scores.Lookup(strconv.Itoa(idx))
		if !ok {
			continue
		}
		score, _ := s.Get("score").Int64()
		reason, _ := s.Get("reason").Str()
		out = append(out, ticket.PanelVote{Voter: v.Voter, Host: v.Host, Score: int(score), Reason: reason})
	}
	return out
}

// DroppedProposals are the proposals a panel dropped that have no ticket of
// their own (boards from before the dispatcher filed them), as closed
// proposal tickets without an id.
func DroppedProposals(ts []*ticket.Ticket) []*ticket.Ticket { return new(reader).droppedProposals(ts) }

func (r *reader) droppedProposals(ts []*ticket.Ticket) []*ticket.Ticket {
	filed := map[string]bool{}
	for _, t := range ts {
		if p, ok := t.Proposal.Get(); ok && t.KindName() == string(model.KindProposal) {
			filed[string(parentOf(t))+"\x00"+p.Title] = true
		}
	}
	var out []*ticket.Ticket
	for _, p := range ts {
		if p.KindName() != string(model.KindProspect) || !p.PanelResult.Present() || len(p.Votes) == 0 {
			continue
		}
		at := ""
		for _, h := range p.History {
			if r.panelDecided(h) {
				at = h.At
			}
		}
		for i, prop := range p.Proposals {
			if filed[string(p.ID)+"\x00"+prop.Title] {
				continue
			}
			votes := votesFor(p, i+1)
			var scores []int
			for _, v := range votes {
				scores = append(scores, v.Score)
			}
			rule := PanelRule(scores)
			if !strings.HasPrefix(rule, "dropped") {
				continue
			}
			out = append(out, &ticket.Ticket{Project: p.Project, Title: prop.Title, Door: string(model.DoorClock),
				Column: string(model.ColRejected), Kind: ticket.Some(string(model.KindProposal)), Parent: ticket.Some(string(p.ID)),
				Proposal: ticket.Some(prop), Panel: ticket.Some(ticket.Panel{Scores: scores, Votes: votes}),
				Created: at, Updated: at, History: []ticket.Entry{{At: at, Event: "panel", Rule: rule}}})
		}
	}
	return out
}

// askSteps are the question, then its answer (or where the answer is) from
// the task it filed.
func (r *reader) askSteps(e ticket.Entry, all map[ticket.ID]*ticket.Ticket) []Step {
	id, q := r.askOf(e)
	steps := []Step{{At: ParseTime(e.At), Who: "You", What: "Asked: " + Clip(q, 400)}}
	c := all[ticket.ID(id)]
	switch {
	case c == nil:
	case c.Result.Or("") != "":
		steps = append(steps, Step{At: ParseTime(c.Updated), Who: "Answer", What: Clip(c.Result.Or(""), 900)})
	case c.Agent.Present():
		steps = append(steps, Step{Who: "Answer", What: "An agent is working on it (" + id + ").", Tone: "now", When: "now"})
	case closed(c):
		steps = append(steps, Step{At: ParseTime(c.Updated), Who: "Answer", What: "None came back (" + id + " closed).", Tone: "flag"})
	default:
		steps = append(steps, Step{Who: "Answer", What: "Queued (" + id + "); an agent picks it up on the next tick.", Tone: "now", When: "soon"})
	}
	return steps
}

// commentText is the comment a "comment" event is about, or the event's own
// clipped line for older files.
func commentText(t *ticket.Ticket, e ticket.Entry) string {
	n := commentN(e)
	for _, c := range t.Comments {
		if n > 0 && c.N == n {
			return c.Text
		}
	}
	return e.Detail
}

// boardQuestionOf is the board question a "question" or "answer" event is
// about, or nil (older files name it only in the event's words).
func boardQuestionOf(t *ticket.Ticket, e ticket.Entry) *ticket.BoardQuestion {
	n := 0
	switch d := e.Data.(type) {
	case ticket.QuestionAsked:
		n = d.N
	case ticket.QuestionAnswered:
		n = d.N
	}
	for i := range t.Questions {
		if n > 0 && t.Questions[i].N == n {
			return &t.Questions[i]
		}
	}
	return nil
}

// WaitingWhat is what the desk waits for on an ask, in a sentence.
func WaitingWhat(t *ticket.Ticket, a *ticket.Ask) string {
	switch model.AskKind(a.Kind) {
	case model.AskReview:
		if Mergeable(t) {
			return "It passed every check for automatic approval. Merge it, or read it on GitHub first."
		}
		if WaitsForApproval(t) {
			return "GitHub merges it only once you approve it there; the factory merges it by itself on the next tick after your approval (or request changes there, or send it back with a note here)."
		}
		return "Approve it on GitHub and the factory merges it, or request changes there (or send it back with a note here)."
	case model.AskOpen:
		return "Open the PR on GitHub, or send it back with a note."
	case model.AskStart:
		return "Start it, or not now."
	case model.AskDecide:
		return "Start it anyway, drop it, or ask triage again with a note."
	case model.AskDesign:
		return "Approve some or all of the tickets, or send it back with a note."
	case model.AskFindings:
		return "Tick the findings to file, choose change or intent for each, and file them; or file none."
	case model.AskProposal:
		return "File it as a change, or drop it."
	case model.AskNotice:
		return "Close it when you have read it."
	case model.AskQuestion:
		if q := Asked(t, a); q != nil {
			return q.By + " asked " + q.Recipient + " and waits for the answer."
		}
		return "The agent waits for your answer."
	case model.AskDraft:
		c := ticket.Items(t)
		switch {
		case c.New+c.Requested > 0 && c.ByHand > 0:
			return "The factory applies the changes on the next tick; then apply the by-hand diff in the config repo on your machine."
		case c.New+c.Requested > 0:
			return "The factory applies the changes on the next tick; nothing is needed from you yet."
		case c.ByHand > 0 || len(t.Drafts)+len(t.Actions) == 0:
			return "Apply the diff in the config repo on your machine and deploy, then say so here; or send it back, or drop it."
		case c.Waiting+c.Failed > 0:
			return "Apply it: the factory lands each waiting change on config main and runs each action, checking every one again first; or send it back, or drop it."
		}
		return "Close it when you have read it."
	case model.AskDeploy:
		return "Deploy it now, or not now: the next merge asks again."
	}
	return "Retry it, retry with a note, or drop it."
}

// formatWhen writes each step's time: the date only when it changes.
func formatWhen(steps []Step) {
	last := ""
	for i := range steps {
		s := &steps[i]
		if s.When != "" || s.At.IsZero() {
			continue
		}
		at := s.At.Local()
		day := at.Format("2 Jan")
		s.When = at.Format("15:04")
		if day != last {
			s.When = day + " " + s.When
			last = day
		}
		if !s.End.IsZero() && s.End.Sub(s.At) >= time.Minute {
			s.When += "–" + s.End.Local().Format("15:04")
		}
	}
}
