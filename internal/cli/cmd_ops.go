package cli

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/jobtail"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/memstore"
	"github.com/stevegeek/agent-factory/internal/ops"
	"github.com/stevegeek/agent-factory/internal/panel"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/prflow"
	"github.com/stevegeek/agent-factory/internal/scionread"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticket"
	"github.com/stevegeek/agent-factory/internal/ticketapi"
	"github.com/stevegeek/agent-factory/internal/wake"
)

// The maintainer's verbs: add, move, note, reply, ask,
// approve, redirect, merge, pr, host, migrate, wake and answer, each a thin
// layer over package ops. The CLI holds the App's credentials, so its
// PRActions are synchronous (prActions over prflow): `move` out of pr_open
// turns the PR back to draft before it returns, and `pr` prints the URL of
// the PR it opened. The command table (commands.go) names their runners:
//
//	add: runAdd, move: runMove, note: runNote, reply: runReply, ask: runAsk,
//	approve: runApprove, redirect: runRedirect, merge: runMerge, pr: runPR,
//	host: runHost, migrate: runMigrate, wake: runWake, answer: runAnswer
//
// What a verb prints is its result only, on stdout; a refusal is its
// message alone on stderr with exit 1.

// opsCLI wires the verbs for one invocation. HTTP is GitHub's transport;
// nil is a real http.Client (tests pass a fake).
type opsCLI struct {
	HTTP github.Doer
}

func runAdd(ctx context.Context, e *Env, a Args) error      { return opsCLI{}.add(ctx, e, a) }
func runMove(ctx context.Context, e *Env, a Args) error     { return opsCLI{}.move(ctx, e, a) }
func runNote(ctx context.Context, e *Env, a Args) error     { return opsCLI{}.note(ctx, e, a) }
func runReply(ctx context.Context, e *Env, a Args) error    { return opsCLI{}.reply(ctx, e, a) }
func runAsk(ctx context.Context, e *Env, a Args) error      { return opsCLI{}.ask(ctx, e, a) }
func runApprove(ctx context.Context, e *Env, a Args) error  { return opsCLI{}.approve(ctx, e, a) }
func runRedirect(ctx context.Context, e *Env, a Args) error { return opsCLI{}.redirect(ctx, e, a) }
func runMerge(ctx context.Context, e *Env, a Args) error    { return opsCLI{}.merge(ctx, e, a) }
func runPR(ctx context.Context, e *Env, a Args) error       { return opsCLI{}.pr(ctx, e, a) }
func runHost(ctx context.Context, e *Env, a Args) error     { return opsCLI{}.host(ctx, e, a) }
func runMigrate(ctx context.Context, e *Env, a Args) error  { return opsCLI{}.migrate(ctx, e, a) }
func runWake(ctx context.Context, e *Env, a Args) error     { return opsCLI{}.wake(ctx, e, a) }
func runAnswer(ctx context.Context, e *Env, a Args) error   { return opsCLI{}.answer(ctx, e, a) }
func runFindings(ctx context.Context, e *Env, a Args) error { return opsCLI{}.findings(ctx, e, a) }
func runProject(ctx context.Context, e *Env, a Args) error  { return opsCLI{}.project(ctx, e, a) }

// runners is every verb of this file and cmd_inspect.go wired with o, by
// command name.
func (o opsCLI) runners() map[string]func(context.Context, *Env, Args) error {
	return map[string]func(context.Context, *Env, Args) error{
		"add": o.add, "move": o.move, "note": o.note, "reply": o.reply, "ask": o.ask, "approve": o.approve,
		"redirect": o.redirect, "merge": o.merge, "pr": o.pr, "host": o.host, "migrate": o.migrate, "wake": o.wake,
		"answer": o.answer, "look": o.look, "audit": o.audit, "memory": o.memory, "github": o.github,
		"gate-workflow": o.gateWorkflow, "findings": o.findings, "project": o.project,
	}
}

func (o opsCLI) add(ctx context.Context, e *Env, a Args) error {
	s, err := o.service(e)
	if err != nil {
		return err
	}
	id, err := s.Add(ctx, ops.AddIn{
		Project: a.Pos("project"), Title: a.Pos("title"), Recipe: a.Str("recipe"), Body: a.Str("body"),
		BodyFile: a.Str("body-file"), Door: a.Str("door"), Triage: a.Bool("triage"), Host: a.Str("host"),
		Task: a.Bool("task"), Intent: a.Bool("intent"), Manage: a.Bool("manage"), Timeout: a.Int("timeout"),
		Findings: a.Bool("findings"),
	})
	if err != nil {
		return refusal(err)
	}
	fmt.Fprintln(e.Stdout, id)
	return nil
}

