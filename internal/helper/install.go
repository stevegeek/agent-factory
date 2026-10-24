package helper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

// versionTimeout bounds the new binary's `version`, inside the verb's 5
// minutes.
const versionTimeout = 30 * time.Second

// install is `install <commit12>` on a helper whose maintainer opted it in
// (~/factory/accept-installs, which nothing here writes): the master's
// bundle arrives as a tar on stdin, what deploy.sh's helper-bundle sends
// (jobproto.InstallFiles, and the InstallDirs trees). Its factory-remote
// must match its BUILD line, BUILD must name commit, and the new binary
// must run and say it is commit before anything here changes. Then it goes
// in as deploy.sh's helper-install does: the old binary kept as .prev, the
// old config as config.toml.prev. The sha256s guard against damage on the
// way, not against the master: an opted-in helper runs the code and config
// the master sends.
func (s *Server) install(ctx context.Context, commit string) error {
	if !jobproto.Commit12OK(commit) {
		return refuse("bad commit: %q (the first 12 hex digits of its id)", commit)
	}
	if !exists(filepath.Join(s.Root, jobproto.AcceptInstalls)) {
		return refuse("this helper takes no installs (touch ~/factory/%s)", jobproto.AcceptInstalls)
	}
	lock, err := fsx.TryLock(filepath.Join(s.Root, ".install.lock"))
	if errors.Is(err, fsx.ErrBusy) {
		return refuse("an install is already running here")
	}
	if err != nil {
		return err
	}
	defer lock.Unlock()

	in := &countingReader{r: io.LimitReader(s.Stdin, jobproto.MaxInstallInput+1)}
	data, rerr := io.ReadAll(in)
	if in.n > jobproto.MaxInstallInput {
		return refuse("install input too large")
	}
	if rerr != nil {
		return rerr
	}
	// under the lock no other install runs: what a killed one left goes
	base := filepath.Join(s.Root, ".install")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return err
	}
	if err := sweep(base); err != nil {
		return err
	}
	work := filepath.Join(base, commit+"-"+strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(work, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(work)
	if err := extract(bytes.NewReader(data), work); err != nil {
		return refuse("bad install bundle: %v", err)
	}
	if err := checkBundle(work); err != nil {
		return refuse("bad install bundle: %v", err)
	}
	if err := s.checkBuild(ctx, work, commit); err != nil {
		return err
	}
	if err := s.put(work); err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.Stdout, "installed %s\n", commit)
	return err
}

