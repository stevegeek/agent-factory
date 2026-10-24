package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/gitread"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// Bare is git on a memory store's bare repository (Python mgit): objects
// and refs only, with no user or system config, no hooks and no fsmonitor,
// as the factory's own identity, with the repository as the working
// directory.
type Bare struct {
	GitDir string
	Exec   execx.Execer
	Env    []string // the base environment; nil is this process's
}

// BareCmd is one git call on a Bare store.
type BareCmd struct {
	Args  []string
	Stdin []byte
	Env   []string // extra variables (GIT_INDEX_FILE=…), after BareEnv's
	OK    []int    // the exit codes that are not errors; nil means {0}
}

// BareEnv is mgit's environment: base without any GIT_* variable, then
// GIT_CONFIG_GLOBAL=/dev/null and GIT_CONFIG_NOSYSTEM=1, then extra
// (replacing a variable of the same name).
func BareEnv(base, extra []string) []string {
	var out []string
	for _, kv := range base {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return setVars(out, append([]string{"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1"}, extra...)...)
}

// Run runs git --git-dir=<GitDir> with mgit's -c options and c.Args. An
// exit code outside c.OK is an error "memory git <verb>: <stderr>", as in
// Python; the Result is returned either way.
func (b Bare) Run(ctx context.Context, c BareCmd) (execx.Result, error) {
	if len(c.Args) == 0 {
		return execx.Result{}, errors.New("memory git: no arguments")
	}
	argv := append([]string{"git", "--git-dir=" + b.GitDir,
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "user.name=factory", "-c", "user.email=factory@localhost", "-c", "commit.gpgsign=false"}, c.Args...)
	base := b.Env
	if base == nil {
		base = os.Environ()
	}
	cmd := execx.Cmd{Argv: argv, Dir: b.GitDir, Env: BareEnv(base, c.Env), Timeout: gitread.DefaultTimeout}
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	res, err := b.Exec.Run(ctx, cmd)
	if err != nil {
		return res, fmt.Errorf("memory git %s: %w", c.Args[0], err)
	}
	ok := c.OK
	if ok == nil {
		ok = []int{0}
	}
	if !slices.Contains(ok, res.Code) {
		return res, fmt.Errorf("memory git %s: %s", c.Args[0], lastRunes(textx.Strip(decode(res.Stderr)), 300))
	}
	return res, nil
}

// Out is Run's stdout, decoded and stripped (mgit_out).
func (b Bare) Out(ctx context.Context, c BareCmd) (string, error) {
	res, err := b.Run(ctx, c)
	if err != nil {
		return "", err
	}
	return textx.Strip(decode(res.Stdout)), nil
}

// CatFileBatch reads the given objects with one cat-file --batch, as
// memory_snapshot does, and returns their contents in the same order. A
// missing object is an error.
func (b Bare) CatFileBatch(ctx context.Context, shas []string) ([][]byte, error) {
	if len(shas) == 0 {
		return nil, nil
	}
	var in strings.Builder
	for _, s := range shas {
		if strings.ContainsAny(s, " \n") {
			return nil, fmt.Errorf("memory git cat-file: bad object name %q", s)
		}
		in.WriteString(s + "\n")
	}
	res, err := b.Run(ctx, BareCmd{Args: []string{"cat-file", "--batch"}, Stdin: []byte(in.String())})
	if err != nil {
		return nil, err
	}
	out := res.Stdout
	files := make([][]byte, 0, len(shas))
	i := 0
	for _, sha := range shas {
		end := bytes.IndexByte(out[i:], '\n')
		if end < 0 {
			return nil, fmt.Errorf("memory git cat-file: short output for %s", sha)
		}
		header := strings.Fields(string(out[i : i+end]))
		if len(header) != 3 {
			return nil, fmt.Errorf("memory git cat-file: %s: %q", sha, strings.Join(header, " "))
		}
		size, err := strconv.Atoi(header[2])
		if err != nil || size < 0 || i+end+1+size > len(out) {
			return nil, fmt.Errorf("memory git cat-file: %s: bad size %q", sha, header[2])
		}
		start := i + end + 1
		files = append(files, out[start:start+size])
		i = start + size + 1
	}
	return files, nil
}

// decode is bytes.decode(errors="replace") for git's output.
func decode(b []byte) string { return strings.ToValidUTF8(string(b), "�") }

// lastRunes is s[-n:] in code points.
func lastRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}
