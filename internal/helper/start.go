package helper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// systemdRunTimeout bounds systemd-run, which only queues the unit.
const systemdRunTimeout = 60 * time.Second

// maxMetaBytes bounds the meta.json read back from the job dir.
const maxMetaBytes = 1 << 20

// start is the start verb: refuse an existing job, a draining
// host, a full host or a full disk; extract the input tar into
// jobs/<job>/work, move meta.json out of the agent's reach and check it
// (a harness it names must be this host's agent);
// then queue the job's unit. Any failure after the job dir exists removes
// it.
func (s *Server) start(ctx context.Context, job string) error {
	d, err := s.jobDir(job)
	if err != nil {
		return err
	}
	if exists(d) {
		return refuse("%s already exists", job)
	}
	if exists(s.Draining) {
		return refuse("host is draining for a reboot")
	}
	cfg, err := s.config()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(s.jobs())
	if err != nil {
		return fmt.Errorf("jobs: %w", err)
	}
	if len(entries) >= cfg.Remote.MaxJobs {
		return refuse("too many jobs on this host")
	}
	free, err := s.freeDisk(s.jobs())
	if err != nil {
		return err
	}
	if free < jobproto.MinFreeBytes {
		return refuse("not enough free disk on this host")
	}
	// the job dir is made exclusively: two starts of one name cannot share it
	if err := os.Mkdir(d, 0o777); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return refuse("%s already exists", job)
		}
		return fmt.Errorf("start %s: %w", job, err)
	}
	meta, err := s.receive(d)
	if err == nil {
		_, err = harnessOn(cfg, meta)
	}
	if err != nil {
		fsx.RemoveTree(d)
		return err
	}
	// a margin over the container's own timeout, so run always gets to write exit
	// (a go job's warm comes out of the job's own time: goCache, jobCmd)
	seconds := meta.TimeoutMinutes*60 + int(jobproto.RuntimeSlack/time.Second)
	res, err := s.Exec.Run(ctx, execx.Cmd{
		Argv: []string{"systemd-run", "--user", "--quiet", "--collect", "--unit=" + unit(job),
			"--property=RuntimeMaxSec=" + strconv.Itoa(seconds), s.Self, jobproto.VerbRun, job},
		Env:     s.env(),
		Timeout: systemdRunTimeout,
		MaxOut:  64 << 10,
	})
	if err == nil && res.Code != 0 {
		err = refuse("systemd-run failed: %s", textx.Strip(string(res.Stderr)))
	}
	if err != nil {
		fsx.RemoveTree(d)
		return err
	}
	_, err = fmt.Fprintf(s.Stdout, "{\"started\": \"%s\"}\n", job)
	return err
}

// receive reads the job's input from stdin into d: work/ (the input),
// home/, and meta.json moved up out of work/ and checked. Stdin is read to
// its end, capped at MaxInputBytes, and an input over the cap is refused
// as such whatever else is wrong with it.
func (s *Server) receive(d string) (jobproto.Meta, error) {
	work := filepath.Join(d, "work")
	for _, sub := range []string{work, filepath.Join(d, "home")} {
		if err := os.Mkdir(sub, 0o777); err != nil {
			return jobproto.Meta{}, fmt.Errorf("start: %w", err)
		}
	}
	in := &countingReader{r: io.LimitReader(s.Stdin, jobproto.MaxInputBytes+1)}
	xerr := extract(in, work)
	if _, err := io.Copy(io.Discard, in); err != nil && xerr == nil {
		xerr = err
	}
	if in.n > jobproto.MaxInputBytes {
		return jobproto.Meta{}, refuse("job input too large")
	}
	if xerr != nil {
		return jobproto.Meta{}, refuse("bad job input: %v", xerr)
	}
	meta, err := moveMeta(d)
	if err != nil {
		return jobproto.Meta{}, refuse("bad job input: %v", err)
	}
	return meta, nil
}

// moveMeta moves work/meta.json to the job dir, where the agent cannot
// reach it, and checks it there.
func moveMeta(d string) (jobproto.Meta, error) {
	src := filepath.Join(d, "work", jobproto.MemberMeta)
	fi, err := os.Lstat(src)
	if err != nil {
		return jobproto.Meta{}, fmt.Errorf("no %s", jobproto.MemberMeta)
	}
	if !fi.Mode().IsRegular() {
		return jobproto.Meta{}, fmt.Errorf("%s is not a file", jobproto.MemberMeta)
	}
	if err := os.Rename(src, filepath.Join(d, jobproto.MemberMeta)); err != nil {
		return jobproto.Meta{}, err
	}
	return loadMeta(d)
}

// loadMeta is load_meta: the job's checked meta.json, with a harness this
// host has.
func loadMeta(d string) (jobproto.Meta, error) {
	b, err := fsx.ReadNoFollow(filepath.Join(d, jobproto.MemberMeta), maxMetaBytes)
	if err != nil {
		return jobproto.Meta{}, err
	}
	m, err := jobproto.ParseMeta(b)
	if err != nil {
		return jobproto.Meta{}, err
	}
	if _, ok := harnessFor(m.Harness); !ok {
		return jobproto.Meta{}, fmt.Errorf("no harness %q on this host", m.Harness)
	}
	if m.Kind == jobproto.KindGate {
		return jobproto.Meta{}, fmt.Errorf("no %s jobs on this host", m.Kind)
	}
	return m, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// extract unpacks the tar on r into dest with fsx.Extract: regular files
// and directories only, every open inside dest.
func extract(r io.Reader, dest string) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer root.Close()
	return fsx.Extract(root, r)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
