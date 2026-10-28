package deploy

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/gitx"
	"github.com/stevegeek/agent-factory/internal/hostrun"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/paths"
)

// bundleSkip is left out of a helper's bundle: a Claude Code helper keeps
// its own sciontool (23 MB), as deploy.sh's helper-bundle leaves it.
const bundleSkip = "images/scion-agent/sciontool"

// HelperBundle is what an opted-in helper's install verb takes: a tar of
// jobproto.InstallFiles and the InstallDirs trees there are, from the
// root, as deploy.sh's helper-bundle sends. A link anywhere in it is
// refused; so is a bundle past jobproto.MaxInstallInput.
func HelperBundle(p paths.Paths) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(rel string, st fs.FileInfo) error {
		h, err := tar.FileInfoHeader(st, "")
		if err != nil {
			return err
		}
		h.Name, h.Uid, h.Gid, h.Uname, h.Gname = rel, 0, 0, "", ""
		if st.IsDir() {
			h.Name += "/"
			return tw.WriteHeader(h)
		}
		b, err := fsx.ReadNoFollow(filepath.Join(p.Root, rel), jobproto.MaxInstallInput)
		if err != nil {
			return err
		}
		h.Size = int64(len(b))
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	}
	for _, rel := range jobproto.InstallFiles() {
		st, err := os.Lstat(filepath.Join(p.Root, rel))
		if err != nil {
			return nil, fmt.Errorf("helper bundle: %w", err)
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("helper bundle: %s is not a regular file", rel)
		}
		if err := add(rel, st); err != nil {
			return nil, fmt.Errorf("helper bundle: %w", err)
		}
	}
	for _, top := range jobproto.InstallDirs() {
		dir := filepath.Join(p.Root, top)
		if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(p.Root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel == bundleSkip {
				return nil
			}
			if !d.IsDir() && !d.Type().IsRegular() {
				return fmt.Errorf("%s is not a regular file", rel)
			}
			st, err := d.Info()
			if err != nil {
				return err
			}
			return add(rel, st)
		})
		if err != nil {
			return nil, fmt.Errorf("helper bundle: %w", err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("helper bundle: %w", err)
	}
	if buf.Len() > jobproto.MaxInstallInput {
		return nil, fmt.Errorf("helper bundle: larger than %d bytes", jobproto.MaxInstallInput)
	}
	return buf.Bytes(), nil
}

// updateHelpers sends each ssh helper the bundle of what the master now
// runs, after the master's check passed. Each outcome is
// "ok" or the error; a failure files nothing (helpers are often off, and
// the next deploy tries again).
func (s Service) updateHelpers(ctx context.Context, cfg *config.Config, commit12 string) map[string]string {
	var hosts []config.HostCfg
	for _, h := range cfg.Hosts {
		if !h.Pull() {
			hosts = append(hosts, h)
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	out := map[string]string{}
	bundle, err := HelperBundle(s.Paths)
	for _, h := range hosts {
		if err == nil && s.SSH == nil {
			err = errors.New("no ssh client")
		}
		if err != nil {
			out[h.Name] = err.Error()
			continue
		}
		if _, derr := s.SSH.Do(ctx, hostrun.HostOf(s.Paths, h), jobproto.VerbInstall, commit12, bundle, 0); derr != nil {
			out[h.Name] = derr.Error()
			s.logf("deploy %s: helper %s: %v", commit12, h.Name, derr)
			continue
		}
		out[h.Name] = "ok"
	}
	return out
}

// rootSteps files a notice when the deploy changed a file only a root
// step installs, or when it cannot tell, because the
// commit it replaced is not in the mirror (a deploy by hand put one off
// main). It never fails the deploy.
func (s Service) rootSteps(ctx context.Context, core gitx.Mirror, from, to string) {
	c := to[:12]
	title := "Deployed " + c + ": root steps wait for you"
	if from == "" || !core.Has(ctx, from) {
		s.notice(ctx, title, "unknown base: check host/ and systemd/. The commit this replaced ("+orString(from, "none in bin/BUILD")+
			") is not on core main, so the deploy cannot list what changed there.")
		return
	}
	changed, err := core.Changed(ctx, from, to, rootPaths()...)
	if err != nil {
		s.notice(ctx, title, "unknown base: check host/ and systemd/ ("+err.Error()+")")
		return
	}
	if len(changed) == 0 {
		return
	}
	s.notice(ctx, title, "Deploying "+c+" over "+from+" changed files that only a root step installs:\n\n- "+
		strings.Join(changed, "\n- ")+"\n\nRun the matching host/ script as root. For systemd/, re-run host/setup.sh from a checkout of "+c+", then its install-units command.")
}
