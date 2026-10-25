// Package gocache keeps one trusted, compile-only Go build cache per
// project on this host, and lends it to agent and gate containers as a
// podman overlay.
//
// A project's caches live in {root}/cache/go/<project>/: one generation
// per base head, <commit12>/, and current, a link to the newest that
// warmed. A generation is written only by a warm: a container with no
// credential and no network that compiles the base branch's head (merged,
// approved code) without running any of it, into a scratch directory
// seeded from current. Only a warm that succeeded becomes a generation, by
// a rename, and current moves to it by a rename; generations other than
// current and the one before are removed, but never one a run still has
// mounted: each run that is lent a generation holds a shared lock on its
// lease file, .lease-<commit12>, until its container is gone, and a
// generation is removed only under that file's exclusive lock.
//
// Every other container gets the generation current names mounted with
// podman's ":O", so its writes go to a throwaway upper layer: nothing an
// agent or a test does reaches the generation or another run. Go needs a
// writable GOCACHE (a read-only one fails builds now and then), which the
// overlay gives it.
//
// Warming is serialised per project on this host by a lock that is never
// waited on: a run that cannot have it, or whose warm fails, gets the
// current generation as it is (warmed from an earlier base head, as
// trusted), or runs cold with none. Nothing here is an error to the run.
package gocache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

// Target is where a container sees the cache, InHome the same path below
// the agent's home, /home/scion; Env points GOCACHE there.
const (
	Target = "/home/scion/" + InHome
	InHome = ".cache/go-build"
)

// Compile is the compile-only command every warm runs, from the root of a
// checkout of the base head: vet, and every package and test binary
// built, with /bin/true in place of each test binary, so nothing the
// project wrote runs. The build writes no binary: with a single main
// package, go build ./... would write one into the source, which the gate
// mounts read-only.
const Compile = "go vet ./... && go test -count=1 -exec /bin/true ./... && go build -o /dev/null ./..."

const (
	currentName = "current"
	lockName    = ".lock"
	warmPrefix  = ".warm-"
	nextLink    = ".current-next"
	leasePrefix = ".lease-"
)

// BundleScript is a warm's script when its working directory holds a
// job's repo.bundle: a clone into dir, where the job's own checkout will be
// (go keys a package's entries by its directory), with the base head
// checked out detached and checked to be that commit, then Compile there.
func BundleScript(base, dir string) (string, error) {
	if !commitID(base) {
		return "", fmt.Errorf("gocache: bad base %q", base)
	}
	if !plainPath(dir) {
		return "", fmt.Errorf("gocache: bad checkout dir %q", dir)
	}
	return "set -e; git -c init.defaultBranch=main clone -q --no-checkout repo.bundle " + dir + "; cd " + dir + "; " +
		"git -c advice.detachedHead=false checkout -q --detach " + base + "; " +
		`test "$(git rev-parse HEAD)" = ` + base + "; " + Compile, nil
}

// WarmEnv keeps the Go settings of env ("K=V" entries whose name starts
// GO or CGO_, but GOCACHE), which a warm runs with so its entries are the
// ones its consumers look up; nothing else, so no credential, reaches it.
func WarmEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "GOCACHE" || !(strings.HasPrefix(k, "GO") && len(k) > 2 || strings.HasPrefix(k, "CGO_")) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Env is the container environment that makes go use Target.
func Env() string { return "GOCACHE=" + Target }

// Mount is the podman -v value that lends gen to a container as an
// overlay at Target.
func Mount(gen string) string { return gen + ":" + Target + ":O" }

// Dir is one project's caches on this host.
type Dir struct {
	path string
}

// Root is where every project's caches live under the factory root:
// {root}/cache/go.
func Root(root string) string { return filepath.Join(root, "cache", "go") }

// Reaches says a podman -v value's source is root's caches directory
// (Root), inside it, or a directory that holds it, as written or with its
// links resolved: a mount no container but a warm may have.
func Reaches(root, mount string) bool { return ReachesDir(root, Root(root), mount) }

// ReachesDir is Reaches for dir, a directory under root that no container
// may mount either (the gate's kept early checks): a mount source that is
// dir, inside it or holds it, as written or with root's links and its own
// resolved, whether or not dir exists yet.
func ReachesDir(root, dir, mount string) bool {
	src, _, _ := strings.Cut(mount, ":")
	if !strings.HasPrefix(src, "/") {
		return false // a named volume
	}
	protected := []string{filepath.Clean(dir)}
	if rel, err := filepath.Rel(root, dir); err == nil && filepath.IsLocal(rel) {
		if r, err := filepath.EvalSymlinks(root); err == nil {
			protected = append(protected, filepath.Join(r, rel))
		}
	}
	srcs := []string{filepath.Clean(src), resolved(src)}
	for _, c := range protected {
		for _, p := range srcs {
			if p == c || p == "/" || strings.HasPrefix(p, c+"/") || strings.HasPrefix(c, p+"/") {
				return true
			}
		}
	}
	return false
}

