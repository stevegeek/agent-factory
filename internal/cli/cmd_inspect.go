package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/stevegeek/agent-factory/internal/gate"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/memory"
	"github.com/stevegeek/agent-factory/internal/memstore"
)

// The inspect commands: look, audit, memory, github and
// gate-workflow. They print; only `memory --revert` changes anything (one
// memory store). The command table (commands.go) names their runners:
//
//	look: runLook, audit: runAudit, memory: runMemory, github: runGitHub,
//	gate-workflow: runGateWorkflow

func runLook(ctx context.Context, e *Env, a Args) error   { return opsCLI{}.look(ctx, e, a) }
func runAudit(ctx context.Context, e *Env, a Args) error  { return opsCLI{}.audit(ctx, e, a) }
func runMemory(ctx context.Context, e *Env, a Args) error { return opsCLI{}.memory(ctx, e, a) }
func runGitHub(ctx context.Context, e *Env, a Args) error { return opsCLI{}.github(ctx, e, a) }
func runGateWorkflow(ctx context.Context, e *Env, a Args) error {
	return opsCLI{}.gateWorkflow(ctx, e, a)
}

// look is cmd_look: the header line "name · stage[ @host] · started X",
// then what the agent shows now, the last --lines lines (clamped 10..400).
// Read-only: nothing here can type into an agent.
func (o opsCLI) look(ctx context.Context, e *Env, a Args) error {
	if _, err := loadTicket(e, a.Pos("ticket")); err != nil {
		return err
	}
	s, err := o.service(e)
	if err != nil {
		return err
	}
	screen, err := s.Look(ctx, a.Pos("ticket"), a.Int("lines"))
	if err != nil {
		return refusal(err)
	}
	fmt.Fprintln(e.Stdout, screen.String())
	return nil
}

// audit is cmd_audit: audit_text(t) as it is, with no newline added.
func (o opsCLI) audit(_ context.Context, e *Env, a Args) error {
	t, err := loadTicket(e, a.Pos("ticket"))
	if err != nil {
		return err
	}
	text, err := gate.AuditText(t)
	if err != nil {
		return err
	}
	_, err = io.WriteString(e.Stdout, text)
	return err
}

// memory is cmd_memory: each matching store's last 20 commits, or with
// --revert one commit undone in the store named by project and role. A
// store that does not exist is a clean refusal (Python printed a
// traceback).
func (o opsCLI) memory(ctx context.Context, e *Env, a Args) error {
	if err := e.Git(ctx); err != nil {
		return err
	}
	p := e.Paths()
	svc := &memory.Service{Store: &memstore.Store{Paths: p, Exec: e.Exec}, Paths: p, Clock: e.Clock}
	project, role := a.Pos("project"), a.Pos("role")
	if c := a.Str("revert"); c != "" {
		if project == "" || role == "" {
			return Refuse("--revert needs the project and the role")
		}
		line, err := svc.Revert(ctx, project, role, c)
		if err != nil {
			if errors.Is(err, memstore.ErrNoStore) {
				return Refuse("no memory store for %s/%s", project, role)
			}
			return refusal(err)
		}
		fmt.Fprintln(e.Stdout, line)
		return nil
	}
	return svc.Listing(ctx, e.Stdout, project, role)
}

// github is cmd_github: the repositories the App's installation reaches
// ("(private)" after a private one, a space after a public one, as
// print(name, "") wrote), then the permissions of a freshly minted
// installation token, sorted by name.
func (o opsCLI) github(ctx context.Context, e *Env, _ Args) error {
	cfg, err := e.Config()
	if err != nil {
		return err
	}
	gh := o.githubClient(e, cfg)
	repos, err := gh.Repos(ctx)
	if err != nil {
		return err
	}
	for _, r := range repos {
		mark := ""
		if r.Private {
			mark = "(private)"
		}
		fmt.Fprintln(e.Stdout, r.FullName, mark)
	}
	app, ok := gh.Auth.(*github.App)
	if !ok {
		return errors.New("github: no App")
	}
	inst, err := app.Mint(ctx)
	if err != nil {
		return err
	}
	perms := make([]string, 0, len(inst.Permissions))
	for _, m := range inst.Permissions {
		perms = append(perms, m.Key+"="+pyText(m.Value))
	}
	sort.Strings(perms)
	fmt.Fprintln(e.Stdout, "permissions:", strings.Join(perms, ", "))
	return nil
}

// pyText is str(v) for a value GitHub sent: a string as it is, True,
// False and None as Python writes them, anything else as JSON.
func pyText(v jsonx.Value) string {
	switch v.Kind() {
	case jsonx.KindString:
		s, _ := v.Str()
		return s
	case jsonx.KindBool:
		if b, _ := v.Bool(); b {
			return "True"
		}
		return "False"
	case jsonx.KindNull:
		return "None"
	}
	return string(jsonx.EncodePython(v, -1))
}

// gateWorkflow is cmd_gate_workflow: the project's GitHub Actions
// workflow, as it is.
func (o opsCLI) gateWorkflow(_ context.Context, e *Env, a Args) error {
	cfg, err := e.Config()
	if err != nil {
		return err
	}
	y, err := gate.Workflow(cfg, a.Pos("project"))
	if err != nil {
		return &Refusal{Msg: err.Error()}
	}
	_, err = io.WriteString(e.Stdout, y)
	return err
}
