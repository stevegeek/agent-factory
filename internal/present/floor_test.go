package present

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFloorGroupsByWeekAndDay(t *testing.T) {
	ts := fixtures(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := BuildFloor(ts, time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC), now, you)
	if f.Title != "Week of 5 October" || f.Prev != "2026-09-28" || f.Next != "2026-10-12" {
		t.Errorf("week %q prev %q next %q", f.Title, f.Prev, f.Next)
	}
	var days []string
	for _, d := range f.Days {
		days = append(days, d.Name)
	}
	want := []string{"Friday 9 October", "Thursday 8 October", "Wednesday 7 October", "Tuesday 6 October", "Monday 5 October"}
	if !reflect.DeepEqual(days, want) {
		t.Fatalf("days\n got %v\nwant %v", days, want)
	}
	lines := map[string]FloorEvent{}
	for _, d := range f.Days {
		for i, e := range d.Events {
			lines[d.Name+" "+e.Line] = e
			if i > 0 && e.At.After(d.Events[i-1].At) {
				t.Errorf("%s: events should run newest first", d.Name)
			}
		}
	}
	if e := lines["Wednesday 7 October Proposal 2"]; e.Class != "dropped" || len(e.Story.Steps) == 0 {
		t.Errorf("dropped proposal line %+v", e)
	}
	if e := lines["Wednesday 7 October T0005 Look for improvements"]; e.Label != "1 kept" {
		t.Errorf("panel line %+v", e)
	}
	if e := lines["Tuesday 6 October T0001 Fix the widget finder"]; e.Label != "PR opened" || !reflect.DeepEqual(e.Earlier, []string{"started", "gate passed"}) {
		t.Errorf("one line per ticket per day, latest first: %+v", e)
	}
	if e := lines["Monday 5 October T0010 Ignore generated files"]; e.Class != "merged" {
		t.Errorf("merge line %+v", e)
	}
	if !strings.Contains(f.Summary, "1 PR opened, 1 merged") || !strings.Contains(f.Summary, "2 proposals, 1 dropped by the panel") {
		t.Errorf("summary %q", f.Summary)
	}
	if len(f.Running) != 1 || f.Running[0].T.ID != "T0011" || f.Running[0].Role != "Worker" {
		t.Errorf("running %+v", f.Running)
	}
	if f.Asks != Waiting(Desk(ts, you)) {
		t.Errorf("asks %d", f.Asks)
	}
	prev := BuildFloor(ts, time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), now, you)
	if len(prev.Days) != 1 || prev.Days[0].Events[0].Label != "PR opened" {
		t.Errorf("week before: %+v", prev.Days)
	}
	cur := BuildFloor(ts, now, now, you)
	if cur.Title != "This week" || cur.Next != "" || len(cur.Days) != 0 {
		t.Errorf("this week %+v", cur)
	}
}

func TestFloorFutureAndPastTense(t *testing.T) {
	ts := fixtures(t)
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	f := BuildFloor(ts, time.Date(2030, 1, 7, 0, 0, 0, 0, time.UTC), now, you)
	if f.Title != "Week of 7 January" || f.Next != "" {
		t.Errorf("a future week: %q next %q", f.Title, f.Next)
	}
	f = BuildFloor(ts, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), now, you)
	waiting := 0
	for _, d := range f.Days {
		for _, e := range d.Events {
			for _, x := range append(append([]string{}, e.Earlier...), e.Detail) {
				if e.T.ID == "T0001" && strings.Contains(x, "waits for you") {
					t.Errorf("T0001 is past its start ask: %q", x)
				}
			}
			if e.T.ID == "T0002" && e.Label == "Accepted" {
				waiting++
				if !strings.Contains(e.Detail, "waits for you") {
					t.Errorf("T0002 still waits: %q", e.Detail)
				}
			}
		}
	}
	if waiting != 1 {
		t.Errorf("T0002's accepted line: %d", waiting)
	}
	t1 := find(ts, "T0001")
	for i, h := range t1.History {
		if strings.Contains(h.Detail, "triage accepted") {
			if _, _, d := FloorOutcome(t1, i); d != "triage accepted it · you started it" {
				t.Errorf("past start ask: %q", d)
			}
		}
	}
}

func TestAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Second: "just now", 5 * time.Minute: "5 min", 3 * time.Hour: "3 h",
		47 * time.Hour: "47 h", 49 * time.Hour: "2 days", 10 * 24 * time.Hour: "10 days",
	} {
		if got := Age(d); got != want {
			t.Errorf("%v: %q", d, got)
		}
	}
	if AgeSince("garbage", time.Now()) != "" {
		t.Errorf("a bad stamp has no age")
	}
}
