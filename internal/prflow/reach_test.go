package prflow

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

type note struct {
	OK      bool
	LastErr string
}

// fakeReach says GitHub is down on the second failed pass in a row and
// back on the first answer after that.
type fakeReach struct {
	notes  []note
	failed int
	down   bool
}

func (r *fakeReach) Note(_ time.Time, ok bool, lastErr string) (string, error) {
	r.notes = append(r.notes, note{ok, lastErr})
	if ok {
		was := r.down
		r.failed, r.down = 0, false
		if was {
			return "github: reachable again", nil
		}
		return "", nil
	}
	r.failed++
	if r.failed == 2 {
		r.down = true
		return "github: unreachable (last error: " + lastErr + ")", nil
	}
	return "", nil
}

func reachHarness(t *testing.T) (*harness, *fakeReach) {
	h := intentHarness(t)
	r := &fakeReach{}
	h.flow.Reach = r
	return h, r
}

func TestUnreachableGitHubIsNoTicketErrorAndOneGlobalLine(t *testing.T) {
	cases := []struct {
		name  string
		setup func(h *harness)
	}{
		{"no answer", func(h *harness) {
			h.routes(at("GET", "/repos/o/gem/pulls/7", reply{NetErr: errNoAnswer}))
		}},
		{"5xx", func(h *harness) {
			h.routes(at("GET", "/repos/o/gem/pulls/7", refuse(502, "Bad Gateway")))
		}},
		{"token mint", func(h *harness) {
			h.token.err = fmt.Errorf("github: installation token: %w",
				&github.NetError{Method: "POST", Path: "/app/installations/42/access_tokens", Err: errNoAnswer})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, r := reachHarness(t)
			a := h.intentTicket(1, "pr_open", "gem", 7, "")
			b := h.intentTicket(2, "pr_open", "gem", 7, "")
			c.setup(h)
			for range 3 {
				h.pass(context.Background(), intentConfig)
			}
			for _, id := range []ticket.ID{a.ID, b.ID} {
				if errs := details(h.board.load(t, id), "error"); len(errs) != 0 {
					t.Errorf("%s: errors %q", id, errs)
				}
			}
			if len(r.notes) != 3 || r.notes[0].OK || r.notes[0].LastErr == "" {
				t.Fatalf("notes = %+v", r.notes)
			}
			var said []string
			for _, l := range h.log.lines {
				if strings.HasPrefix(l, "github: unreachable") {
					said = append(said, l)
				}
			}
			if len(said) != 1 {
				t.Fatalf("log = %q", h.log.lines)
			}
		})
	}
}

func TestTheFirstAnswerClearsUnreachableOnce(t *testing.T) {
	h, r := reachHarness(t)
	h.intentTicket(1, "pr_open", "gem", 7, "")
	h.routes(at("GET", "/repos/o/gem/pulls/7", reply{NetErr: errNoAnswer}))
	h.pass(context.Background(), intentConfig)
	h.pass(context.Background(), intentConfig)
	h.routes(at("GET", "/repos/o/gem/pulls/7", refuse(404, "Not Found")))
	h.pass(context.Background(), intentConfig)
	h.pass(context.Background(), intentConfig)
	if r.down || len(r.notes) != 4 || !r.notes[2].OK || !r.notes[3].OK {
		t.Fatalf("notes = %+v", r.notes)
	}
	n := 0
	for _, l := range h.log.lines {
		if l == "github: reachable again" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("log = %q", h.log.lines)
	}
}

func TestAnAnswerOnAnyTicketMakesThePassGood(t *testing.T) {
	h, r := reachHarness(t)
	h.intentTicket(1, "pr_open", "gem", 7, "")
	h.intentTicket(2, "pr_open", "gem", 8, "")
	h.routes(
		at("GET", "/repos/o/gem/pulls/7", reply{NetErr: errNoAnswer}),
		at("GET", "/repos/o/gem/pulls/8", refuse(404, "Not Found")),
	)
	h.pass(context.Background(), intentConfig)
	if len(r.notes) != 1 || !r.notes[0].OK {
		t.Fatalf("notes = %+v", r.notes)
	}
}

func TestA4xxStillErrorsTheTicket(t *testing.T) {
	for _, code := range []int{403, 404, 422} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			h, r := reachHarness(t)
			tk := h.intentTicket(1, "pr_open", "gem", 7, "")
			h.routes(at("GET", "/repos/o/gem/pulls/7", refuse(code, "No")))
			h.pass(context.Background(), intentConfig)
			got := h.board.load(t, tk.ID)
			if e := lastEntry(got); e.Event != "error" || !strings.HasPrefix(e.Detail, fmt.Sprintf("github: HTTP Error %d: ", code)) {
				t.Errorf("last = %+v", e)
			}
			if len(r.notes) != 1 || !r.notes[0].OK {
				t.Errorf("notes = %+v", r.notes)
			}
		})
	}
}

func TestAPassWithNothingToFollowNotesNothing(t *testing.T) {
	h, r := reachHarness(t)
	h.intentTicket(1, "done", "gem", 7, "")
	h.pass(context.Background(), intentConfig)
	if len(r.notes) != 0 {
		t.Fatalf("notes = %+v", r.notes)
	}
}