// resolved is p with its links resolved, as far as it exists: the part
// that does not is kept as written.
func resolved(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	return filepath.Join(resolved(parent), filepath.Base(p))
}

// For is the project's caches under root: {root}/cache/go/<project>. The
// project must be one safe path element (jobproto.ValidProject).
func For(root, project string) (Dir, error) {
	if root == "" {
		return Dir{}, errors.New("gocache: no root")
	}
	if !jobproto.ValidProject(project) {
		return Dir{}, fmt.Errorf("gocache: bad project %q", project)
	}
	return Dir{path: filepath.Join(Root(root), project)}, nil
}

// Path is the project's cache directory.
func (d Dir) Path() string { return d.path }

// Current is the generation current names, as its real path: false when
// there is none, or current is anything but a link to a 12-hex name of a
// real directory beside it.
func (d Dir) Current() (string, bool) {
	if !d.real() {
		return "", false
	}
	name, err := os.Readlink(filepath.Join(d.path, currentName))
	if err != nil || !commit12(name) {
		return "", false
	}
	gen := filepath.Join(d.path, name)
	fi, err := os.Lstat(gen)
	if err != nil || !fi.IsDir() {
		return "", false
	}
	return gen, true
}

// real says the project's directory is a directory, not a link.
func (d Dir) real() bool {
	fi, err := os.Lstat(d.path)
	return err == nil && fi.IsDir()
}

// Lend is what a run gets: Gen, the generation to mount ("" to run cold),
// whether this run warmed it, and a line for the run's log. A Lend with a
// Gen holds that generation's lease: the run calls Release once its
// container is gone.
type Lend struct {
	Gen    string
	Warmed bool
	Note   string
	lease  *fsx.Locked
}

// Release gives up the lease, after which the generation may be pruned. A
// Lend with none, or released already, does nothing.
func (l *Lend) Release() {
	if l.lease != nil {
		l.lease.Unlock()
		l.lease = nil
	}
}

// lent is gen lent under its lease, or none, running cold, when the lease
// cannot be had or gen is gone: a prune is removing it.
func (d Dir) lent(gen string, warmed bool, note string) Lend {
	lease, err := fsx.TryShare(d.leasePath(filepath.Base(gen)))
	if err == nil {
		if fi, serr := os.Lstat(gen); serr != nil || !fi.IsDir() {
			lease.Unlock()
			err = errors.New("gone")
		}
	}
	if err != nil {
		return Lend{Note: note + "; but it is being removed, so running cold"}
	}
	return Lend{Gen: gen, Warmed: warmed, Note: note, lease: lease}
}

func (d Dir) leasePath(name string) string { return filepath.Join(d.path, leasePrefix+name) }

// Ensure makes sure the generation for base (a full commit id) is current
// and lends it. With none yet, it takes the lock without waiting, seeds a
// scratch directory from current, has compile fill it (compile gets only
// that directory, to mount writable), and makes it the generation; a base
// whose generation is still on disk becomes current without a compile.
// Whatever fails, the run gets what was current before, or nothing.
func (d Dir) Ensure(ctx context.Context, base string, compile func(ctx context.Context, gen string) error) Lend {
	cold := d.AsItIs
	if !commitID(base) {
		return cold(fmt.Sprintf("no base head (%q) to warm from", base))
	}
	name := base[:12]
	if gen, ok := d.Current(); ok && filepath.Base(gen) == name {
		return d.lent(gen, false, "build cache warm for "+name)
	}
	if err := d.mkdirs(); err != nil {
		return cold(err.Error())
	}
	lock, err := fsx.TryLock(filepath.Join(d.path, lockName))
	if err != nil {
		if errors.Is(err, fsx.ErrBusy) {
			return cold("another run is warming the build cache")
		}
		return cold(err.Error())
	}
	defer lock.Unlock()
	if gen, ok := d.Current(); ok && filepath.Base(gen) == name {
		return d.lent(gen, false, "build cache warm for "+name)
	}
	prev, _ := d.Current()
	gen := filepath.Join(d.path, name)
	if fi, err := os.Lstat(gen); err == nil && fi.IsDir() {
		if err := d.switchTo(name); err != nil {
			return cold(err.Error())
		}
		d.prune(name, prev)
		return d.lent(gen, false, "build cache for "+name+" made current again")
	}
	scratch := filepath.Join(d.path, warmPrefix+name)
	if err := fsx.RemoveTree(scratch); err != nil {
		return cold(err.Error())
	}
	if err := os.Mkdir(scratch, 0o700); err != nil {
		return cold(fmt.Sprintf("warm: %v", err))
	}
	if prev != "" {
		if err := copyTree(prev, scratch); err != nil {
			fsx.RemoveTree(scratch)
			return cold(fmt.Sprintf("warm: seeding from %s: %v", filepath.Base(prev), err))
		}
	}
	if err := compile(ctx, scratch); err != nil {
		fsx.RemoveTree(scratch)
		return cold(fmt.Sprintf("warming the build cache for %s failed: %v", name, err))
	}
	if err := os.Rename(scratch, gen); err != nil {
		fsx.RemoveTree(scratch)
		return cold(fmt.Sprintf("warm: %v", err))
	}
	if err := d.switchTo(name); err != nil {
		return cold(err.Error())
	}
	d.prune(name, prev)
	return d.lent(gen, true, "build cache warmed for "+name)
}

