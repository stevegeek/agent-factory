package ops

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// AddIn is `factory add`'s arguments.
type AddIn struct {
	Project  string
	Title    string // "" with a recipe takes the recipe's
	Recipe   string
	Body     string
	BodyFile string // read when set; it wins over Body
	Door     string // human (the default), clock, github or peer
	Triage   bool   // file it into triage
	Host     string // auto, local or a [hosts] name: every stage runs there
	Task     bool   // one agent run whose result comes back to you
	Intent   bool   // an idea to design first
	Manage   bool   // a request for the manager
	Findings bool   // with Task: its agent reports structured findings, which a verifier checks
	Timeout  int    // minutes its agents may run; 0 is the default
	By       string // the Foreman user filing it; "" is the maintainer
}

// MaxBodyFile caps what --body-file reads: far above any ticket's body, far
// below anything that would hurt.
const MaxBodyFile = 16 << 20

// Add files a ticket (cmd_add) and returns its id. The checks and their
// messages are Python's, in its order, except that --host is checked before
// anything is filed: Python filed the ticket and then refused the host,
// leaving a ticket nobody was told about.
func (s *Service) Add(ctx context.Context, in AddIn) (ticket.ID, error) {
	cfg, err := s.config()
	if err != nil {
		return "", err
	}
	c := &call{s: s, cfg: cfg, now: s.now()}
	if err := c.actAs(in.By); err != nil {
		return "", err
	}
	if in.Door == "" {
		in.Door = string(model.DoorHuman)
	}
	if mode, ok := cfg.ProjectMode(in.Project); ok && mode == config.ModeBoard {
		return "", refuse(BadRequest, "%s is a board project: use `factory ticket create`", in.Project)
	}
	github := ""
	if p, ok := projectOf(cfg, in.Project); ok {
		github = p.GitHub
	}
	if in.Recipe != "" {
		r, ok := cfg.Recipe(in.Recipe)
		if !ok {
			return "", refuse(BadRequest, "no recipe %s; see `factory recipes`", in.Recipe)
		}
		if len(r.Projects) > 0 && !contains(r.Projects, in.Project) {
			return "", refuse(BadRequest, "recipe %s is for %s", in.Recipe, strings.Join(r.Projects, ", "))
		}
		if in.Title == "" {
			in.Title = r.Title
		}
		if in.Body == "" {
			in.Body = strings.ReplaceAll(r.Body, "{github}", github)
		}
		in.Task = in.Task || r.Kind == string(model.KindTask)
		in.Intent = in.Intent || r.Kind == string(model.KindIntent)
		in.Findings = in.Findings || r.Findings
		if r.Kind == string(model.KindProspect) {
			if !isFactoryProject(cfg, in.Project) {
				return "", refuse(BadRequest, "unknown project %s; known: %s", in.Project, strings.Join(factoryKeys(cfg), ", "))
			}
			t, err := s.Board.FileTicket(ctx, ticket.NewIn{Project: in.Project, Title: in.Title, Body: in.Body,
				Door: in.Door, Column: string(model.ColReady), By: c.by()})
			if err != nil {
				return "", err
			}
			t.Kind = ticket.Some(string(model.KindProspect))
			if err := s.Board.Save(t); err != nil {
				return "", err
			}
			return t.ID, nil
		}
	}
	if in.Title == "" {
		return "", refuse(BadRequest, "a title is needed (or --recipe)")
	}
	known := factoryKeys(cfg)
	if in.Task || in.Manage {
		known = append(known, model.NoticeProject)
	}
	if !contains(known, in.Project) {
		msg := fmt.Sprintf("unknown project %s; known: %s", in.Project, strings.Join(known, ", "))
		if !in.Task {
			msg += " (" + model.NoticeProject + " only with --task or --manage)"
		}
		return "", refuse(BadRequest, "%s", msg)
	}
	if in.Manage && in.Project != model.NoticeProject {
		return "", refuse(BadRequest, "the manager works on the %s project", model.NoticeProject)
	}
	kinds := 0
	for _, k := range []bool{in.Task, in.Intent, in.Manage} {
		if k {
			kinds++
		}
	}
	if kinds > 1 {
		return "", refuse(BadRequest, "--task, --intent and --manage are different kinds; pick one")
	}
	if in.Findings && !in.Task {
		return "", refuse(BadRequest, "--findings is for a task (--task): its findings go to a verifier")
	}
	body := in.Body
	if in.BodyFile != "" {
		if body, err = readBodyFile(in.BodyFile); err != nil {
			return "", err
		}
	}
	// a recipe's text names the project's repository as {github}, wherever the ticket came from
	body = strings.ReplaceAll(body, "{github}", github)
	if _, ok := cfg.Project(in.Project); in.Intent && !ok {
		return "", refuse(BadRequest, "an intent needs a real project")
	}
	if in.Host != "" {
		if err := knownHost(cfg, in.Host); err != nil {
			return "", err
		}
		if err := kindRefused(cfg, in.Host, in.Project); err != nil {
			return "", err
		}
	}
	single := in.Task || in.Intent || in.Manage
	column := model.ColReady
	if !single && (in.Triage || in.Door == string(model.DoorGitHub) || in.Door == string(model.DoorClock)) {
		column = model.ColTriage
	}
	t, err := s.Board.FileTicket(ctx, ticket.NewIn{Project: in.Project, Title: in.Title, Body: body, Door: in.Door,
		Column: string(column), By: c.by()})
	if err != nil {
		return "", err
	}
	switch {
	case in.Task:
		t.Kind = ticket.Some(string(model.KindTask))
		if in.Findings {
			t.FindingsAsked = ticket.Some(true)
		}
	case in.Intent:
		t.Kind = ticket.Some(string(model.KindIntent))
	case in.Manage:
		t.Kind = ticket.Some(string(model.KindManage))
		t.Host = ticket.Some(model.LocalHost) // its read-only view of the factory exists only here
	}
	if in.Timeout != 0 {
		t.TimeoutMinutes = ticket.Some(in.Timeout)
		// Python adds timeout_minutes before pin_host adds host: encoding
		// now fixes that key order in the ticket's tree
		ticket.Encode(t)
	}
	if in.Host != "" {
		c.pinHost(t, in.Host)
	}
	if err := s.Board.Save(t); err != nil {
		return "", err
	}
	return t.ID, nil
}

