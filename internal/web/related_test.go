package web

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

// relatedHarness files T0003 from T0002's second finding, T0007 and T0008
// as follow-ups to T0003.
func relatedHarness(t *testing.T) *harness {
	h := newHarness(t, false)
	h.change("T0003", func(x *ticket.Ticket) {
		x.Parent = ticket.Some("T0002")
		x.FoundIn = ticket.Some(ticket.FindingRef{Ticket: "T0002", N: 2})
	})
	h.change("T0007", func(x *ticket.Ticket) { x.Parent = ticket.Some("T0003") })
	h.change("T0008", func(x *ticket.Ticket) { x.Parent = ticket.Some("T0003") })
	return h
}

func TestTheTicketPageShowsItsThreadAndDescendants(t *testing.T) {
	h := relatedHarness(t)
	c := h.login()
	body := h.do("GET", "/t/T0003", nil, c).Body.String()
	if !strings.Contains(body, `<span>Finding 2 of <a class="mono" href="/t/T0002">T0002</a>: Cache the widget coder</span>`) {
		t.Errorf("the header does not name the finding")
	}
	rel := body[strings.Index(body, `<section class="related"`):]
	rel = rel[:strings.Index(rel, "</section>")]
	root, self, kid := strings.Index(rel, `href="/t/T0002"`), strings.Index(rel, `<b class="mono">T0003</b>`), strings.Index(rel, `href="/t/T0007"`)
	if root < 0 || self < root || kid < self || !strings.Contains(rel, `<span class="how">Finding 2</span>`) {
		t.Errorf("the related section is out of order:\n%s", rel)
	}
	if !strings.Contains(rel, "<h4>Follow-ups <span class=\"rel-meta\">2 open · 0 done</span></h4>") || !strings.Contains(rel, "waits for you") {
		t.Errorf("the follow-ups lack their count or state:\n%s", rel)
	}
	if strings.Index(body, `class="related"`) > strings.Index(body, `class="story"`) {
		t.Errorf("the related section is below the story")
	}
	if got := h.do("GET", "/t/T0007", nil, c).Body.String(); !strings.Contains(got, `<span>Filed from <a class="mono" href="/t/T0003">T0003</a>`) {
		t.Errorf("a follow-up does not name its parent")
	}
	for _, path := range []string{"/", "/b/widgets"} {
		if got := h.do("GET", path, nil, c).Body.String(); !strings.Contains(got, `<span class="parent-tag mono">↳ T0002</span>`) {
			t.Errorf("%s has no parent tag", path)
		}
	}
}

func TestATicketWithoutRelationsHasNoRelatedSection(t *testing.T) {
	h := relatedHarness(t)
	body := h.do("GET", "/t/T0010", nil, h.login()).Body.String()
	if strings.Contains(body, `class="related"`) || strings.Contains(body, "parent-tag") {
		t.Errorf("an unrelated ticket shows relations")
	}
}