// checkBundle walks the unpacked bundle: regular files and directories
// only, each one of InstallFiles, inside an InstallDirs tree, or a
// directory on the way to one; and every InstallFiles there.
func checkBundle(work string) error {
	err := filepath.WalkDir(work, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(work, p)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !d.IsDir() && !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}
		if !inBundle(rel, d.IsDir()) {
			return fmt.Errorf("%s is not part of an install", rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, f := range jobproto.InstallFiles() {
		if fi, err := os.Lstat(filepath.Join(work, f)); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("it has no %s", f)
		}
	}
	return nil
}

func inBundle(rel string, dir bool) bool {
	if slices.Contains(jobproto.InstallFiles(), rel) {
		return !dir
	}
	for _, d := range jobproto.InstallDirs() {
		if rel == d || strings.HasPrefix(rel, d+"/") || dir && strings.HasPrefix(d, rel+"/") {
			return true
		}
	}
	return dir && slices.ContainsFunc(jobproto.InstallFiles(), func(f string) bool { return strings.HasPrefix(f, rel+"/") })
}

// checkBuild is the bundle's own consistency: factory-remote as BUILD
// lists it, BUILD naming commit, and the new binary running and saying it
// is commit. It runs with this process's environment less
// SSH_ORIGINAL_COMMAND, without which serve refuses any argv.
func (s *Server) checkBuild(ctx context.Context, work, commit string) error {
	exe := filepath.Join(work, "bin", "factory-remote")
	build, err := fsx.ReadNoFollow(filepath.Join(work, "bin", "BUILD"), 64<<10)
	if err != nil {
		return refuse("bad install bundle: BUILD: %v", errors.Unwrap(err))
	}
	want, named := "", false
	for line := range strings.Lines(string(build)) {
		f := strings.Fields(line)
		switch {
		case len(f) == 2 && f[1] == "factory-remote":
			want = f[0]
		case len(f) >= 2 && f[0] == "commit":
			named = f[1] == commit
		}
	}
	got, err := sha256File(exe)
	if err != nil {
		return err
	}
	if want == "" || got != want {
		return refuse("bad install bundle: factory-remote does not match its sha256 in BUILD")
	}
	if !named {
		return refuse("bad install bundle: its BUILD names another commit than %s", commit)
	}
	if err := os.Chmod(exe, 0o755); err != nil {
		return err
	}
	env := slices.DeleteFunc(slices.Clone(s.Environ), func(kv string) bool { return strings.HasPrefix(kv, "SSH_ORIGINAL_COMMAND=") })
	res, err := s.Exec.Run(ctx, execx.Cmd{Argv: []string{exe, "version"}, Dir: "/", Env: env, Timeout: versionTimeout, MaxOut: buildLogTail})
	if err != nil {
		return fmt.Errorf("the new factory-remote version: %w: %s", err, tail(res))
	}
	if res.Code != 0 {
		return refuse("the new factory-remote does not run (exit %d): %s", res.Code, tail(res))
	}
	if out := string(res.Stdout); !strings.HasPrefix(out, commit+" ") {
		return refuse("the new factory-remote says it is %q, not %s", strings.TrimSpace(out), commit)
	}
	return nil
}

// put installs the checked bundle in work, as deploy.sh's helper-install.
func (s *Server) put(work string) error {
	bin := filepath.Join(s.Root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	if err := putBinary(filepath.Join(work, "bin", "factory-remote"), filepath.Join(bin, "factory-remote")); err != nil {
		return err
	}
	if err := putFile(filepath.Join(work, "bin", "BUILD"), filepath.Join(bin, "BUILD"), 0o644); err != nil {
		return err
	}
	if err := putFile(filepath.Join(work, "bin", "pi-run"), filepath.Join(bin, "pi-run"), 0o755); err != nil {
		return err
	}
	// a helper runs no dispatcher code: bin/factory is factory-remote
	link := filepath.Join(bin, ".factory.install.tmp")
	os.Remove(link)
	if err := os.Symlink("factory-remote", link); err != nil {
		return err
	}
	if err := os.Rename(link, filepath.Join(bin, "factory")); err != nil {
		return err
	}
	cfg := s.configPath()
	if fi, err := os.Lstat(cfg); err == nil && fi.Mode().IsRegular() {
		if err := putFile(cfg, cfg+".prev", fi.Mode().Perm()); err != nil {
			return err
		}
	}
	if err := putFile(filepath.Join(work, "config.toml"), cfg, 0o644); err != nil {
		return err
	}
	// the trees go over what is there: what the bundle lacks (sciontool) stays
	for _, d := range jobproto.InstallDirs() {
		if err := putTree(filepath.Join(work, d), filepath.Join(s.Root, d)); err != nil {
			return err
		}
	}
	return nil
}

// putBinary renames src over dest, so a running binary keeps its old
// inode; a dest that differs is kept first as dest.prev, a hard link.
func putBinary(src, dest string) error {
	if fi, err := os.Lstat(dest); err == nil && fi.Mode().IsRegular() {
		a, err := sha256File(src)
		if err != nil {
			return err
		}
		b, err := sha256File(dest)
		if err != nil {
			return err
		}
		if a != b {
			// a reinstall of the same build must not overwrite .prev with itself
			os.Remove(dest + ".prev")
			if err := os.Link(dest, dest+".prev"); err != nil {
				return err
			}
		}
	}
	if err := os.Chmod(src, 0o755); err != nil {
		return err
	}
	return os.Rename(src, dest)
}

// putFile writes src's bytes to dest through a temp file and a rename.
func putFile(src, dest string, perm fs.FileMode) error {
	b, err := fsx.ReadNoFollow(src, jobproto.MaxInstallInput)
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomic(dest, b, perm); err != nil {
		return err
	}
	return os.Chmod(dest, perm)
}

// putTree copies the tree src over dest, file by file; a src that is not
// there copies nothing.
func putTree(src, dest string) error {
	if !exists(src) {
		return nil
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		to := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		return putFile(p, to, fi.Mode().Perm()&0o755)
	})
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
