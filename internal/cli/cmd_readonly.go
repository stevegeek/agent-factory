package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/buildinfo"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/gocache"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/tokens"
)

// The read-only commands: ls, show, result, proposals,
// recipes, model, check and version. Their output is Python's, byte for
// byte (testdata/py holds what Python printed). They read the tickets'
// trees, as Python's dicts, so a value of an unexpected type prints as
// Python printed it.

// kindTag is ls's short name for a ticket's kind.
func kindTag(kind jsonx.Value) string {
	s, _ := kind.Str()
	switch s {
	case "task":
		return "task"
	case "intent":
		return "idea"
	case "prospect":
		return "scan"
	case "proposal":
		return "prop"
	case "item":
		return "item"
	}
	return ""
}

// runLs is cmd_ls: one line per ticket, closed ones only with --all.
func runLs(_ context.Context, e *Env, a Args) error {
	cfg, _ := e.Config()
	tickets, _ := e.Board().Reader.List()
	for _, t := range tickets {
		tr := t.Tree()
		col := tr.Get("column")
		if c, ok := col.Str(); ok && !a.Bool("all") && model.Closed(c) {
			continue
		}
		agent := ""
		if host := tr.Get("host"); jsonx.Truthy(host) {
			agent = "(@" + jsonx.PyStr(host) + ")"
		}
		if p, ok := tr.Get("project").Str(); ok {
			if _, known := cfg.Project(p); known && !cfg.Enabled(p) {
				agent = "(paused)"
			}
		}
		if ag := tr.Get("agent"); jsonx.Truthy(ag) {
			host := jsonx.String("local")
			if o, ok := ag.Object(); ok {
				if h, has := o.Lookup("host"); has {
					host = h
				}
			}
			agent = jsonx.PyStr(ag.Get("name"))
			if h, ok := host.Str(); !ok || h != "local" {
				agent += "@" + jsonx.PyStr(host)
			}
		}
		if m, _ := tr.Get("mode").Str(); m == "board" {
			agent = jsonx.PyStr(pyOr(tr.Get("assignee"), jsonx.String("")))
		}
		fmt.Fprintf(e.Stdout, "%s  %s %s %s %s %s\n", jsonx.PyStr(tr.Get("id")), ljust(jsonx.PyStr(col), 13),
			ljust(jsonx.PyStr(tr.Get("project")), 12), ljust(kindTag(tr.Get("kind")), 4), ljust(agent, 9),
			pySlice(jsonx.PyStr(tr.Get("title")), 60))
	}
	return nil
}

// loadTicket is load_ticket for a command naming a ticket.
func loadTicket(e *Env, name string) (*ticket.Ticket, error) {
	t, err := e.Board().Reader.Load(ticket.ID(name))
	switch {
	case errors.Is(err, boardread.ErrBadID):
		return nil, Refuse("%s is not a ticket id", name)
	case errors.Is(err, fs.ErrNotExist):
		return nil, Refuse("no ticket %s", strings.ToUpper(name))
	case err != nil:
		return nil, err
	}
	return t, nil
}

// runShow is cmd_show: the ticket as json.dumps(t, indent=2).
func runShow(_ context.Context, e *Env, a Args) error {
	t, err := loadTicket(e, a.Pos("ticket"))
	if err != nil {
		return err
	}
	e.Stdout.Write(jsonx.EncodePython(t.Tree(), 2))
	fmt.Fprintln(e.Stdout)
	return nil
}

// runResult is cmd_result: an idea's design and proposed tickets, or a
// task's result.
func runResult(_ context.Context, e *Env, a Args) error {
	t, err := loadTicket(e, a.Pos("ticket"))
	if err != nil {
		return err
	}
	tr := t.Tree()
	id := jsonx.PyStr(tr.Get("id"))
	if k, _ := tr.Get("kind").Str(); k == "intent" {
		design := tr.Get("design")
		if !jsonx.Truthy(design) {
			return Refuse("%s has no design yet", id)
		}
		fmt.Fprintln(e.Stdout, textx.RStrip(jsonx.PyStr(design))+"\n\n## Proposed tickets\n")
		proposed, _ := tr.Get("proposed").Array()
		for i, p := range proposed {
			body := strings.ReplaceAll(jsonx.PyStr(p.Get("body")), "\n", "\n   ")
			fmt.Fprintf(e.Stdout, "%d. %s\n\n   %s\n\n", i+1, jsonx.PyStr(p.Get("title")), body)
		}
		fmt.Fprintf(e.Stdout, "factory approve %s [--only 1,2]  |  factory redirect %s --note \"...\"\n", id, id)
		return nil
	}
	report := pyOr(tr.Get("reports").Get("task"), jsonx.ObjectOf())
	out := pyOr(tr.Get("result"), pyOr(report.Get("summary"), jsonx.String(id+" has no task result")))
	fmt.Fprintln(e.Stdout, jsonx.PyStr(out))
	return nil
}

