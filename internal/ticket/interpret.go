package ticket

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// startDetail is a legacy start's detail.
//
// It is Foreman's startDetail, with the branch captured.
//
// re: go
var startDetail = regexp.MustCompile(`^(\w+) (\S+)(?: on (\S+))?(?: @(\S+))?(?: \(([^)]*)\))?$`)

// moveDetail is MOVE_DETAIL (re.S, fullmatch).
//
// py: (\w+) -> (\w+)(?:: (.*))?
var moveDetail = regexp.MustCompile(`\A(?s:(` + textx.PyWord + `+) -> (` + textx.PyWord + `+)(?:: (.*))?)\z`)

// gateDetail is a legacy gate event's detail, "<base> moved; the checks
// pass again with it merged in (<commit>)".
//
// re: go
var gateDetail = regexp.MustCompile(`^(\S+) moved; .* \((\w+)\)$`)

// Interpret is the one reader of history entries for code that needs their
// facts (present, ops, migrate): the typed payload when the entry has one,
// else the facts read from the detail of a legacy entry, with legacy true.
// It holds the only detail parsers: Python's starts(), said(), asked_on and
// MOVE_DETAIL and Foreman's events.go, in one place. A legacy entry it cannot
// read gives (nil, true).
//
// A Moved payload always carries Text, the words after the columns, and a
// Filed payload the words after the id.
func Interpret(e Entry) (Payload, bool) {
	if e.Data != nil {
		switch d := e.Data.(type) {
		case Moved:
			_, d.Text, _ = strings.Cut(e.Detail, ": ")
			return d, false
		case Filed:
			_, d.Text, _ = strings.Cut(e.Detail, ": ")
			return d, false
		}
		return e.Data, false
	}
	switch e.Event {
	case "start":
		return legacyStart(e.Detail), true
	case "finished":
		stage, agent, _ := strings.Cut(e.Detail, " ")
		if stage == "" || agent == "" {
			return nil, true
		}
		return Finished{Stage: stage, Agent: agent}, true
	case "move":
		return legacyMove(e.Detail), true
	case "asked", "answered":
		id, text, ok := strings.Cut(e.Detail, ": ")
		if !ok {
			return nil, true
		}
		if e.Event == "asked" {
			return Asked{Ticket: id, Text: text}, true
		}
		return Answered{Ticket: id, Text: text}, true
	case "memory":
		return legacyMemory(e.Detail), true
	case "created":
		var c Created
		for _, f := range strings.Fields(e.Detail) {
			k, v, _ := strings.Cut(f, "=")
			switch k {
			case "door":
				c.Door = v
			case "column":
				c.Column = v
			}
		}
		if c.Door == "" && c.Column == "" {
			return nil, true
		}
		return c, true
	case "error":
		return Errored{Text: e.Detail}, true
	case "pr":
		return PRNote{Why: e.Detail}, true
	case "gate":
		m := gateDetail.FindStringSubmatch(e.Detail)
		if m == nil {
			return nil, true
		}
		return Gated{Base: m[1], Commit: m[2]}, true
	case "filed":
		id, text, _ := strings.Cut(e.Detail, ": ")
		return Filed{Ticket: id, Text: text}, true
	case "vote":
		k, n, ok := strings.Cut(strings.TrimSuffix(e.Detail, " in"), "/")
		ki, err1 := strconv.Atoi(k)
		ni, err2 := strconv.Atoi(n)
		if !ok || err1 != nil || err2 != nil {
			return nil, true
		}
		return Voted{K: ki, N: ni}, true
	case "panel":
		return PanelScored{Kept: strings.HasPrefix(e.Rule, "kept")}, true
	case "host":
		return Pinned{Host: e.Detail}, true
	case "review requested":
		return ReviewRequested{Reviewers: strings.Split(e.Detail, ", ")}, true
	}
	return nil, true
}

// legacyStart reads "<stage> <agent>[ on <branch>][ @<host>][ (<model>)]".
func legacyStart(detail string) Payload {
	m := startDetail.FindStringSubmatch(detail)
	if m == nil {
		return nil
	}
	host := m[4]
	if host == "" {
		host = model.LocalHost
	}
	return Started{Stage: m[1], Agent: m[2], Branch: m[3], Host: host, Model: m[5]}
}

// legacyMove reads MOVE_DETAIL, "<old> -> <new>[: detail]", and the reason
// the wording names, where it names one: the prefixes ask_for's said() reads,
// "by human" and "panel kept".
func legacyMove(detail string) Payload {
	m := moveDetail.FindStringSubmatch(detail)
	if m == nil {
		return nil
	}
	mv := Moved{From: m[1], To: m[2], Text: m[3]}
	for _, w := range []struct {
		prefix string
		reason model.Reason
	}{
		{"by human", model.ReasonByHuman}, {"triage accepted", model.ReasonTriageAccepted},
		{"triage unsure", model.ReasonTriageUnsure}, {"designer blocked", model.ReasonDesignerBlocked},
		{"manager drafted", model.ReasonManagerDrafted}, {"manager answered", model.ReasonManagerAnswered},
		{"task done", model.ReasonTaskDone}, {"task blocked", model.ReasonTaskBlocked},
	} {
		if strings.HasPrefix(mv.Text, w.prefix) {
			mv.Reason = w.reason
		}
	}
	if stage, _, ok := strings.Cut(mv.Text, " asks: "); ok && !strings.Contains(stage, " ") {
		mv.Reason = model.ReasonAgentQuestion
	}
	if strings.Contains(detail, "panel kept") {
		mv.Reason = model.ReasonPanelDone
	}
	return mv
}

// legacyMemory reads the three memory details: "<role> memory <sha>:
// <files>[ …]", "no memory came back from <host>; …" and "<role> memory is
// <n> KB, over its limit: …".
func legacyMemory(detail string) Payload {
	if rest, ok := strings.CutPrefix(detail, "no memory came back from "); ok {
		host, _, _ := strings.Cut(rest, ";")
		return Memory{LostOn: host}
	}
	role, rest, ok := strings.Cut(detail, " memory ")
	if !ok || role == "" || strings.ContainsAny(role, " ") {
		return nil
	}
	if kb, ok := strings.CutPrefix(rest, "is "); ok {
		n, err := strconv.Atoi(strings.SplitN(kb, " ", 2)[0])
		if err != nil {
			return nil
		}
		return Memory{Role: role, OverLimitKB: n}
	}
	sha, files, ok := strings.Cut(rest, ": ")
	if !ok || len(sha) < 7 || len(sha) > 40 || strings.Trim(sha, "0123456789abcdef") != "" {
		return nil
	}
	mem := Memory{Role: role, Commit: sha}
	for _, f := range strings.Split(strings.TrimSuffix(files, " …"), ", ") {
		if f != "" {
			mem.Files = append(mem.Files, f)
		}
	}
	return mem
}
