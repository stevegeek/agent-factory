// Package cli is the factory command line (ARCHITECTURE.md §7): an
// in-house router with the argparse behaviours the callers rely on, the
// command table, and one file per command group with its output formats.
//
// Every command is Go (§15): there is no Python delegation.
package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/logx"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/store"
	"github.com/stevegeek/agent-factory/internal/ticketapi"
	"github.com/stevegeek/agent-factory/internal/tokens"
)

// FlagKind is what a flag takes.
type FlagKind int

const (
	StringFlag FlagKind = iota // --name VALUE
	BoolFlag                   // --name (store_true)
	IntFlag                    // --name N (type=int)
	FloatFlag                  // --name X (type=float)
	RepeatFlag                 // --name VALUE, repeatable (action="append")
)

// Flag is one --option of a command.
type Flag struct {
	Name     string // without the dashes
	Kind     FlagKind
	Default  string // what Args returns when the flag is absent ("" is none)
	Choices  []string
	Required bool
	Metavar  string // the value's name in the usage; NAME upper-cased by default
	Help     string
}

// Positional is one positional argument (nargs=None, or "?" when Optional).
type Positional struct {
	Name     string
	Optional bool
	Choices  []string
	Help     string
}

// Command is one subcommand.
type Command struct {
	Name        string
	Help        string
	Flags       []Flag
	Positionals []Positional
	// Rest names a trailing nargs="*" (RestMin 0) or nargs="+" (RestMin 1)
	// positional; "" is none.
	Rest    string
	RestMin int
	// Config says the command loads config.toml before it runs, as
	// Python's main() did for every command but model and ticket --json:
	// a missing or broken config fails it.
	Config bool
	// Run carries the command out on its parsed arguments.
	Run func(ctx context.Context, e *Env, a Args) error
	// Raw, when set instead of Run, is handed the arguments after the
	// command name unparsed and returns the exit code itself.
	Raw func(ctx context.Context, e *Env, args []string) int
}

// Args are a command's parsed arguments.
type Args struct {
	pos   map[string]string
	flags map[string][]string
	defs  map[string]string
	rest  []string
}

// Pos is a positional's value ("" when an optional one is absent).
func (a Args) Pos(name string) string { return a.pos[name] }

// Has reports whether a positional or flag was given.
func (a Args) Has(name string) bool {
	if _, ok := a.pos[name]; ok {
		return true
	}
	_, ok := a.flags[name]
	return ok
}

// Str is a flag's value, or its default.
func (a Args) Str(name string) string {
	if v := a.flags[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return a.defs[name]
}

// Bool is a store_true flag.
func (a Args) Bool(name string) bool { return a.Str(name) == "true" }

// Int is an int flag (validated when parsed), 0 when absent with no default.
func (a Args) Int(name string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(a.Str(name)))
	return n
}

// Float is a float flag (validated when parsed).
func (a Args) Float(name string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(a.Str(name)), 64)
	return f
}

// List is a repeatable flag's values in order, nil when absent.
func (a Args) List(name string) []string { return a.flags[name] }

// Rest is the trailing positionals.
func (a Args) Rest() []string { return a.rest }

// Text is the trailing positionals joined by spaces: " ".join(args.text).
func (a Args) Text() string { return strings.Join(a.rest, " ") }

// Refusal is a command that will not do what it was asked (Python's
// sys.exit("message")): the message alone on stderr, exit 1.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

// Refuse is a *Refusal with a formatted message.
func Refuse(format string, args ...any) error {
	return &Refusal{Msg: fmt.Sprintf(format, args...)}
}

// ExitCode ends a command with a code after it printed what it had to
// (check with problems, ticket --json refusals and crashes).
type ExitCode int

func (c ExitCode) Error() string { return fmt.Sprintf("exit %d", int(c)) }

// Exit is an ExitCode error.
func Exit(code int) error { return ExitCode(code) }

// UsageError is a command line the command cannot take: usage and the
// error on stderr, exit 2.
type UsageError struct{ Msg string }

func (u *UsageError) Error() string { return u.Msg }

// Usage is a *UsageError.
func Usage(format string, args ...any) error {
	return &UsageError{Msg: fmt.Sprintf(format, args...)}
}

