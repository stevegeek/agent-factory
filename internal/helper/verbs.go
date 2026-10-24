package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/imagebuild"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/podman"
	"github.com/stevegeek/agent-factory/internal/usage"
	"github.com/stevegeek/agent-factory/internal/vitals"
)

// Bounds on the systemctl and podman calls of status and remove (Python
// set none; the dispatcher's ssh gives up after 120 s).
const (
	isActiveTimeout = 30 * time.Second
	stopTimeout     = 100 * time.Second
	rmTimeout       = 60 * time.Second
)

// status is the status verb: done once the job's exit file exists,
// running while its dir exists and its unit is active, missing otherwise.
func (s *Server) status(ctx context.Context, job string) error {
	d, err := s.jobDir(job)
	if err != nil {
		return err
	}
	state := jobproto.StateMissing
	switch {
	case exists(filepath.Join(d, "exit")):
		state = jobproto.StateDone
	case isDir(d):
		res, err := s.Exec.Run(ctx, execx.Cmd{
			Argv: []string{"systemctl", "--user", "is-active", "--quiet", unit(job)},
			Env:  s.env(), Timeout: isActiveTimeout, MaxOut: 64 << 10,
		})
		if err != nil {
			return fmt.Errorf("status %s: %w", job, err)
		}
		if res.Code == 0 {
			state = jobproto.StateRunning
		}
	}
	_, err = fmt.Fprintf(s.Stdout, "{\"state\": \"%s\"}\n", state)
	return err
}

// collect is the collect verb: a tar on stdout of what the job
// left, each member within its cap (jobproto.CollectCaps). report.json
// comes from home/ (the agent's ~), the rest from work/. A member over its
// cap is left out, except agent.log, which is cut to its tail; a member
// reached through a link anywhere below the job dir, or that is not a
// regular file, is left out.
func (s *Server) collect(job string) error {
	d, err := s.jobDir(job)
	if err != nil {
		return err
	}
	if !isDir(d) {
		return refuse("%s not found", job)
	}
	realD, err := filepath.EvalSymlinks(d)
	if err != nil {
		return fmt.Errorf("collect %s: %w", job, err)
	}
	out := &countingWriter{w: s.Stdout}
	for _, c := range jobproto.CollectCaps() {
		sub := "work"
		if c.Name == jobproto.MemberReport {
			sub = "home"
		}
		data, ok, err := readJobFile(realD, sub, c.Name, c.Max, c.Name == jobproto.MemberLog)
		if err != nil {
			return fmt.Errorf("collect %s: %w", job, err)
		}
		if !ok {
			continue
		}
		pad := make([]byte, (tarBlock-len(data)%tarBlock)%tarBlock)
		for _, b := range [][]byte{ustarHeader(c.Name, int64(len(data))), data, pad} {
			if _, err := out.Write(b); err != nil {
				return fmt.Errorf("collect %s: %w", job, err)
			}
		}
	}
	// the end: two zero blocks, then zeros to a whole record, as tarfile writes
	end := 2*tarBlock + (tarRecord-(out.n+2*tarBlock)%tarRecord)%tarRecord
	if _, err := out.Write(make([]byte, end)); err != nil {
		return fmt.Errorf("collect %s: %w", job, err)
	}
	return nil
}

const (
	tarBlock  = 512
	tarRecord = 20 * tarBlock
)