// move prints nothing; a PR that could not be turned back to draft is a
// warning on stderr (the move itself is saved).
func (o opsCLI) move(ctx context.Context, e *Env, a Args) error {
	s, err := o.service(e)
	if err != nil {
		return err
	}
	warning, err := s.Move(ctx, a.Pos("ticket"), a.Pos("column"), a.Str("note"))
	if err != nil {
		return refusal(err)
	}
	if warning != "" {
		fmt.Fprintln(e.Stderr, warning)
	}
	return nil
}

func (o opsCLI) note(ctx context.Context, e *Env, a Args) error {
	s, err := o.service(e)
	if err != nil {
		return err
	}
	return refusal(s.Note(ctx, a.Pos("ticket"), a.Text()))
}

func (o opsCLI) reply(ctx context.Context, e *Env, a Args) error {
	s, err := o.service(e)
	if err != nil {
		return err
	}
	line, err := s.Reply(ctx, a.Pos("ticket"), a.Text(), strings.Split(a.Str("option"), ",")...)
	if err != nil {
		return refusal(err)
	}
	fmt.Fprintln(e.Stdout, line)
	return nil
}

func (o opsCLI) ask(ctx context.Context, e *Env, a Args) error {
	s, err := o.service(e)
	if err != nil {
		return err
	}
	id, err := s.Ask(ctx, a.Pos("ticket"), a.Text())
	if err != nil {
		return refusal(err)
	}
	fmt.Fprintln(e.Stdout, id)
	return nil
}

// approve prints the filed tickets' ids, space-separated.
func (o opsCLI) approve(ctx context.Context, e *Env, a Args) error {
	only, err := pyInts(a.Str("only"))
	if err != nil {
		return err
	}
	s, err := o.service(e)
	if err != nil {
		return err
	}
	ids, err := s.Approve(ctx, a.Pos("ticket"), only)
	if err != nil {
		return refusal(err)
	}
	fmt.Fprintln(e.Stdout, joinIDs(ids))
	return nil
}

func (o opsCLI) redirect(ctx context.Context, e *Env, a Args) error {
	s, err := o.service(e)
	if err != nil {
		return err
	}
	return refusal(s.Redirect(ctx, a.Pos("ticket"), a.Str("note")))
}

func (o opsCLI) merge(ctx context.Context, e *Env, a Args) error {
	s, err := o.service(e)
	if err != nil {
		return err
	}
	line, err := s.RequestMerge(ctx, a.Pos("ticket"))
	if err != nil {
		return refusal(err)
	}
	fmt.Fprintln(e.Stdout, line)
	return nil
}

// pr opens or updates the ticket's PR now and prints its URL.
func (o opsCLI) pr(ctx context.Context, e *Env, a Args) error {
	s, err := o.service(e)
	if err != nil {
		return err
	}
	if err := e.Git(ctx); err != nil {
		return err
	}
	url, err := s.OpenPR(ctx, a.Pos("ticket"))
	if err != nil {
		return refusal(err)
	}
	fmt.Fprintln(e.Stdout, url)
	return nil
}

func (o opsCLI) host(ctx context.Context, e *Env, a Args) error {
	s, err := o.service(e)
	if err != nil {
		return err
	}
	return refusal(s.Host(ctx, a.Pos("ticket"), a.Pos("host")))
}

// migrate prints one line per change.
func (o opsCLI) migrate(ctx context.Context, e *Env, _ Args) error {
	s, err := o.service(e)
	if err != nil {
		return err
	}
	lines, err := s.Migrate(ctx)
	for _, l := range lines {
		fmt.Fprintln(e.Stdout, l)
	}
	return refusal(err)
}

func (o opsCLI) wake(ctx context.Context, e *Env, _ Args) error {
	return waker(e).Wake(ctx)
}

// project is `factory project enable|disable|follow NAME`: the switch the
// maintainer has on Foreman's Boards (ops.SwitchProject), for a shell over
// ssh. It needs no GitHub, so its service is the board, the config and the
// wake. It prints what the switch did.
func (o opsCLI) project(ctx context.Context, e *Env, a Args) error {
	cfg, err := e.Config()
	if err != nil {
		return err
	}
	s := &ops.Service{Board: e.Board(), Config: loadedConfig{cfg}, Clock: e.Clock, Wake: waker(e), Log: e.Log()}
	state := map[string]string{"enable": ops.SwitchOn, "disable": ops.SwitchOff, "follow": ops.SwitchFollow}[a.Pos("verb")]
	line, err := s.SwitchProject(ctx, a.Pos("name"), state, "")
	if err != nil {
		return err
	}
	fmt.Fprintln(e.Stdout, line)
	return nil
}