// runProposals is cmd_proposals: the open proposals, the panel's priority
// first (a stable sort, so ties keep id order).
func runProposals(_ context.Context, e *Env, _ Args) error {
	tickets, _ := e.Board().Reader.List()
	var open []jsonx.Value
	for _, t := range tickets {
		tr := t.Tree()
		k, _ := tr.Get("kind").Str()
		c, _ := tr.Get("column").Str()
		if k == "proposal" && c == "needs_human" {
			open = append(open, tr)
		}
	}
	priority := func(v jsonx.Value) float64 {
		f, _ := v.Get("panel").Get("priority").Float64()
		return f
	}
	sort.SliceStable(open, func(i, j int) bool { return priority(open[i]) > priority(open[j]) })
	for _, tr := range open {
		p := tr.Get("panel")
		fmt.Fprintf(e.Stdout, "%s  priority %s mean %s scores %s  %s\n", jsonx.PyStr(tr.Get("id")),
			ljust(jsonx.PyStr(p.Get("priority")), 4), ljust(jsonx.PyStr(p.Get("mean")), 4), jsonx.PyRepr(p.Get("scores")),
			pySlice(jsonx.PyStr(tr.Get("title")), 60))
	}
	if len(open) == 0 {
		fmt.Fprintln(e.Stdout, "no open proposals")
	}
	return nil
}

// runRecipes is cmd_recipes: the table, or with --json the list Foreman reads.
func runRecipes(_ context.Context, e *Env, a Args) error {
	cfg, _ := e.Config()
	rs := cfg.Recipes()
	if a.Bool("json") {
		list := make([]jsonx.Value, len(rs))
		for i, r := range rs {
			projects := make([]jsonx.Value, len(r.Projects))
			for j, p := range r.Projects {
				projects[j] = jsonx.String(p)
			}
			list[i] = jsonx.ObjectOf(
				jsonx.Member{Key: "name", Value: jsonx.String(r.Name)},
				jsonx.Member{Key: "title", Value: jsonx.String(r.Title)},
				jsonx.Member{Key: "body", Value: jsonx.String(r.Body)},
				jsonx.Member{Key: "kind", Value: jsonx.String(r.Kind)},
				jsonx.Member{Key: "projects", Value: jsonx.ArrayOf(projects...)},
				jsonx.Member{Key: "about", Value: jsonx.String(r.About)},
				jsonx.Member{Key: "scheduled", Value: jsonx.Bool(r.Scheduled)})
		}
		e.Stdout.Write(jsonx.EncodePython(jsonx.ArrayOf(list...), -1))
		fmt.Fprintln(e.Stdout)
		return nil
	}
	for _, r := range rs {
		where := strings.Join(r.Projects, ", ")
		if where == "" {
			where = "any project"
		}
		line := fmt.Sprintf("%s %s %s %s", ljust(r.Name, 24), ljust(r.Kind, 8), ljust(where, 30), r.Title)
		if r.Scheduled {
			line += " (also scheduled)"
		}
		fmt.Fprintln(e.Stdout, line)
	}
	return nil
}

// runModel is cmd_model: json.dumps(model(), indent=2, sort_keys=True).
// It needs no config.
func runModel(_ context.Context, e *Env, _ Args) error {
	raw, err := json.Marshal(model.Export())
	if err != nil {
		return err
	}
	v, err := jsonx.Parse(raw)
	if err != nil {
		return err
	}
	e.Stdout.Write(jsonx.Format{Indent: 2, SortKeys: true}.Encode(v))
	fmt.Fprintln(e.Stdout)
	return nil
}

// runVersion is `factory version`: the commit and the Go version.
func runVersion(_ context.Context, e *Env, _ Args) error {
	fmt.Fprintln(e.Stdout, buildinfo.String())
	return nil
}