// AsItIs lends the current generation without a warm, or none, running
// cold; why, for the run's log, says why nothing was warmed.
func (d Dir) AsItIs(why string) Lend {
	if gen, ok := d.Current(); ok {
		return d.lent(gen, false, why+"; using the cache as it was")
	}
	return Lend{Note: why + "; running cold"}
}

// mkdirs makes {root}/cache/go and the project's directory, 0700, and
// refuses a project directory that is a link.
func (d Dir) mkdirs() error {
	for _, p := range []string{filepath.Dir(d.path), d.path} {
		if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			if errors.Is(err, fs.ErrNotExist) {
				err = os.MkdirAll(p, 0o700)
			}
			if err != nil {
				return fmt.Errorf("gocache: %w", err)
			}
		}
	}
	if !d.real() {
		return fmt.Errorf("gocache: %s is not a directory", d.path)
	}
	return nil
}

// switchTo points current at name by renaming a new link over it.
func (d Dir) switchTo(name string) error {
	next := filepath.Join(d.path, nextLink)
	os.Remove(next)
	if err := os.Symlink(name, next); err != nil {
		return fmt.Errorf("gocache: %w", err)
	}
	if err := os.Rename(next, filepath.Join(d.path, currentName)); err != nil {
		os.Remove(next)
		return fmt.Errorf("gocache: %w", err)
	}
	return nil
}

// prune removes every generation and scratch directory but name and the
// one current named before (prev, "" for none), and leases left with no
// generation. A generation goes, with its lease file, only under that
// file's exclusive lock: one a run has leased stays for a later prune.
func (d Dir) prune(name, prev string) {
	es, err := os.ReadDir(d.path)
	if err != nil {
		return
	}
	keep := func(n string) bool { return n == name || prev != "" && n == filepath.Base(prev) }
	for _, e := range es {
		n := e.Name()
		switch {
		case n == currentName || n == lockName || keep(n):
		case strings.HasPrefix(n, leasePrefix):
			if g := strings.TrimPrefix(n, leasePrefix); !keep(g) && !onDisk(filepath.Join(d.path, g)) {
				d.removeLeased(g)
			}
		case commit12(n):
			d.removeLeased(n)
		default:
			fsx.RemoveTree(filepath.Join(d.path, n))
		}
	}
}

// removeLeased removes the generation name and its lease file, unless a
// run holds the lease.
func (d Dir) removeLeased(name string) {
	lease, err := fsx.TryLock(d.leasePath(name))
	if err != nil {
		return
	}
	defer lease.Unlock()
	if fsx.RemoveTree(filepath.Join(d.path, name)) == nil {
		os.Remove(lease.Path())
	}
}

func onDisk(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// copyTree copies the directories and regular files under src into dst
// with their modes and times (go trims entries by their times); anything
// else is left out. Nothing is opened through a link.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || rel == "." {
			return err
		}
		fi, err := e.Info()
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		switch {
		case fi.IsDir():
			if err := os.Mkdir(out, fi.Mode().Perm()|0o700); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			if err := copyFile(p, out, fi); err != nil {
				return err
			}
		default:
			return nil
		}
		return os.Chtimes(out, fi.ModTime(), fi.ModTime())
	})
}

func copyFile(src, dst string, fi fs.FileInfo) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, fi.Mode().Perm()|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// plainPath is an absolute path of letters, digits, "/", ".", "_" and "-",
// which a shell reads as one word.
func plainPath(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	for _, c := range p {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("/._-", c)) {
			return false
		}
	}
	return true
}

// commitID is a full lowercase SHA-1 or SHA-256 commit id.
func commitID(s string) bool { return (len(s) == 40 || len(s) == 64) && lowerHex(s) }

func commit12(s string) bool { return len(s) == 12 && lowerHex(s) }

func lowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