// answer is the desk's answer to what a ticket asks (ops.Answer), from the
// command line: `answer T VALUE [--note TEXT] [--only 1,3] [--kind K
// --since TS]`. It prints what the answer printed (the filed ids, the PR's
// URL, the reply's line), if anything. Approving a design without --only
// files every proposed ticket, as `approve` does.
func (o opsCLI) answer(ctx context.Context, e *Env, a Args) error {
	picks, err := pyInts(a.Str("only"))
	if err != nil {
		return err
	}
	s, err := o.service(e)
	if err != nil {
		return err
	}
	id, value := a.Pos("ticket"), a.Pos("value")
	if value == "approve" && len(picks) == 0 {
		if t, err := loadTicket(e, id); err == nil {
			for i := range t.Proposed {
				picks = append(picks, i+1)
			}
		}
	}
	out, err := s.Answer(ctx, ops.AnswerIn{ID: id, Value: value, Note: a.Str("note"), Picks: picks,
		Kind: a.Str("kind"), Since: a.Str("since"), Options: strings.Split(a.Str("option"), ",")})
	if err != nil {
		return refusal(err)
	}
	if out != "" {
		fmt.Fprintln(e.Stdout, out)
	}
	return nil
}

// findings is `findings T [--file 1,3|all] [--as 1=intent,...] [--none]
// [--verify]`: with no flag it prints the check's verified findings;
// --file files the ones named (printing the new ids) and closes the check,
// --none closes it with none filed, --verify sends a finished task's
// result to the verifier.
func (o opsCLI) findings(ctx context.Context, e *Env, a Args) error {
	file, none, verify := a.Str("file"), a.Bool("none"), a.Bool("verify")
	n := 0
	for _, on := range []bool{file != "", none, verify} {
		if on {
			n++
		}
	}
	if n > 1 {
		return Refuse("--file, --none and --verify are different answers; pick one")
	}
	if a.Str("as") != "" && file == "" {
		return Refuse("--as goes with --file")
	}
	as := map[int]string{}
	if s := a.Str("as"); s != "" {
		for _, item := range strings.Split(s, ",") {
			num, kind, ok := strings.Cut(strings.TrimSpace(item), "=")
			k, isInt := intLiteral(num)
			if !ok || !isInt || kind == "" {
				return Refuse("--as takes N=change or N=intent, comma-separated: %s", item)
			}
			as[k] = kind
		}
	}
	id := a.Pos("ticket")
	s, err := o.service(e)
	if err != nil {
		return err
	}
	switch {
	case verify:
		return refusal(s.VerifyFindings(ctx, id))
	case none:
		return refusal(s.DismissFindings(ctx, id))
	case file == "":
		out, err := s.Findings(ctx, id)
		if err != nil {
			return refusal(err)
		}
		fmt.Fprintln(e.Stdout, out)
		return nil
	}
	var picks []int
	if file == "all" {
		t, err := loadTicket(e, id)
		if err != nil {
			return err
		}
		for i, f := range t.Findings {
			if f.Open() {
				picks = append(picks, i+1)
			}
		}
	} else if picks, err = pyInts(file); err != nil {
		return err
	}
	ids, err := s.FileFindings(ctx, id, picks, as)
	if err != nil {
		return refusal(err)
	}
	fmt.Fprintln(e.Stdout, joinIDs(ids))
	return nil
}

// pyInts is [int(x) for x in s.split(",")] when s is not empty, with
// int()'s whitespace, sign and underscore rules and its ValueError as the
// refusal.
func pyInts(s string) ([]int, error) {
	if s == "" {
		return nil, nil
	}
	var out []int
	for _, x := range strings.Split(s, ",") {
		n, ok := intLiteral(x)
		if !ok {
			return nil, Refuse("invalid literal for int() with base 10: %s", pyQuote(x))
		}
		out = append(out, n)
	}
	return out, nil
}

// intLiteral is int(x) for base 10: surrounding whitespace, one sign,
// Unicode decimal digits with single underscores between them.
func intLiteral(x string) (int, bool) {
	s := strings.TrimSpace(x)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg, s = s[0] == '-', s[1:]
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return 0, false
	}
	n, ok := pyInt(strings.ReplaceAll(s, "_", ""))
	if !ok || n > math.MaxInt32 {
		return 0, false
	}
	if neg {
		n = -n
	}
	return int(n), true
}

