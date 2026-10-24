package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/agent-factory/internal/deployrec"
	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/paths"
)

// systemForemanUnit is where host/foreman-system.sh puts Foreman's system
// unit; while it is there, foreman-restart.path restarts Foreman when
// bin/foreman or foreman.env changes.
const systemForemanUnit = "/etc/systemd/system/foreman.service"

// maxSnapshotFile caps what a restore reads of one file.
const maxSnapshotFile = 256 << 20

// snapshotBinaries are hard-linked into the snapshot: an install renames
// a new binary over the old, so the old inode stays as it was.
func snapshotBinaries() []string {
	return []string{"bin/factory", "bin/foreman", "bin/factory-remote"}
}

// snapshotFiles are copied into the snapshot. The rest of an install
// (context/, rendered units, images) is left new by a restore.
func snapshotFiles() []string {
	return []string{"bin/BUILD", "bin/pi-run", "config.toml", "foreman.env"}
}

func snapshotted() []string { return append(snapshotBinaries(), snapshotFiles()...) }

// snapshotRecord is the file in deploys/before that names the deploy the
// snapshot was taken for: "for <commit12> from <from>".
const snapshotRecord = "SNAPSHOT"

// Snapshot keeps the running files in deploys/before, replacing the last
// snapshot only once the new one is whole: before.new is filled, before
// becomes before.old, before.new becomes before, and before.old goes.
// Files the root lacks are left out. SNAPSHOT records the deploy it is
// for: of commit12, over from (the running build, "-" for none).
func Snapshot(p paths.Paths, commit12, from string) error {
	dir := p.DeployBefore()
	next, old := dir+".new", dir+".old"
	for _, d := range []string{next, old} {
		if err := fsx.RemoveTree(d); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(next, "bin"), 0o700); err != nil {
		return err
	}
	for _, rel := range snapshotBinaries() {
		src := filepath.Join(p.Root, rel)
		if !regular(src) {
			continue
		}
		if err := os.Link(src, filepath.Join(next, rel)); err != nil {
			return err
		}
	}
	for _, rel := range snapshotFiles() {
		src := filepath.Join(p.Root, rel)
		if !regular(src) {
			continue
		}
		if err := copyFile(src, filepath.Join(next, rel)); err != nil {
			return err
		}
	}
	record := "for " + commit12 + " from " + orString(from, "-") + "\n"
	if err := os.WriteFile(filepath.Join(next, snapshotRecord), []byte(record), 0o600); err != nil {
		return err
	}
	if err := os.Rename(dir, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(next, dir); err != nil {
		return err
	}
	if err := fsx.RemoveTree(old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// changedSinceSnapshot lists the snapshot's files whose bytes now differ
// from it (or are gone).
func changedSinceSnapshot(p paths.Paths) []string {
	var out []string
	for _, rel := range snapshotted() {
		want, err := os.ReadFile(filepath.Join(p.DeployBefore(), rel))
		if err != nil {
			continue
		}
		have, err := os.ReadFile(filepath.Join(p.Root, rel))
		if err != nil || !bytes.Equal(have, want) {
			out = append(out, rel)
		}
	}
	return out
}

// Restore puts back each snapshot file whose bytes differ from the
// running one, by a temp file beside it renamed over it (safe under
// running processes), and returns what it put back. It waits for the tick
// lock (at most TickLockWait, then goes on: a restore must not wait on a
// stuck tick) and checks the snapshot's binaries against the snapshot's
// BUILD before it changes anything. When Foreman is a user unit (not
// host/foreman-system.sh's system unit, which its path unit restarts) and
// enabled, it restarts it.
func (s Service) Restore(ctx context.Context) ([]string, error) {
	p := s.Paths
	before := p.DeployBefore()
	if _, err := os.Stat(before); err != nil {
		return nil, fmt.Errorf("no snapshot to restore: %w", err)
	}
	if err := checkSnapshot(before); err != nil {
		return nil, err
	}
	wait := s.TickLockWait
	if wait == 0 {
		wait = TickLockWait
	}
	tick, err := fsx.Lock(ctx, p.TickLock, fsx.Wait(wait))
	switch {
	case err == nil:
		defer tick.Unlock()
	case errors.Is(err, fsx.ErrBusy):
		s.logf("deploy restore: the tick held its lock for %v: restoring anyway", wait)
	default:
		if ctx.Err() != nil {
			return nil, err
		}
		s.logf("deploy restore: the tick lock: %v: restoring anyway", err)
	}
	var restored []string
	for _, rel := range changedSinceSnapshot(p) {
		if err := putBack(filepath.Join(before, rel), filepath.Join(p.Root, rel)); err != nil {
			return restored, fmt.Errorf("restore %s: %w", rel, err)
		}
		restored = append(restored, rel)
	}
	if len(restored) > 0 {
		s.logf("deploy restore: put back %s", strings.Join(restored, ", "))
		s.restartUserForeman(ctx)
	}
	return restored, nil
}

// RestoreByHand is `factory deploy restore`: Restore under the deploy
// lock, refused while a runner holds it or a desktop deploy may be running
// (a restore then would mix its install with the snapshot, and a Snapshot
// may be replacing before/). A runner's own Restore holds the lock
// already. Unless forced, it is refused too when the snapshot is not of
// the last deploy (snapshotOf).
func (s Service) RestoreByHand(ctx context.Context, force bool) ([]string, error) {
	if err := s.desktopDeploy(); err != nil {
		return nil, err
	}
	lock, err := fsx.TryLock(s.Paths.DeployLock())
	switch {
	case errors.Is(err, fsx.ErrBusy):
		return nil, errors.New("a deploy is running (factory deploy status)")
	case errors.Is(err, fs.ErrNotExist):
		// no deploys/: Restore says there is no snapshot
	case err != nil:
		return nil, err
	default:
		defer lock.Unlock()
	}
	if !force {
		if err := s.snapshotOf(); err != nil {
			return nil, fmt.Errorf("%w: nothing restored (factory deploy restore --force puts the snapshot back anyway)", err)
		}
	}
	return s.Restore(ctx)
}

// snapshotOf refuses a snapshot that would not undo the last deploy: one
// taken for another deploy (the last one stopped before its own snapshot),
// or one under a build that no longer runs, when the deploy ended with
// that build put back or another installed since (a desktop deploy). An
// interrupted or rollback_failed deploy may have installed only part of
// its build, so bin/BUILD is not asked then. With no snapshot at all,
// Restore says so.
func (s Service) snapshotOf() error {
	before := s.Paths.DeployBefore()
	if _, err := os.Stat(before); err != nil {
		return nil
	}
	rec, err := os.ReadFile(filepath.Join(before, snapshotRecord))
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("the snapshot records no deploy")
	}
	if err != nil {
		return err
	}
	var forCommit, from string
	if f := strings.Fields(string(rec)); len(f) == 4 && f[0] == "for" && f[2] == "from" {
		forCommit, from = f[1], f[3]
	} else {
		return fmt.Errorf("the snapshot's record %q does not read", strings.TrimSpace(string(rec)))
	}
	st, err := deployrec.ReadState(s.Paths.DeployState())
	if err != nil {
		return err
	}
	if st.Commit != forCommit {
		return fmt.Errorf("the snapshot is of the deploy of %s (from %s), the last deploy is of %s", forCommit, from, orString(st.Commit, orString(st.Rev, "nothing")))
	}
	if !st.Snapshotted {
		return fmt.Errorf("the last deploy of %s stopped before its snapshot: the snapshot is of an earlier deploy", forCommit)
	}
	if st.Result == deployrec.ResultInterrupted || st.Result == deployrec.ResultRollbackFailed {
		return nil
	}
	if runs := buildCommit(s.Paths); runs != forCommit {
		return fmt.Errorf("bin/BUILD names %s, not %s, and the deploy of %s ended %s: the snapshot would not undo what runs", orString(runs, "no commit"), forCommit, forCommit, orString(st.Result, st.Phase))
	}
	return nil
}

// checkSnapshot refuses a snapshot whose binaries do not match its BUILD.
func checkSnapshot(before string) error {
	build, err := os.ReadFile(filepath.Join(before, "bin", "BUILD"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, rel := range snapshotBinaries() {
		b, err := os.ReadFile(filepath.Join(before, rel))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		want := buildSha(build, filepath.Base(rel))
		sum := sha256.Sum256(b)
		if want == "" || hex.EncodeToString(sum[:]) != want {
			return fmt.Errorf("the snapshot's %s does not match its BUILD: nothing restored", rel)
		}
	}
	return nil
}

// buildSha is the sha256 BUILD lists for name ("<sha256> <name>").
func buildSha(build []byte, name string) string {
	for _, line := range strings.Split(string(build), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[1] == name {
			return f[0]
		}
	}
	return ""
}

func (s Service) restartUserForeman(ctx context.Context) {
	unit := orString(s.SystemUnit, systemForemanUnit)
	if _, err := os.Stat(unit); err == nil {
		return // foreman-restart.path restarts the system unit
	}
	res, err := s.exec().Run(ctx, execx.Cmd{Argv: []string{"systemctl", "--user", "-q", "is-enabled", "foreman"}, Env: s.environ(), Timeout: SystemdRunTimeout})
	if err != nil || res.Code != 0 {
		return
	}
	res, err = s.exec().Run(ctx, execx.Cmd{Argv: []string{"systemctl", "--user", "restart", "foreman"}, Env: s.environ(), Timeout: SystemdRunTimeout})
	if err == nil {
		err = execx.CheckErr(res)
	}
	if err != nil {
		s.logf("deploy restore: foreman did not restart: %v", err)
	}
}

// putBack copies src over dst through a temp file beside dst, with src's
// mode.
func putBack(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	b, err := fsx.ReadNoFollow(src, maxSnapshotFile)
	if err != nil {
		return err
	}
	return fsx.WriteAtomicSync(dst, b, st.Mode().Perm())
}

func copyFile(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	b, err := fsx.ReadNoFollow(src, maxSnapshotFile)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, st.Mode().Perm())
}

// regular is a regular file, not a link.
func regular(path string) bool {
	st, err := os.Lstat(path)
	return err == nil && st.Mode().IsRegular()
}
