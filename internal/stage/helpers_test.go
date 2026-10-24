package stage

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

var now = time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)

// decode is a ticket from a JSON literal.
func decode(t testing.TB, js string) *ticket.Ticket {
	t.Helper()
	tk, problems, err := ticket.Decode([]byte(js))
	if err != nil {
		t.Fatalf("ticket %s: %v", js, err)
	}
	if len(problems) > 0 {
		t.Fatalf("ticket problems: %v", problems)
	}
	return tk
}

// report parses a report as the pipeline reads report.json.
func report(js string) jsonx.Value { return ParseReport([]byte(js)) }

// cfg parses a config.toml.
func cfg(t testing.TB, toml string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(toml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

// tomlStr is a TOML basic string.
func tomlStr(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// projectsTOML is [projects.<key>] tables for string-valued projects.
func projectsTOML(projects map[string]map[string]string) string {
	keys := make([]string, 0, len(projects))
	for k := range projects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "[projects.%s]\n", tomlStr(k))
		fields := make([]string, 0, len(projects[k]))
		for f := range projects[k] {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		for _, f := range fields {
			fmt.Fprintf(&b, "%s = %s\n", f, tomlStr(projects[k][f]))
		}
	}
	return b.String()
}

// finish runs the stage that finished on t with a report and facts.
func finish(t testing.TB, in FinishIn) Outcome {
	t.Helper()
	if in.Now.IsZero() {
		in.Now = now
	}
	if in.Config == nil {
		in.Config = cfg(t, "")
	}
	need := Needs(in)
	if len(in.IDs) == 0 {
		for i := range need {
			in.IDs = append(in.IDs, ticket.FormatID(100+i))
		}
	}
	out, err := Finish(in)
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(out.File) != need {
		t.Fatalf("filed %d tickets, Needs said %d", len(out.File), need)
	}
	return out
}

// last is the ticket's last history entry.
func last(t *ticket.Ticket) ticket.Entry { return t.History[len(t.History)-1] }

// reason is the typed reason of the ticket's last move.
func reason(t *ticket.Ticket) model.Reason {
	for i := len(t.History) - 1; i >= 0; i-- {
		if m, ok := t.History[i].Data.(ticket.Moved); ok {
			return m.Reason
		}
	}
	return ""
}

// running is a ticket with an agent of stage running on it.
func running(t testing.TB, id, stage, column, extra string) *ticket.Ticket {
	t.Helper()
	if extra != "" {
		extra = ", " + extra
	}
	js := fmt.Sprintf(`{"id": %q, "project": "gem", "title": "t", "body": "", "door": "human", "by": "factory", "column": %q,
		"branch": "b", "agent": {"name": %q, "stage": %q, "started": "2026-10-17T09:00:00+00:00", "host": "local"},
		"reports": {}, "created": "2026-10-17T08:00:00+00:00",
		"history": [{"at": "x", "event": "start", "detail": "work t0001-w1 on b"}], "kind": null, "ask": null%s}`,
		id, column, strings.ToLower(id)+"-"+string(stageSuffix(stage))+"1", stage, extra)
	return decode(t, js)
}

func stageSuffix(s string) byte {
	info, _ := model.StageInfoOf(model.Stage(s))
	return info.Suffix
}

type configProject = config.Project
