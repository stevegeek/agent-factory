package gitread

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// repoConfigOK is REPO_CONFIG_OK (re.match, $ without re.M): the keys a
// plain clone's .git/config has. It is applied to the lowercased key.
//
// py: ^(core\.(repositoryformatversion|filemode|bare|logallrefupdates)|remote\.origin\.fetch|remote\.origin\.url|branch\..+\.(remote|merge)|extensions\.relativeworktrees)$
var repoConfigOK = regexp.MustCompile(`^(core\.(repositoryformatversion|filemode|bare|logallrefupdates)|remote\.origin\.fetch|remote\.origin\.url|branch\..+\.(remote|merge)|extensions\.relativeworktrees)\z`)

// matchConfigKey is REPO_CONFIG_OK.match(s), as submatch offsets or nil.
func matchConfigKey(s string) []int { return textx.AtEnd(repoConfigOK, s) }

// NotPlain is the removal reason recorded for a .git/config that is a
// symlink or not a regular file: everything in it is dropped.
const NotPlain = ".git/config was not a plain file"

// KeepEntry is guard_repo's rule for one config entry: the key is one a
// plain clone has, and when the project has a GitHub repository the origin
// URL is exactly https://github.com/<github>.git.
func KeepEntry(key, value, github string) bool {
	k := textx.Lower(key)
	if matchConfigKey(k) == nil {
		return false
	}
	return !(k == "remote.origin.url" && github != "" && value != "https://github.com/"+github+".git")
}

// StatKey is the (st_ino, st_mtime_ns, st_size) of a .git/config: the
// guard's cache key.
type StatKey struct {
	Ino     uint64
	MtimeNs int64
	Size    int64
}

// ConfigStat is what lstat says about a repository's .git/config.
type ConfigStat struct {
	Path    string // <repo>/.git/config
	Present bool   // there is a .git directory with a config entry in it
	Plain   bool   // the config is a regular file (not a symlink, fifo, …)
	Link    bool   // the config is a symlink
	Key     StatKey
}

// StatConfig lstats <path>/.git/config. A .git that exists but is not a
// directory (a gitdir file, a symlink) is an error, where
// Python skipped the check. A missing .git or config is not present.
func StatConfig(path string) (ConfigStat, error) {
	gitdir := filepath.Join(path, ".git")
	st := ConfigStat{Path: filepath.Join(gitdir, "config")}
	gi, err := os.Lstat(gitdir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st, nil
	case err != nil:
		return st, fmt.Errorf("guard %s: %w", path, err)
	case gi.Mode()&fs.ModeSymlink != 0:
		return st, fmt.Errorf("%s: unexpected .git symlink: the factory only runs git in a plain clone", path)
	case gi.Mode().IsRegular():
		return st, fmt.Errorf("%s: unexpected .git file: the factory only runs git in a plain clone", path)
	case !gi.IsDir():
		return st, fmt.Errorf("%s: unexpected .git (%v): the factory only runs git in a plain clone", path, gi.Mode().Type())
	}
	ci, err := os.Lstat(st.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st, nil
	case err != nil:
		return st, fmt.Errorf("guard %s: %w", path, err)
	}
	st.Present = true
	st.Link = ci.Mode()&fs.ModeSymlink != 0
	st.Plain = ci.Mode().IsRegular()
	if sys, ok := ci.Sys().(*syscall.Stat_t); ok {
		st.Key = StatKey{Ino: sys.Ino, MtimeNs: sys.Mtim.Sec*1e9 + sys.Mtim.Nsec, Size: sys.Size}
	}
	return st, nil
}

// Entry is one key and value of a git config listing.
type Entry struct{ Key, Value string }

// Inspection is guard_repo's reading of a .git/config, before any change.
type Inspection struct {
	ConfigStat
	Kept    []Entry  // the entries a plain clone has, in file order
	Removed []string // the keys (or NotPlain) a guard drops, in file order
}