// cacheMountChecks are the mounts written as absolute paths that reach
// the trusted build caches (gocache.Reaches, links resolved here), which
// the gate and a helper refuse at run time for every job of every kind;
// config.Problems has already caught the ones written with {root}. A
// [gate] mount is checked against this host's root, a problem, as is one
// that reaches its gate-early, which every gate refuses too. A helper's
// root is not known here, so a [remote] or [remote.<host>] mount is
// checked against this host's root and {factory_home}/factory, a warning.
func cacheMountChecks(cfg *config.Config, root string) (problems, warnings []string) {
	if cfg == nil {
		return nil, nil
	}
	absolute := func(m string) bool { return strings.HasPrefix(m, "/") }
	for _, m := range cfg.Gate.Mounts {
		if absolute(m) && gocache.Reaches(root, m) {
			problems = append(problems, fmt.Sprintf("[gate] mounts: %q reaches %s, the trusted build caches, which only a warm may mount: every gate here would be refused",
				m, gocache.Root(root)))
		} else if early := filepath.Join(root, "gate-early"); absolute(m) && gocache.ReachesDir(root, early, m) {
			problems = append(problems, fmt.Sprintf("[gate] mounts: %q reaches %s, the gate's kept early checks: every gate here would be refused", m, early))
		}
	}
	roots := []string{root}
	if h := strings.TrimRight(cfg.Host.FactoryHome, "/"); h != "" && h+"/factory" != root {
		roots = append(roots, h+"/factory")
	}
	type table struct {
		label  string
		mounts []string
	}
	tables := []table{{"[remote]", cfg.Remote.Mounts}}
	for _, h := range cfg.Remote.Hosts {
		tables = append(tables, table{"[remote." + h.Name + "]", h.Mounts})
	}
	for _, tb := range tables {
		for _, m := range tb.mounts {
			if !absolute(m) {
				continue
			}
			for _, r := range roots {
				if gocache.Reaches(r, m) {
					warnings = append(warnings, fmt.Sprintf("%s mounts: %q reaches %s: a helper whose factory root is %s refuses every job with it",
						tb.label, m, gocache.Root(r), r))
					break
				}
			}
		}
	}
	return problems, warnings
}

// checkTokens is the token store as config.Problems reads it.
type checkTokens struct{ s *tokens.Store }

func (c checkTokens) Records() ([]config.TokenRecord, error) {
	f, err := c.s.Load()
	if err != nil {
		return nil, err
	}
	var out []config.TokenRecord
	for _, r := range f.Records() {
		if _, ok := r.Value().Object(); !ok {
			continue
		}
		out = append(out, config.TokenRecord{Name: r.NameString(), Projects: r.Projects()})
	}
	return out, nil
}

// intentAge is how old an intent may be before check warns.
const intentAge = time.Hour

// runCheck is cmd_check: config_problems, then the board's own checks.
// Warnings print first ("warning: …"), then problems; problems exit 1.
func runCheck(ctx context.Context, e *Env, a Args) error {
	cfg, _ := e.Config()
	problems, warnings := cfg.Problems(checkTokens{e.Tokens()})
	problems = append(problems, cfg.HostProblems(config.ThisHost())...)
	if err := e.Git(ctx); err != nil {
		problems = append(problems, err.Error()) // the git version is a contract
	}
	problems = append(problems, gitFileProblems(cfg)...)
	cp, cw := cacheMountChecks(cfg, e.Paths().Root)
	problems, warnings = append(problems, cp...), append(warnings, cw...)
	reader := e.Board().Reader
	tickets, _ := reader.List()
	warnings = append(warnings, oldIntents(tickets, e.Clock.Now())...)
	if a.Bool("board") {
		for _, p := range reader.CheckBoard() {
			problems = append(problems, "board: "+p.String())
		}
	}
	if a.Bool("asks") {
		for _, m := range reader.CheckAsks() {
			problems = append(problems, fmt.Sprintf("ask: %s is in %s with no stored ask", m.ID, m.Column))
		}
	}
	notes, bad := switchNotes(cfg, e.Paths().ProjectSwitches())
	warnings = append(warnings, bad...)
	if cfg != nil {
		notes = append(notes, cfg.KindNotes()...)
	}
	for _, n := range notes {
		fmt.Fprintln(e.Stdout, "note: "+n)
	}
	for _, w := range warnings {
		fmt.Fprintln(e.Stdout, "warning: "+w)
	}
	for _, p := range problems {
		fmt.Fprintln(e.Stdout, p)
	}
	if len(problems) > 0 {
		return Exit(1)
	}
	return nil
}