// Main runs the factory command line: args is os.Args (args[0] is the
// program). It returns the exit code (§7.2).
func Main(args []string, env Env) int {
	e := &env
	e.init()
	out := bufio.NewWriter(e.Stdout)
	e.Stdout = out
	defer out.Flush()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan int, 1)
	if e.HandleSignals {
		caught := make(chan os.Signal, 1)
		sigs, ignoreHUP := signalsFor(args)
		if ignoreHUP {
			signal.Ignore(syscall.SIGHUP)
		}
		signal.Notify(caught, sigs...)
		defer signal.Stop(caught)
		go func() {
			select {
			case s := <-caught:
				if n, ok := s.(syscall.Signal); ok {
					got <- int(n)
				}
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	var rest []string
	if len(args) > 1 {
		rest = args[1:]
	}
	code := dispatch(ctx, e, commands(), rest)
	select {
	case n := <-got:
		return 128 + n
	default:
		return code
	}
}

// signalsFor is §5.4: every command is cancelled by SIGINT and SIGTERM;
// the long-running ones (the timers' tick, clock, sweep, rebuild and
// backup) ignore SIGHUP, and any other command is cancelled by it too.
func signalsFor(args []string) (catch []os.Signal, ignoreHUP bool) {
	name := ""
	if len(args) > 1 {
		name = args[1]
	}
	switch name {
	case "tick", "clock", "sweep", "rebuild", "backup":
		return []os.Signal{syscall.SIGINT, syscall.SIGTERM}, true
	}
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}, false
}

// dispatch picks the command named by argv[0], parses the rest and runs it.
func dispatch(ctx context.Context, e *Env, cmds []Command, argv []string) int {
	if len(argv) == 0 {
		return usageFail(e, topUsage(cmds), "factory", "the following arguments are required: command")
	}
	name := argv[0]
	if name == "-h" || name == "--help" {
		fmt.Fprint(e.Stdout, topHelp(cmds))
		return 0
	}
	var cmd *Command
	for i := range cmds {
		if cmds[i].Name == name {
			cmd = &cmds[i]
		}
	}
	if cmd == nil {
		if looksLikeFlag(name) {
			return usageFail(e, topUsage(cmds), "factory", "unrecognized arguments: "+name)
		}
		names := make([]string, len(cmds))
		for i, c := range cmds {
			names[i] = "'" + c.Name + "'"
		}
		return usageFail(e, topUsage(cmds), "factory",
			fmt.Sprintf("argument command: invalid choice: '%s' (choose from %s)", name, strings.Join(names, ", ")))
	}
	if cmd.Raw != nil {
		return cmd.Raw(ctx, e, argv[1:])
	}
	a, help, err := parse(cmd, argv[1:])
	if help {
		fmt.Fprint(e.Stdout, commandHelp(cmd))
		return 0
	}
	if err != nil {
		return usageFail(e, commandUsage(cmd), "factory "+cmd.Name, err.Error())
	}
	if cmd.Config {
		if _, err := e.Config(); err != nil {
			return report(e, cmd, err)
		}
	}
	return report(e, cmd, cmd.Run(ctx, e, a))
}

// report turns a command's error into its exit code and stderr (§7.2).
func report(e *Env, cmd *Command, err error) int {
	var (
		code ExitCode
		ref  *Refusal
		use  *UsageError
		tref *tokens.Refused
		aref *ticketapi.Refused
	)
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	case errors.As(err, &use):
		return usageFail(e, commandUsage(cmd), "factory "+cmd.Name, use.Msg)
	case errors.As(err, &ref):
		fmt.Fprintln(e.Stderr, ref.Msg)
	case errors.As(err, &tref):
		fmt.Fprintln(e.Stderr, tref.Msg)
	case errors.As(err, &aref):
		fmt.Fprintln(e.Stderr, aref.Message)
	default:
		fmt.Fprintf(e.Stderr, "factory: %v\n", err)
	}
	return 1
}

func usageFail(e *Env, usage, prog, msg string) int {
	fmt.Fprintf(e.Stderr, "%s%s: error: %s\n", usage, prog, msg)
	return 2
}