// Inspect reads <path>/.git/config the way guard_repo does, changing
// nothing: git config --file <cfg> --no-includes --list -z with SafeEnv
// from /, then KeepEntry on each entry. A config that is not a plain file
// is not read: all of it is removed. github is the project's owner/repo,
// or "".
func Inspect(ctx context.Context, ex execx.Execer, env []string, path, github string) (Inspection, error) {
	st, err := StatConfig(path)
	in := Inspection{ConfigStat: st}
	if err != nil || !st.Present {
		return in, err
	}
	if !st.Plain {
		in.Removed = []string{NotPlain}
		return in, nil
	}
	if env == nil {
		env = os.Environ()
	}
	argv := []string{"git", "config", "--file", st.Path, "--no-includes", "--list", "-z"}
	res, err := ex.Run(ctx, execx.Cmd{Argv: argv, Dir: "/", Env: SafeEnv(env), Timeout: DefaultTimeout})
	if err != nil {
		return in, fmt.Errorf("guard %s: %w", path, err)
	}
	// Python ignores the exit code: a config git cannot parse lists nothing
	for _, e := range bytes.Split(res.Stdout, []byte{0}) {
		if len(e) == 0 {
			continue
		}
		k, v, _ := bytes.Cut(e, []byte{'\n'})
		entry := Entry{Key: decodeReplace(k), Value: decodeReplace(v)}
		if KeepEntry(entry.Key, entry.Value, github) {
			in.Kept = append(in.Kept, entry)
		} else {
			in.Removed = append(in.Removed, entry.Key)
		}
	}
	return in, nil
}

// Verifier is a Guard that changes nothing: it refuses a repository whose
// .git/config holds anything a plain clone's does not. It is for processes
// that may not write a repository (Foreman); the dispatcher's guard is
// gitx.Guarder, which cleans the config instead.
type Verifier struct {
	Exec execx.Execer
	Env  []string
}

// Check implements Guard.
func (v Verifier) Check(ctx context.Context, path, github string) error {
	in, err := Inspect(ctx, v.Exec, v.Env, path, github)
	if err != nil {
		return err
	}
	if len(in.Removed) > 0 {
		return &ConfigRefusal{Path: path, Removed: in.Removed}
	}
	return nil
}

// ConfigRefusal is Verifier's refusal: the repository at Path has the
// settings Removed (Inspection.Removed) that a plain clone does not.
type ConfigRefusal struct {
	Path    string
	Removed []string
}

func (e *ConfigRefusal) Error() string {
	return fmt.Sprintf("%s: .git/config has settings a plain clone does not (%s); the factory's next tick removes them",
		e.Path, strings.Join(firstN(e.Removed, 8), ", "))
}

// ByHand turns the Verifier's refusal of a checkout into the git commands
// that clean it, for a command run by hand (factory onboard). Its own
// advice, that the next tick removes the settings, does not hold there: a
// tick cleans a checkout only when it runs git in it, and a project being
// onboarded is new or paused. Any other error is err.
func ByHand(err error, github string) error {
	var r *ConfigRefusal
	if !errors.As(err, &r) {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: .git/config has settings a plain clone does not (%s); onboard changes nothing, "+
		"and a tick cleans it only once it works on the project, so run:", r.Path, strings.Join(r.Removed, ", "))
	git := "git -C " + shQuote(r.Path)
	for _, k := range r.Removed {
		switch {
		case k == NotPlain:
			fmt.Fprintf(&b, "\nreplace %s with a plain file", shQuote(filepath.Join(r.Path, ".git", "config")))
		case textx.Lower(k) == "remote.origin.url" && github != "":
			fmt.Fprintf(&b, "\n%s remote set-url origin %s", git, shQuote("https://github.com/"+github+".git"))
		default:
			fmt.Fprintf(&b, "\n%s config --unset-all %s", git, shQuote(k))
		}
	}
	return errors.New(b.String())
}

// shQuote is s as one sh word: as it is when it needs no quoting, else in
// single quotes.
func shQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._-+:@=,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func firstN(xs []string, n int) []string {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}

// decodeReplace is close to bytes.decode(errors="replace"): each invalid
// byte becomes U+FFFD (Python gives one for a truncated multi-byte
// sequence; a plain clone's config is ASCII, so the two never differ on
// what is kept).
func decodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			sb.WriteRune(utf8.RuneError)
		} else {
			sb.Write(b[:size])
		}
		b = b[size:]
	}
	return sb.String()
}