// switchNotes are the maintainer's project switches (path is
// foreman-state/projects.json), one note per factory project switched over
// config.toml's `enabled`, in config's order, and a warning for a file that
// does not read or names what is not a factory project (both ignored).
func switchNotes(cfg *config.Config, path string) (notes, warnings []string) {
	sw, err := config.ReadSwitches(path)
	if err != nil {
		return nil, []string{err.Error() + ": every project follows config.toml's enabled"}
	}
	if cfg == nil {
		return nil, nil
	}
	for _, p := range cfg.FactoryProjects() {
		if p.Switch != nil {
			notes = append(notes, fmt.Sprintf("[projects.%s] %s, over config's %s (`factory project follow %s` to follow config)",
				p.Key, p.SwitchNote(), p.ConfigState(), p.Key))
		}
	}
	var stray []string
	for k := range sw {
		if p, ok := cfg.Project(k); !ok || p.Mode != config.ModeFactory {
			stray = append(stray, k)
		}
	}
	sort.Strings(stray)
	for _, k := range stray {
		warnings = append(warnings, fmt.Sprintf("%s switches %s, which is not a factory project: ignored", filepath.Base(path), k))
	}
	return notes, warnings
}

// gitFileProblems is a pre-flight check: a factory project whose .git is
// not a directory, which the port refuses.
func gitFileProblems(cfg *config.Config) []string {
	var out []string
	for _, p := range cfg.FactoryProjects() {
		if p.Path == "" {
			continue
		}
		dotgit := filepath.Join(p.Path, ".git")
		if fi, err := os.Lstat(dotgit); err == nil && !fi.IsDir() {
			out = append(out, fmt.Sprintf("[projects.%s] %s is not a directory: the factory refuses a repository whose .git is a file or a link", p.Key, dotgit))
		}
	}
	return out
}

// oldIntents are the PR intents Foreman recorded that the tick has not
// carried out within the hour, in the order the tick drains them.
func oldIntents(ts []*ticket.Ticket, now time.Time) []string {
	var out []string
	for _, t := range ts {
		pr, ok := t.PR.Get()
		if !ok {
			continue
		}
		for _, in := range []struct {
			what string
			at   ticket.Opt[string]
		}{{"hold", pr.HoldRequested}, {"open", pr.OpenRequested}, {"merge", pr.MergeRequested}} {
			at, ok := in.at.Get()
			if !ok {
				continue
			}
			if when, err := clock.Parse(at); err == nil && now.Sub(when) <= intentAge {
				continue
			}
			out = append(out, fmt.Sprintf("%s asked to %s its PR at %s, over an hour ago, and the tick has not done it", t.ID, in.what, at))
		}
	}
	return out
}

// Python's view of the values json.load made: str(), repr(), truth,
// `x or y`, len(), slicing and str.ljust, over jsonx values. A lone
// surrogate (kept by jsonx as its three WTF-8 bytes) is one code point.
// (boardread keeps its own unexported copy of str and repr.)

func pyOr(v, def jsonx.Value) jsonx.Value {
	if jsonx.Truthy(v) {
		return v
	}
	return def
}

// isSurrogate: s starts with the WTF-8 bytes of U+D800..U+DFFF.
func isSurrogate(s string) bool {
	return len(s) >= 3 && s[0] == 0xED && s[1] >= 0xA0 && s[1] <= 0xBF && s[2] >= 0x80 && s[2] <= 0xBF
}

// pyRunes splits s into Python code points.
func pyRunes(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		if size == 1 && isSurrogate(s[i:]) {
			size = 3
		}
		out = append(out, s[i:i+size])
		i += size
	}
	return out
}

// pyPrintable is str.isprintable for one code point: not Other and not a
// separator but the space, which is Go's unicode.IsPrint.
func pyPrintable(r rune) bool { return unicode.IsPrint(r) }

// pySlice is s[:n].
func pySlice(s string, n int) string {
	r := pyRunes(s)
	if len(r) <= n {
		return s
	}
	return strings.Join(r[:n], "")
}

// ljust is f"{s:<n}": s padded with spaces to n code points.
func ljust(s string, n int) string {
	if k := len(pyRunes(s)); k < n {
		return s + strings.Repeat(" ", n-k)
	}
	return s
}