// File is Foreman's ＋ page: a ticket from the maintainer (by "") or another
// Foreman user through the human door, of kind change, intent, task or
// manage (add --body=… [--intent|--task|--manage] -- PROJECT TITLE).
func (s *Service) File(ctx context.Context, project, kind, title, body, by string) (ticket.ID, error) {
	in := AddIn{Project: project, Title: title, Body: body, By: by}
	switch kind {
	case "", string(model.KindChange):
	case string(model.KindIntent):
		in.Intent = true
	case string(model.KindTask):
		in.Task = true
	case string(model.KindManage):
		in.Manage = true
	default:
		return "", refuse(BadRequest, "a new ticket is a change, an intent, a task or a request for the manager")
	}
	return s.Add(ctx, in)
}

// readBodyFile is open(path).read(): text, with universal newlines.
func readBodyFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("body file: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxBodyFile+1))
	if err != nil {
		return "", fmt.Errorf("body file %s: %w", path, err)
	}
	if len(data) > MaxBodyFile {
		return "", refuse(BadRequest, "%s is larger than %d MiB", path, MaxBodyFile>>20)
	}
	if !utf8.Valid(data) {
		return "", refuse(BadRequest, "%s is not UTF-8 text", path)
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n"), nil
}

// knownHost refuses a host pin_host does not know.
func knownHost(cfg *config.Config, host string) error {
	if host == "auto" || host == model.LocalHost {
		return nil
	}
	if _, ok := cfg.HostNamed(host); ok {
		return nil
	}
	names := []string{"auto", model.LocalHost}
	for _, h := range cfg.Hosts {
		names = append(names, h.Name)
	}
	return refuse(BadRequest, "unknown host %s; known: %s", host, strings.Join(names, ", "))
}

// kindRefused refuses a pin to a host whose kinds or not_kinds leave out
// the project's kind: none of its stages would ever run there.
func kindRefused(cfg *config.Config, host, project string) error {
	p, ok := cfg.Project(project)
	if !ok {
		return nil
	}
	if why, refused := cfg.HostRefuses(host, *p); refused {
		return refuse(BadRequest, "cannot pin to %s", why)
	}
	return nil
}

// pinHost is pin_host for a host knownHost accepted: auto removes the pin;
// either way a host event says what was asked.
func (c *call) pinHost(t *ticket.Ticket, host string) {
	if host == "auto" {
		t.Host = ticket.Opt[string]{}
	} else {
		t.Host = ticket.Some(host)
	}
	ticket.Event(t, "host", host, "", ticket.Pinned{Host: host}, c.now)
}

func factoryKeys(cfg *config.Config) []string {
	var out []string
	for _, p := range cfg.FactoryProjects() {
		out = append(out, p.Key)
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// fields is a NewIn's extra keys, in order.
func fields(ms ...jsonx.Member) jsonx.Object { return jsonx.Object(ms) }