// pyQuote is repr() of a str that holds no quotes or control characters,
// and a fair approximation otherwise.
func pyQuote(s string) string {
	if !strings.Contains(s, "'") {
		return "'" + strings.ReplaceAll(s, `\`, `\\`) + "'"
	}
	if !strings.Contains(s, `"`) {
		return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
	}
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

func joinIDs(ids []ticket.ID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = string(id)
	}
	return strings.Join(s, " ")
}

// refusal turns a verb's refusal into the command's: its message
// alone on stderr, exit 1. Any other error stays as it is (factory: …).
func refusal(err error) error {
	if err == nil {
		return nil
	}
	var (
		or  *ops.Refused
		pr  *prflow.Refusal
		nf  *store.NotFoundError
		bz  *store.BusyError
		na  *ops.ErrNoAgent
		cf  *memstore.ConflictError
		ref *Refusal
	)
	switch {
	case errors.As(err, &ref):
		return err
	case errors.As(err, &or):
		return &Refusal{Msg: or.Message}
	case errors.As(err, &pr):
		return &Refusal{Msg: pr.Message}
	case errors.As(err, &nf):
		return &Refusal{Msg: nf.Error()}
	case errors.As(err, &bz):
		return &Refusal{Msg: bz.Error()}
	case errors.As(err, &na):
		return &Refusal{Msg: na.Error()}
	case errors.As(err, &cf):
		return &Refusal{Msg: cf.Error()}
	case errors.Is(err, ops.ErrNoteNeeded), errors.Is(err, ops.ErrStale), errors.Is(err, memstore.ErrRootCommit),
		errors.Is(err, memstore.ErrNoStore):
		return &Refusal{Msg: err.Error()}
	}
	return err
}

// service is the ops service for this invocation, on the config main()
// loaded. Nothing in it reads the App key or runs anything until a verb
// needs it.
func (o opsCLI) service(e *Env) (*ops.Service, error) {
	cfg, err := e.Config()
	if err != nil {
		return nil, err
	}
	p := e.Paths()
	log := e.Log()
	env := environList(e.Environ)
	guard := gitx.NewGuarder(e.Exec, env, log)
	return &ops.Service{
		Board:  e.Board(),
		Config: loadedConfig{cfg},
		Clock:  e.Clock,
		Wake:   waker(e),
		PR:     prActions{flow: o.flow(e, cfg, env, guard)},
		Git:    ops.GitBranches{Repo: gitread.Repo{Exec: e.Exec, Env: env, Guard: guard}},
		Scion:  scionread.Reader{Bin: p.Scion, Exec: e.Exec, Env: gitread.SafeEnv(env)},
		Jobs:   jobTails{read: jobtail.Reader{Exec: e.Exec, Env: env}, home: e},
		Notes:  boardNoter{e: e},
		Panel:  panelRules{},
		Log:    log,
		AskOf:  present.AskFor,
	}, nil
}

// flow is the PR flow the CLI's PR actions use: the App's client, git
// through the process's guard. It merges nothing (the tick does), so it
// has no merge check.
func (o opsCLI) flow(e *Env, cfg *config.Config, env []string, guard *gitx.Guarder) *prflow.Flow {
	repo := func(p config.Project) *gitread.Repo {
		return &gitread.Repo{Path: p.Path, Base: p.Base, GitHub: p.GitHub, Exec: e.Exec, Env: env, Guard: guard}
	}
	return &prflow.Flow{
		GitHub:  o.githubClient(e, cfg),
		Git:     prflow.Repos{Repo: repo},
		Board:   e.Board(),
		LogsDir: e.Paths().Logs,
		Clock:   e.Clock,
		Log:     e.Log(),
	}
}

// githubClient is the App's authenticated client. The App's clock is the
// real one even when FACTORY_TEST_NOW fixes the factory's: GitHub checks the
// JWT and the token's expiry against its own time. FACTORY_GITHUB_API (seam
// 1) points it elsewhere.
func (o opsCLI) githubClient(e *Env, cfg *config.Config) *github.Client {
	var doer github.Doer = &http.Client{}
	if o.HTTP != nil {
		doer = o.HTTP
	}
	c := &github.Client{BaseURL: e.Environ["FACTORY_GITHUB_API"], NPMURL: e.Environ["FACTORY_NPM_REGISTRY"], HTTP: doer}
	c.Auth = &github.App{
		ClientID:       cfg.GitHub.AppClientID,
		InstallationID: cfg.GitHub.InstallationID,
		KeyPath:        appKeyPath(e, cfg),
		Clock:          clock.System{},
		API:            c,
	}
	return c
}

// appKeyPath is os.path.expanduser([github] key), by default
// ~/.config/factory/github-app.pem.
func appKeyPath(e *Env, cfg *config.Config) string {
	p := e.Paths()
	key := cfg.GitHub.Key
	switch {
	case key == "":
		return p.AppKey
	case key == "~":
		return p.Home
	case strings.HasPrefix(key, "~/"):
		return filepath.Join(p.Home, key[2:])
	}
	return key
}

// waker starts a tick (FACTORY_WAKE_LOG only notes it).
func waker(e *Env) wake.Waker {
	var sp wake.Spawner = execx.OS{}
	if s, ok := e.Exec.(wake.Spawner); ok {
		sp = s
	}
	return wake.Waker{Paths: e.Paths(), Exec: e.Exec, Spawn: sp, Env: e.Environ, Clock: e.Clock}
}

// environList is the environment map as KEY=value lines, sorted.
func environList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// loadedConfig is the config main() loaded, as ops reads it: one snapshot
// for the whole invocation, as Python's load_config() was.
type loadedConfig struct{ cfg *config.Config }

func (l loadedConfig) Current() (*config.Config, error) { return l.cfg, nil }

// prActions is ops.PRActions done at once with the App's credentials
// (prflow): the CLI's, where Foreman's only records intents. Both are called
// by ops under the ticket's lock, so they change the ticket in hand and
// never take the lock again (prflow.Sync would).
type prActions struct{ flow *prflow.Flow }

// Hold is hold_pr: the PR back to draft now, or a *prflow.NotHeldError
// (recorded on the ticket) that ops turns into the warning.
func (p prActions) Hold(ctx context.Context, cfg *config.Config, t *ticket.Ticket) error {
	return p.flow.HoldPR(ctx, cfg, t)
}

// Open is open_pr: the branch pushed, the PR opened or updated, its URL.
func (p prActions) Open(ctx context.Context, cfg *config.Config, t *ticket.Ticket) (string, error) {
	proj, ok := cfg.Project(t.Project)
	if !ok {
		return "", Refuse("unknown project %s", t.Project)
	}
	pr, err := p.flow.OpenPR(ctx, cfg, *proj, t)
	if errors.Is(err, prflow.ErrUnread) {
		// public review has not passed the commit: the ticket went back to
		// the gate, which has it read first, and that move is saved
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return pr.URL, nil
}

// jobTailTimeout is remote(h, "tail", …, timeout=20).
const jobTailTimeout = 20 * time.Second

// jobTails is ops.JobTails over jobtail: the helper's [hosts] entry as the
// dispatcher reaches it (its key, ~ expanded).
type jobTails struct {
	read jobtail.Reader
	home *Env
}

func (j jobTails) Tail(ctx context.Context, h config.HostCfg, job string) (string, error) {
	if h.Pull() {
		// a pull host's job: the log tail its last beat left in the mailbox
		return jobtail.MailTail(j.home.Paths().Mailbox(), h.Name, job)
	}
	ctx, cancel := context.WithTimeout(ctx, jobTailTimeout)
	defer cancel()
	return j.read.Tail(ctx, hostrun.HostOf(j.home.Paths(), h), job)
}

// boardNoter is ops.BoardNoter through the ticket API's note verb, as the
// maintainer: a person's note on a board ticket joins its conversation.
type boardNoter struct{ e *Env }

func (b boardNoter) BoardNote(ctx context.Context, id ticket.ID, text string) error {
	svc, err := b.e.ticketAPI()
	if err != nil {
		return err
	}
	req := jsonx.ObjectOf(jsonx.Member{Key: "id", Value: jsonx.String(string(id))},
		jsonx.Member{Key: "text", Value: jsonx.String(text)})
	_, err = svc.Do(ctx, ticketapi.ForPerson(), "note", req, time.Duration(store.CLIWait))
	return err
}

// panelRules is ops.ProspectPanel from package panel.
type panelRules struct{}

func (panelRules) Survives(i int, votes []ticket.Vote) bool { return panel.Tally(i, votes).Survives }

func (panelRules) Votes(i int, votes []ticket.Vote) []ticket.PanelVote { return panel.Votes(i, votes) }

func (panelRules) FileProposal(id ticket.ID, t *ticket.Ticket, i int, at string, now time.Time) *ticket.Ticket {
	return panel.File(t, i, t.Proposals[i-1], t.Votes, id, now, at)
}
