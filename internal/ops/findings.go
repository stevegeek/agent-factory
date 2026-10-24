package ops

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The findings flow's verbs: a finding task's findings, checked by a
// verifier, wait on the desk (ask kind findings); the maintainer files the
// ones he picks, each as a change (into triage) or an intent (to its
// designer), and the hunt closes. A task whose findings are only prose
// (from before structured findings) is sent to the verifier on request.

// Findings is what `factory findings T` prints: the hunt's findings with
// their verdicts and how each would be filed, and the commands that file
// them.
func (s *Service) Findings(ctx context.Context, id string) (string, error) {
	t, err := s.load(id)
	if err != nil {
		return "", err
	}
	if len(t.Findings) == 0 {
		if t.Column == string(model.ColVerifying) {
			return string(t.ID) + "'s findings are with the verifier", nil
		}
		return "", refuse(Conflict, "%s has no verified findings; `factory findings %s --verify` sends a task's result to the verifier", t.ID, t.ID)
	}
	confirmed := 0
	for _, f := range t.Findings {
		if f.Confirmed() {
			confirmed++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %d findings, %d confirmed\n\n", t.ID, t.Title, len(t.Findings), confirmed)
	var open []string
	for i, f := range t.Findings {
		facts := []string{ticket.VerdictLabel(f)}
		if f.Confirmed() {
			facts = append(facts, strings.TrimSpace(f.Severity+" "+f.Kind))
			if f.BehaviourChange {
				facts = append(facts, "changes behaviour")
			}
			switch {
			case f.Filed != "":
				facts = append(facts, "filed as "+f.Filed+" ("+f.FiledAs+")")
			case f.DefaultKind() == string(model.KindIntent):
				facts = append(facts, "files as an intent")
			default:
				facts = append(facts, "files as a change")
			}
		}
		if f.Open() {
			open = append(open, strconv.Itoa(i+1))
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, f.Title, strings.Join(facts, " · "))
		if e := textx.Clip(f.Evidence, 200); e != "" {
			fmt.Fprintf(&b, "   %s\n", e)
		}
		b.WriteString("\n")
	}
	if len(open) > 0 && t.Column == string(model.ColNeedsHuman) {
		fmt.Fprintf(&b, "factory findings %s --file %s [--as N=intent|change,...]  |  factory findings %s --none",
			t.ID, strings.Join(open, ","), t.ID)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// FileFindings files the findings picked (1-based) of a hunt waiting on
// the desk, each as as says (change or intent) or else by its default, and
// closes the hunt: `factory findings T --file 1,3 [--as 3=intent]`. It
// returns the new tickets' ids.
func (s *Service) FileFindings(ctx context.Context, id string, picks []int, as map[int]string) ([]ticket.ID, error) {
	var ids []ticket.ID
	err := s.change(ctx, id, func(c *call, t *ticket.Ticket) error {
		if err := factoryOnly(t); err != nil {
			return err
		}
		var err error
		ids, err = c.fileFindings(ctx, t, picks, as)
		return err
	})
	return ids, err
}

// DismissFindings closes a hunt waiting on the desk with none of its open
// findings filed: `factory findings T --none`.
func (s *Service) DismissFindings(ctx context.Context, id string) error {
	return s.change(ctx, id, func(c *call, t *ticket.Ticket) error {
		if err := factoryOnly(t); err != nil {
			return err
		}
		if err := waitingFindings(t); err != nil {
			return err
		}
		ticket.CloseFindings(t, c.by(), "", c.now)
		return nil
	})
}

// VerifyFindings sends a finished task's result to the verifier:
// `factory findings T --verify`, the desk's "Verify these findings".
func (s *Service) VerifyFindings(ctx context.Context, id string) error {
	return s.change(ctx, id, func(c *call, t *ticket.Ticket) error {
		if err := factoryOnly(t); err != nil {
			return err
		}
		return c.verify(t)
	})
}

// waitingFindings refuses a ticket that is not a hunt whose findings wait
// on the desk.
func waitingFindings(t *ticket.Ticket) error {
	if len(t.Findings) == 0 || t.Column != string(model.ColNeedsHuman) || t.Agent.Present() {
		return refuse(Conflict, "%s has no findings waiting for you", t.ID)
	}
	return nil
}

// verifiable says whether a task's result can go to the verifier: a task
// on one of the factory's projects (the verifier re-runs each finding on
// its checkout), back from its agent.
func verifiable(t *ticket.Ticket) bool {
	return t.KindName() == string(model.KindTask) && t.Project != model.NoticeProject && t.Column == string(model.ColNeedsHuman) &&
		!t.Agent.Present() && len(t.Findings) == 0
}

func (c *call) verify(t *ticket.Ticket) error {
	if len(t.Findings) > 0 {
		return refuse(Conflict, "%s's findings are verified already: file them, or none (`factory findings %s`)", t.ID, t.ID)
	}
	if !verifiable(t) {
		return refuse(Conflict, "%s is not a finished task on a project; only those are verified", t.ID)
	}
	if !isFactoryProject(c.cfg, t.Project) {
		return refuse(Conflict, "%s is not a factory project: the verifier needs its repository", t.Project)
	}
	if _, ran := t.Reports.Lookup(string(model.StageTask)); t.Result.Or("") == "" && !ran {
		return refuse(Conflict, "%s has no result to verify", t.ID)
	}
	ticket.Move(t, string(model.ColVerifying), model.ReasonVerifyRequested,
		"verify the findings in its result; a verifier checks each one", "", c.by(), c.now)
	c.wake = true
	return nil
}

// fileFindings files the picked findings and closes the hunt. A finding a
// cut-short earlier answer already filed (its ticket names this hunt and
// finding in found_in) is recorded, never filed again.
func (c *call) fileFindings(ctx context.Context, t *ticket.Ticket, picks []int, as map[int]string) ([]ticket.ID, error) {
	if err := waitingFindings(t); err != nil {
		return nil, err
	}
	picks = dedupe(picks)
	if len(picks) == 0 {
		return nil, refuse(BadRequest, "tick at least one finding to file, or file none")
	}
	for _, n := range picks {
		if n < 1 || n > len(t.Findings) {
			return nil, refuse(BadRequest, "pick numbers 1 to %d", len(t.Findings))
		}
		f := t.Findings[n-1]
		if !f.Confirmed() {
			return nil, refuse(BadRequest, "finding %d was not confirmed (%s); it is not filed", n, ticket.VerdictLabel(f))
		}
		if k, ok := as[n]; ok && k != string(model.KindChange) && k != string(model.KindIntent) {
			return nil, refuse(BadRequest, "finding %d: file it as a change or an intent, not %q", n, k)
		}
	}
	c.recoverFiled(t)
	var ids []ticket.ID
	for _, n := range picks {
		if f := t.Findings[n-1]; f.Filed != "" {
			ids = append(ids, ticket.ID(f.Filed))
			continue
		}
		kind, ok := as[n]
		if !ok {
			kind = t.Findings[n-1].DefaultKind()
		}
		id, err := c.s.Board.NextID(ctx)
		if err != nil {
			return nil, err
		}
		child, err := ticket.FileFinding(id, t, n, kind, c.by(), c.now)
		if err != nil {
			return nil, refuse(Conflict, "%s", err.Error())
		}
		if err := c.s.Board.Save(child); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	ticket.CloseFindings(t, c.by(), "", c.now)
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = string(id)
	}
	c.print(strings.Join(names, " "))
	c.wake = true
	return ids, nil
}

// recoverFiled records on the hunt the tickets an earlier, interrupted
// filing saved before the hunt itself was saved.
func (c *call) recoverFiled(t *ticket.Ticket) {
	ts, _ := c.s.Board.Reader.List()
	for _, x := range ts {
		r, ok := x.FoundIn.Get()
		if !ok || r.Ticket != string(t.ID) || r.N < 1 || r.N > len(t.Findings) || t.Findings[r.N-1].Filed != "" {
			continue
		}
		kind := x.KindName()
		if kind == "" {
			kind = string(model.KindChange)
		}
		t.Findings[r.N-1].Filed, t.Findings[r.N-1].FiledAs = string(x.ID), kind
		if !contains(t.Children, string(x.ID)) {
			t.Children = append(t.Children, string(x.ID))
		}
	}
}
