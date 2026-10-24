package web

import (
	"strings"
	"testing"
)

// The floor's metrics: five weeks in a table per project and in total, the
// attention row only the viewer's own answers unless they are the
// maintainer.
func TestFloorMetricsRender(t *testing.T) {
	h := newHarness(t, false)
	page := h.do("GET", "/floor/metrics", nil, h.login()).Body.String()
	for _, want := range []string{
		`aria-label="Floor metrics"`,
		`<details class="mtable" data-key="metrics-All projects" open>`,
		`<details class="mtable" data-key="metrics-widgets">`,
		`<th scope="col">This week</th><th scope="col">28 Sep</th><th scope="col">21 Sep</th><th scope="col">14 Sep</th><th scope="col">7 Sep</th>`,
		`asks answered on the desk, by anyone`,
		`<th scope="row">Asks answered</th><td>2</td><td>0</td>`,
		`<tr class="msub"><th scope="row">Review</th><td>1</td>`,
		`<th scope="row">PRs merged</th><td>1</td>`,
		`<th scope="row">Reverts</th><td class="mnot" colspan="5">not tracked`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the floor metrics lack %q", want)
		}
	}
	if earlier := h.do("GET", "/floor/metrics?week=2026-09-14", nil, h.login()).Body.String(); !strings.Contains(earlier, "Nothing to count in these five weeks.") {
		t.Errorf("a week with nothing before it is quiet")
	}
	secret, _ := h.users.Add("robin", []string{"widgets"})
	robin := h.do("GET", "/floor/metrics", nil, h.loginWith(secret, "/")).Body.String()
	if !strings.Contains(robin, "asks you answered") || !strings.Contains(robin, `<th scope="row">Asks answered</th><td>0</td>`) ||
		!strings.Contains(robin, `<th scope="row">PRs merged</th><td>1</td>`) {
		t.Errorf("robin's floor counts only robin's answers, and everyone's outcomes")
	}
	floor := h.do("GET", "/floor?week=2026-09-28", nil, h.login()).Body.String()
	if strings.Contains(floor, `class="metrics"`) || !strings.Contains(floor, `href="/floor/metrics?week=2026-09-28">Floor metrics</a>`) {
		t.Errorf("the floor still holds the metrics, or lacks a link to them for its week")
	}
	if w := h.do("GET", "/floor/metrics?week=bad", nil, h.login()); w.Code != 400 {
		t.Errorf("a bad week: %d", w.Code)
	}
}
