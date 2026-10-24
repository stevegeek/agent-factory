// Package scionread is read-only Scion (ARCHITECTURE.md §2.3): `scion list`
// and `scion look`, the only verbs Foreman's code may reach. It contains no
// verb that changes an agent.
//
// The read verbs run without the Claude token or an API key in their
// environment. Scion's look is `tmux capture-pane` run in the agent's
// container through the runtime (cmd/look.go), and list reads Scion's own
// agent state: neither starts a harness, so neither needs the token. This
// is what lets the Foreman unit make the token file inaccessible (§6.2).
package scionread

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// Timeout is the limit on every scion call (Python scion(), F:678).
const Timeout = 600 * time.Second

// Variables a scion read verb never gets, whatever the caller's base holds.
const (
	TokenVar  = "CLAUDE_CODE_OAUTH_TOKEN"
	APIKeyVar = "ANTHROPIC_API_KEY"
)

// Reader runs scion's read verbs.
type Reader struct {
	Bin  string       // paths.Scion: ~/.local/bin/scion
	Exec execx.Execer // the OS execer, or a fake
	// Env is the base environment, normally the safe git environment
	// (gitread.SafeEnv over the process environment): Scion runs git on the
	// host too. ReadEnv strips the secrets from it.
	Env []string
}

// ReadEnv is the environment of a read verb: base without the token and
// the API key, and with scion's directory put first on PATH exactly as
// scion_env() does (os.path.dirname(SCION) + ":" + PATH, PATH "" if unset).
func ReadEnv(base []string, bin string) []string {
	out := make([]string, 0, len(base)+1)
	path := ""
	for _, kv := range base {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case TokenVar, APIKeyVar:
			continue
		case "PATH":
			path = v
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PATH="+filepath.Dir(bin)+":"+path)
}

// CheckName refuses an agent name that could be read as an option or a
// path, or that a glob would expand. The factory's names are like t0042-w1.
func CheckName(name string) error {
	ok := name != "" && name[0] != '-' && name[0] != '.' && len(name) <= 128
	for i := 0; ok && i < len(name); i++ {
		c := name[i]
		ok = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
	}
	if !ok {
		return fmt.Errorf("bad agent name %q", name)
	}
	return nil
}

// run runs scion with args in dir. It stays unexported: an exported
// runner would let a caller name a write verb through this package.
func (r Reader) run(ctx context.Context, dir string, env []string, args ...string) (execx.Result, error) {
	argv := append([]string{r.Bin}, args...)
	res, err := r.Exec.Run(ctx, execx.Cmd{Argv: argv, Dir: dir, Env: env, Timeout: Timeout})
	if err != nil {
		return res, fmt.Errorf("scion %s: %w", firstArg(args), err)
	}
	return res, nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// List is `scion list --format json` in a project: the names of the agents
// Scion runs there. A failed or unreadable list is an error, never an empty
// list (running_agents, F:686).
func (r Reader) List(ctx context.Context, dir string) ([]string, error) {
	res, err := r.run(ctx, dir, ReadEnv(r.Env, r.Bin), "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	if err := execx.CheckErr(res); err != nil {
		return nil, err
	}
	return parseList(res.Stdout)
}

// ErrList is a scion list answer that is not a list of named agents.
var ErrList = errors.New("scion list: not a list of agents")

func parseList(out []byte) ([]string, error) {
	if len(out) == 0 {
		out = []byte("[]") // json.loads(r.stdout or "[]")
	}
	v, err := jsonx.Parse(out)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrList, err)
	}
	items, ok := v.Array()
	if !ok {
		return nil, ErrList
	}
	names := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, it := range items {
		if _, isObj := it.Object(); !isObj {
			return nil, ErrList
		}
		name, ok := it.Get("name").Str()
		if !ok {
			return nil, ErrList
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names, nil
}

// Look is `scion look <name> --plain -n <lines>`: the agent's screen as
// Scion prints it. A non-zero exit is an error (the caller falls back to
// the agent's log). Nothing here can type into an agent.
func (r Reader) Look(ctx context.Context, dir, name string, lines int) (string, error) {
	if err := CheckName(name); err != nil {
		return "", err
	}
	res, err := r.run(ctx, dir, ReadEnv(r.Env, r.Bin), "look", name, "--plain", "-n", strconv.Itoa(lines))
	if err != nil {
		return "", err
	}
	if err := execx.CheckErr(res); err != nil {
		return "", err
	}
	return res.Out(), nil
}