// looksLikeFlag is argparse's test for an option string: a leading dash,
// not a negative number, and no space (unless it names a known flag,
// which the caller checks first).
func looksLikeFlag(s string) bool {
	if len(s) < 2 || s[0] != '-' {
		return false
	}
	if isNegativeNumber(s) {
		return false
	}
	return !strings.Contains(s, " ")
}

func isNegativeNumber(s string) bool {
	if len(s) < 2 || s[0] != '-' {
		return false
	}
	// argparse: ^-\d+$|^-\d*\.\d+$
	body := s[1:]
	intPart, frac, dot := strings.Cut(body, ".")
	digits := func(x string) bool {
		for _, r := range x {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	if !dot {
		return body != "" && digits(body)
	}
	return frac != "" && digits(intPart) && digits(frac)
}

// parse reads a command's arguments the argparse way: options anywhere,
// --flag value and --flag=value, -- ends options, -h/--help. Prefix
// abbreviations are not accepted (§7.1).
func parse(cmd *Command, argv []string) (Args, bool, error) {
	a := Args{pos: map[string]string{}, flags: map[string][]string{}, defs: map[string]string{}}
	byName := map[string]*Flag{}
	for i := range cmd.Flags {
		f := &cmd.Flags[i]
		byName["--"+f.Name] = f
		if f.Default != "" {
			a.defs[f.Name] = f.Default
		}
	}
	var positionals []string
	opts := true
	for i := 0; i < len(argv); i++ {
		s := argv[i]
		if opts && s == "--" {
			opts = false
			continue
		}
		key, val, hasVal := strings.Cut(s, "=")
		f, known := byName[key]
		if !opts || !(known || looksLikeFlag(s)) {
			positionals = append(positionals, s)
			continue
		}
		if s == "-h" || s == "--help" {
			return a, true, nil
		}
		if !known {
			return a, false, fmt.Errorf("unrecognized arguments: %s", s)
		}
		if f.Kind == BoolFlag {
			if hasVal {
				return a, false, fmt.Errorf("argument --%s: ignored explicit argument '%s'", f.Name, val)
			}
			a.flags[f.Name] = []string{"true"}
			continue
		}
		if !hasVal {
			if i+1 >= len(argv) || argv[i+1] == "--" || looksLikeFlag(argv[i+1]) {
				return a, false, fmt.Errorf("argument --%s: expected one argument", f.Name)
			}
			i++
			val = argv[i]
		}
		if err := checkValue(f, val); err != nil {
			return a, false, err
		}
		if f.Kind == RepeatFlag {
			a.flags[f.Name] = append(a.flags[f.Name], val)
		} else {
			a.flags[f.Name] = []string{val}
		}
	}
	var missing []string
	n := 0
	for _, p := range cmd.Positionals {
		if n < len(positionals) {
			v := positionals[n]
			n++
			if len(p.Choices) > 0 && !contains(p.Choices, v) {
				return a, false, fmt.Errorf("argument %s: invalid choice: '%s' (choose from %s)", p.Name, v, quoteList(p.Choices))
			}
			a.pos[p.Name] = v
		} else if !p.Optional {
			missing = append(missing, p.Name)
		}
	}
	if cmd.Rest != "" {
		a.rest = append([]string(nil), positionals[n:]...)
		n = len(positionals)
		if len(a.rest) < cmd.RestMin {
			missing = append(missing, cmd.Rest)
		}
	}
	for _, f := range cmd.Flags {
		if f.Required && !a.Has(f.Name) {
			missing = append(missing, "--"+f.Name)
		}
	}
	if len(missing) > 0 {
		return a, false, fmt.Errorf("the following arguments are required: %s", strings.Join(missing, ", "))
	}
	if n < len(positionals) {
		return a, false, fmt.Errorf("unrecognized arguments: %s", strings.Join(positionals[n:], " "))
	}
	return a, false, nil
}

func checkValue(f *Flag, val string) error {
	switch f.Kind {
	case IntFlag:
		if _, err := strconv.Atoi(strings.TrimSpace(val)); err != nil {
			return fmt.Errorf("argument --%s: invalid int value: '%s'", f.Name, val)
		}
	case FloatFlag:
		if _, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err != nil {
			return fmt.Errorf("argument --%s: invalid float value: '%s'", f.Name, val)
		}
	}
	if len(f.Choices) > 0 && !contains(f.Choices, val) {
		return fmt.Errorf("argument --%s: invalid choice: '%s' (choose from %s)", f.Name, val, quoteList(f.Choices))
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func quoteList(list []string) string {
	q := make([]string, len(list))
	for i, s := range list {
		q[i] = "'" + s + "'"
	}
	return strings.Join(q, ", ")
}

func (f Flag) metavar() string {
	if f.Metavar != "" {
		return f.Metavar
	}
	if len(f.Choices) > 0 {
		return "{" + strings.Join(f.Choices, ",") + "}"
	}
	return strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))
}

func (p Positional) metavar() string {
	if len(p.Choices) > 0 {
		return "{" + strings.Join(p.Choices, ",") + "}"
	}
	return p.Name
}

// commandUsage is argparse's usage line: options first, then positionals.
func commandUsage(cmd *Command) string {
	parts := []string{"usage: factory " + cmd.Name, "[-h]"}
	for _, f := range cmd.Flags {
		s := "--" + f.Name
		if f.Kind != BoolFlag {
			s += " " + f.metavar()
		}
		if !f.Required {
			s = "[" + s + "]"
		}
		parts = append(parts, s)
	}
	for _, p := range cmd.Positionals {
		if p.Optional {
			parts = append(parts, "["+p.metavar()+"]")
		} else {
			parts = append(parts, p.metavar())
		}
	}
	if cmd.Rest != "" {
		if cmd.RestMin > 0 {
			parts = append(parts, cmd.Rest, "["+cmd.Rest+" ...]")
		} else {
			parts = append(parts, "["+cmd.Rest+" ...]")
		}
	}
	return strings.Join(parts, " ") + "\n"
}

func commandHelp(cmd *Command) string {
	var b strings.Builder
	b.WriteString(commandUsage(cmd))
	if cmd.Help != "" {
		b.WriteString("\n" + cmd.Help + "\n")
	}
	if len(cmd.Positionals) > 0 || cmd.Rest != "" {
		b.WriteString("\npositional arguments:\n")
		for _, p := range cmd.Positionals {
			fmt.Fprintf(&b, "  %-22s %s\n", p.metavar(), p.Help)
		}
		if cmd.Rest != "" {
			fmt.Fprintf(&b, "  %s\n", cmd.Rest)
		}
	}
	b.WriteString("\noptions:\n  -h, --help             show this help message and exit\n")
	for _, f := range cmd.Flags {
		s := "--" + f.Name
		if f.Kind != BoolFlag {
			s += " " + f.metavar()
		}
		fmt.Fprintf(&b, "  %-22s %s\n", s, f.Help)
	}
	return b.String()
}

func topUsage(cmds []Command) string {
	names := make([]string, len(cmds))
	for i, c := range cmds {
		names[i] = c.Name
	}
	return "usage: factory [-h] {" + strings.Join(names, ",") + "} ...\n"
}

func topHelp(cmds []Command) string {
	var b strings.Builder
	b.WriteString(topUsage(cmds))
	b.WriteString("\ncommands:\n")
	sorted := append([]Command(nil), cmds...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, c := range sorted {
		fmt.Fprintf(&b, "  %-14s %s\n", c.Name, c.Help)
	}
	b.WriteString("\nOptions go anywhere after the command; -- ends them. Unlike Python's argparse, " +
		"a flag is never abbreviated: give its whole name (--note, not --no).\n")
	return b.String()
}

// ioRunner adapts a group runner of the form run(args, stdout, stderr) int
// (cmd_render.go) into a Raw command.
func ioRunner(f func(args []string, stdout, stderr io.Writer) int) func(context.Context, *Env, []string) int {
	return func(_ context.Context, e *Env, args []string) int { return f(args, e.Stdout, e.Stderr) }
}

// Env is what a command runs with: its streams and environment, and the
// services built from them on first use (one invocation's worth).
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Environ is the process environment as a map: paths.FromEnv reads
	// FACTORY_ROOT, HOME and FACTORY_API_TOKENS from it, and
	// FACTORY_TEST_NOW (seam 3) fixes the clock.
	Environ map[string]string
	Exec    execx.Execer // nil: execx.OS
	Rand    io.Reader    // nil: crypto/rand
	Clock   clock.Clock  // nil: FACTORY_TEST_NOW's clock, or the system's
	// HandleSignals makes Main cancel the command on SIGINT, SIGTERM or
	// SIGHUP and exit 128+n (the real binary sets it; tests do not).
	HandleSignals bool

	paths   paths.Paths
	cfg     *config.Config
	cfgErr  error
	cfgRead bool
	log     *logx.Log
	board   *store.Board
	tokens  *tokens.Store
	// gitChecked and gitErr are Git's one check of git --version.
	gitChecked bool
	gitErr     error
	// ticketService replaces the ticket API in tests (a crash on demand).
	ticketService func(*Env) (ticketService, error)
}

func (e *Env) init() {
	if e.Stdin == nil {
		e.Stdin = strings.NewReader("")
	}
	if e.Stdout == nil {
		e.Stdout = io.Discard
	}
	if e.Stderr == nil {
		e.Stderr = io.Discard
	}
	if e.Environ == nil {
		e.Environ = map[string]string{}
	}
	e.paths = paths.FromEnv(e.Environ)
	if e.Clock == nil {
		c, err := clock.FromTestNow(e.Environ["FACTORY_TEST_NOW"])
		if err != nil {
			fmt.Fprintf(e.Stderr, "factory: FACTORY_TEST_NOW: %v; using the system clock\n", err)
			c = clock.System{}
		}
		e.Clock = c
	}
	if e.Exec == nil {
		e.Exec = execx.OS{}
	}
	if e.Rand == nil {
		e.Rand = rand.Reader
	}
}

// Paths is the factory's layout for this environment.
func (e *Env) Paths() paths.Paths { return e.paths }

// Log is the factory log (stderr and logs/factory.log).
func (e *Env) Log() *logx.Log {
	if e.log == nil {
		e.log = &logx.Log{Dir: e.paths.Logs, Clock: e.Clock, Stderr: e.Stderr}
	}
	return e.log
}

// Config is config.toml, read once per invocation (load_config), with the
// maintainer's project switches (foreman-state/projects.json) applied, so
// every command sees a project paused or switched on as the tick does.
func (e *Env) Config() (*config.Config, error) {
	if !e.cfgRead {
		e.cfgRead = true
		e.cfg, e.cfgErr = config.Load(e.paths.Config)
		if e.cfgErr == nil {
			e.cfg = e.cfg.Switched(e.paths.ProjectSwitches(), e.Log())
		}
	}
	return e.cfg, e.cfgErr
}

// ConfigSource is config.toml followed as it changes (config.Source), with
// the project switches applied as Config applies them.
func (e *Env) ConfigSource() *config.Source {
	return &config.Source{Path: e.paths.Config, Switches: e.paths.ProjectSwitches(), Log: e.Log()}
}

// Git refuses a git older than gitread.MinVersion (§4.8), once per
// invocation: every command that runs git calls it before its first git
// call, so an old git is refused up front instead of failing later inside
// merge-tree --write-tree.
func (e *Env) Git(ctx context.Context) error {
	if !e.gitChecked {
		e.gitChecked = true
		_, e.gitErr = gitread.CheckVersion(ctx, e.Exec, gitread.SafeEnv(doorsEnviron(e.Environ)))
	}
	return e.gitErr
}

// Board is the board, reading and writing under the root.
func (e *Env) Board() *store.Board {
	if e.board == nil {
		log := e.Log()
		e.board = &store.Board{Reader: &boardread.Reader{Paths: e.paths, Log: log}, Paths: e.paths, Clock: e.Clock, Log: log}
	}
	return e.board
}

// Tokens is the API token store.
func (e *Env) Tokens() *tokens.Store {
	if e.tokens == nil {
		e.tokens = tokens.New(e.paths, e.Clock, e.Log())
	}
	return e.tokens
}