// ustarHeader is the header Python's tarfile writes for a TarInfo(name)
// with only its size and mode 0644 set: uid, gid and mtime 0, no owner
// names, and (unlike Go's archive/tar) the device fields left as NULs. The
// name is one of the collect members, which always fit the name field.
func ustarHeader(name string, size int64) []byte {
	h := make([]byte, tarBlock)
	octal := func(off, width int, n int64) {
		copy(h[off:], fmt.Sprintf("%0*o", width-1, n))
	}
	copy(h[0:100], name)
	octal(100, 8, 0o644) // mode
	octal(108, 8, 0)     // uid
	octal(116, 8, 0)     // gid
	octal(124, 12, size)
	octal(136, 12, 0) // mtime
	copy(h[148:156], "        ")
	h[156] = '0'
	copy(h[257:265], "ustar\x0000")
	var sum int64
	for _, b := range h {
		sum += int64(b)
	}
	copy(h[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return h
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// tail is the tail verb: the last TailBytes of work/agent.log, or
// nothing when it is missing, not a regular file or reached through a link.
func (s *Server) tail(job string) error {
	d, err := s.jobDir(job)
	if err != nil {
		return err
	}
	realD, err := filepath.EvalSymlinks(d)
	if err != nil {
		return nil // no job, no output
	}
	data, ok, err := readJobFile(realD, "work", jobproto.MemberLog, jobproto.TailBytes, true)
	if err != nil || !ok {
		return err
	}
	_, err = s.Stdout.Write(data)
	return err
}

// readJobFile reads dir/sub/name below a job's real dir, refusing a link
// at either step (Python compares the realpath with the expected path;
// here no link is ever followed, so the job cannot swap one in between
// the check and the read) and anything but a regular file. With tail, a
// file over max gives its last max bytes; without, it is left out. ok is
// false when the file is left out.
func readJobFile(dir, sub, name string, max int64, tail bool) (data []byte, ok bool, err error) {
	const flags = syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	dfd, err := syscall.Open(dir, flags|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, false, nil
	}
	defer syscall.Close(dfd)
	sfd, err := syscall.Openat(dfd, sub, flags|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, false, nil
	}
	defer syscall.Close(sfd)
	fd, err := syscall.Openat(sfd, name, flags|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, nil
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false, nil
	}
	size := fi.Size()
	if size > max && !tail {
		return nil, false, nil
	}
	data = make([]byte, min(size, max))
	n, err := f.ReadAt(data, max64(0, size-max))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, fmt.Errorf("read %s: %w", name, err)
	}
	return data[:n], true, nil
}

func max64(a, b int64) int64 { return max(a, b) }

// remove is the remove verb: stop the unit, remove the container,
// delete the job's files (also directories an agent made unreadable).
// Failures of the stop and the rm are ignored, as in Python. What the
// agent spent is read from the job's home once the container is gone and
// before the home is deleted, and goes back with the answer, {"removed":
// <job>, "usage": <usage.json>}, so a job removed before it was done (its
// timeout, a lost poll) keeps its usage; a home with no transcript, or
// one that cannot be read, sends none.
func (s *Server) remove(ctx context.Context, job string) error {
	d, err := s.jobDir(job)
	if err != nil {
		return err
	}
	s.Exec.Run(ctx, execx.Cmd{Argv: []string{"systemctl", "--user", "stop", unit(job)},
		Env: s.env(), Timeout: stopTimeout, MaxOut: 64 << 10})
	s.Exec.Run(ctx, execx.Cmd{Argv: []string{"podman", "rm", "-f", unit(job)},
		Env: s.env(), Timeout: rmTimeout, MaxOut: 64 << 10})
	spent := ""
	if isDir(d) {
		if u, err := usage.Read(filepath.Join(d, "home")); err == nil && !u.Empty() {
			b, _ := json.Marshal(u) // maps and int64s always encode
			spent = ", \"usage\": " + string(b)
		}
	}
	fsx.RemoveTree(d)
	_, err = fmt.Fprintf(s.Stdout, "{\"removed\": \"%s\"%s}\n", job, spent)
	return err
}

// list is the list verb: every name in the jobs dir, sorted.
func (s *Server) list() error {
	entries, err := os.ReadDir(s.jobs())
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	for _, n := range names {
		if _, err := fmt.Fprintln(s.Stdout, n); err != nil {
			return err
		}
	}
	return nil
}

// vitals is the vitals verb: this host's vital signs as JSON
// (vitals.Collect), with / and the factory root's disks, podman's running
// containers, which podman is given vitals.ContainersTimeout to name, and
// the image versions in this host's images.json, which its rebuild keeps.
// Nothing else (no config, no environment) is in it.
func (s *Server) vitals(ctx context.Context) error {
	pm := podman.Client{Exec: s.Exec, Dir: s.Home, Env: s.env()}
	v := vitals.Collect(ctx, vitals.Deps{Root: s.ProcRoot, Now: s.Clock.Now, DiskPaths: []string{"/", s.Root},
		Statfs: s.Statfs, Containers: func(ctx context.Context) ([]string, error) { return pm.Running(ctx, "") }})
	if state, err := imagebuild.LoadState(filepath.Join(s.Root, "images.json")); err != nil {
		v.Problems = append(v.Problems, err.Error())
	} else {
		for kind, st := range state {
			if v.Images == nil {
				v.Images = map[string]vitals.Image{}
			}
			v.Images[kind] = vitals.Image{Version: st.Version, Since: st.Since, Previous: st.Previous, Held: st.Held}
		}
	}
	b, err := v.Encode()
	if err != nil {
		return err
	}
	_, err = s.Stdout.Write(b)
	return err
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
