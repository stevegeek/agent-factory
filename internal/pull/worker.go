package pull

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"syscall"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/jobproto"
)

// Everything factory-pull does in a worker's directory goes through an
// os.Root at it: the directory is the agent's to write in, so a link it
// plants (jobs/ to ~/.ssh, report.json to a key) cannot take a read or a
// write outside it. Opens are non-blocking, so a fifo cannot hang the loop,
// and only regular files are read.

// The files a worker leaves in its job directory.
const (
	progressLog = "progress.log" // what it has done so far, for the beats
	memoryDir   = "memory"
	maxMetaSize = 1 << 20
)

// jobsRoot opens the worker's jobs/ (made when missing), through the
// worker's directory.
func (p *Puller) jobsRoot(worker string) (*os.Root, error) {
	w, err := os.OpenRoot(p.Cfg.WorkerDir(worker))
	if err != nil {
		return nil, err
	}
	defer w.Close()
	if err := w.Mkdir("jobs", 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return w.OpenRoot("jobs")
}

// busy reports a worker with a job in its jobs/ that has no report.json
// yet (one factory-pull holds, or one it does not know of: either way the
// worker is not idle), or that cannot be read.
func (p *Puller) busy(worker string) bool {
	jr, err := p.jobsRoot(worker)
	if err != nil {
		p.logf("worker %s: %v", worker, err)
		return true
	}
	defer jr.Close()
	entries, err := fs.ReadDir(jr.FS(), ".")
	if err != nil {
		return true
	}
	for _, e := range entries {
		if !e.IsDir() || !jobproto.JobNameOK(e.Name()) {
			continue
		}
		if fi, err := jr.Lstat(path.Join(e.Name(), jobproto.MemberReport)); err != nil || !fi.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// install unpacks a claimed job into the worker's jobs/<id>: into a fresh
// jobs/.incoming-<id> with fsx.Extract, then renamed into place, so the
// worker never sees half a job. It checks meta.json and prompt.md, writes
// job.sh, and returns the meta and whether the job carries memory. A gate
// job has checks.sh and repo.bundle instead of a prompt, and gets gate.sh.
func (p *Puller) install(worker, id string, start []byte) (jobproto.Meta, bool, error) {
	jr, err := p.jobsRoot(worker)
	if err != nil {
		return jobproto.Meta{}, false, err
	}
	defer jr.Close()
	tmp := ".incoming-" + id
	for _, old := range []string{tmp, id} {
		if err := jr.RemoveAll(old); err != nil {
			return jobproto.Meta{}, false, err
		}
	}
	if err := jr.Mkdir(tmp, 0o755); err != nil {
		return jobproto.Meta{}, false, err
	}
	meta, memory, err := p.unpack(jr, tmp, id, start)
	if err == nil {
		err = jr.Rename(tmp, id)
	}
	if err != nil {
		jr.RemoveAll(tmp)
		return jobproto.Meta{}, false, err
	}
	return meta, memory, nil
}

func (p *Puller) unpack(jr *os.Root, tmp, id string, start []byte) (jobproto.Meta, bool, error) {
	tr, err := jr.OpenRoot(tmp)
	if err != nil {
		return jobproto.Meta{}, false, err
	}
	defer tr.Close()
	if err := fsx.Extract(tr, bytes.NewReader(start)); err != nil {
		return jobproto.Meta{}, false, fmt.Errorf("bad job input: %w", err)
	}
	raw, ok, err := readIn(tr, jobproto.MemberMeta, maxMetaSize, false)
	if err != nil || !ok {
		return jobproto.Meta{}, false, fmt.Errorf("bad job input: no %s", jobproto.MemberMeta)
	}
	meta, err := jobproto.ParseMeta(raw)
	if err != nil {
		return jobproto.Meta{}, false, fmt.Errorf("bad job input: %w", err)
	}
	if meta.Kind == jobproto.KindGate {
		for _, name := range []string{jobproto.MemberChecks, jobproto.MemberBundle} {
			if fi, err := tr.Lstat(name); err != nil || !fi.Mode().IsRegular() {
				return jobproto.Meta{}, false, fmt.Errorf("bad job input: no %s", name)
			}
		}
		if err := tr.WriteFile(GateScriptName, gateScript(id, meta), 0o755); err != nil {
			return jobproto.Meta{}, false, err
		}
		return meta, false, nil
	}
	if fi, err := tr.Lstat(jobproto.MemberPrompt); err != nil || !fi.Mode().IsRegular() {
		return jobproto.Meta{}, false, fmt.Errorf("bad job input: no %s", jobproto.MemberPrompt)
	}
	if err := tr.WriteFile(ScriptName, script(id, meta, p.Cfg.GitName, p.Cfg.GitEmail), 0o755); err != nil {
		return jobproto.Meta{}, false, err
	}
	fi, err := tr.Lstat(memoryDir)
	return meta, err == nil && fi.IsDir(), nil
}

// jobRoot opens the worker's jobs/<id>.
func (p *Puller) jobRoot(worker, id string) (*os.Root, error) {
	jr, err := p.jobsRoot(worker)
	if err != nil {
		return nil, err
	}
	defer jr.Close()
	return jr.OpenRoot(id)
}

// reported reports a report.json in the job's directory: the worker writes
// it last.
func (p *Puller) reported(worker, id string) bool {
	r, err := p.jobRoot(worker, id)
	if err != nil {
		return false
	}
	defer r.Close()
	fi, err := r.Lstat(jobproto.MemberReport)
	return err == nil && fi.Mode().IsRegular()
}

// startMarks are what a worker makes in a job's directory once it takes
// the job: the checkout (job.sh prepare), its progress, a gate's output,
// and what it leaves at the end.
func startMarks() []string {
	return []string{"repo", progressLog, "gate.out", jobproto.MemberHead, jobproto.MemberReport}
}

// took reports that the worker took the job: Lever reports it running and
// not idle at its prompt, or it has made one of startMarks.
func (p *Puller) took(j *job) bool {
	if p.view.workers[j.Worker].Busy() {
		return true
	}
	r, err := p.jobRoot(j.Worker, j.ID)
	if err != nil {
		return false
	}
	defer r.Close()
	for _, name := range startMarks() {
		if _, err := r.Lstat(name); err == nil {
			return true
		}
	}
	return false
}

// progress is the tail of the worker's progress.log (else agent.log), for
// a beat.
func (p *Puller) progress(worker, id string) []byte {
	r, err := p.jobRoot(worker, id)
	if err != nil {
		return nil
	}
	defer r.Close()
	for _, name := range []string{progressLog, jobproto.MemberLog} {
		if b, ok, _ := readIn(r, name, jobproto.TailBytes, true); ok {
			return b
		}
	}
	return nil
}

// remove deletes the job's directory from the worker's jobs/.
func (p *Puller) remove(worker, id string) error {
	jr, err := p.jobsRoot(worker)
	if err != nil {
		return err
	}
	defer jr.Close()
	return jr.RemoveAll(id)
}

// pack is the collect tar of a finished job, exactly what factory-remote's
// collect sends (jobproto.PackCollect): report.json, prompt.md, head,
// agent.log (else progress.log), out.bundle, and memory.tar of memory/ when
// the job carried memory. report, when not nil, stands in for the worker's
// report.json (a job that timed out or could not start) and nothing but
// the logs and prompt is taken from the directory.
func (p *Puller) pack(worker, id string, memory bool, report []byte) ([]byte, error) {
	files := map[string][]byte{}
	if report != nil {
		files[jobproto.MemberReport] = report
	}
	r, err := p.jobRoot(worker, id)
	if err != nil {
		if report == nil {
			return nil, err
		}
		return jobproto.PackCollect(files)
	}
	defer r.Close()
	caps := map[string]int64{}
	for _, c := range jobproto.CollectCaps() {
		caps[c.Name] = c.Max
	}
	take := []string{jobproto.MemberPrompt}
	if report == nil {
		take = append(take, jobproto.MemberReport, jobproto.MemberHead, jobproto.MemberOutBundle)
	}
	for _, name := range take {
		b, ok, err := readIn(r, name, caps[name], false)
		if err != nil {
			return nil, err
		}
		if ok {
			files[name] = b
		}
	}
	for _, name := range []string{jobproto.MemberLog, progressLog} {
		if b, ok, _ := readIn(r, name, caps[jobproto.MemberLog], true); ok {
			files[jobproto.MemberLog] = b
			break
		}
	}
	if memory && report == nil {
		if b, err := tarDir(r, memoryDir, caps[jobproto.MemberMemoryTar]); err == nil {
			files[jobproto.MemberMemoryTar] = b
		} else {
			p.logf("job %s: memory left out: %v", id, err)
		}
	}
	return jobproto.PackCollect(files)
}

// readIn reads a regular file in r, at most max bytes: a bigger one is left
// out (ok false), or with tail its last max bytes are read. A missing file,
// or anything but a regular file, is ok false.
func readIn(r *os.Root, name string, limit int64, tail bool) ([]byte, bool, error) {
	f, err := r.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false, nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false, nil
	}
	size := fi.Size()
	if size > limit && !tail {
		return nil, false, nil
	}
	data := make([]byte, min(size, limit))
	n, err := f.ReadAt(data, max(0, size-limit))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	return data[:n], true, nil
}

// tarDir is a tar of the regular files under dir in r, named relative to
// it ("./notes.md", as `tar -C dir -cf - .` names them): links and other
// non-regular entries are left out. A tar over max is an error.
func tarDir(r *os.Root, dir string, max int64) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := fs.WalkDir(r.FS(), dir, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, ok, err := readIn(r, name, max, false)
		if err != nil || !ok {
			return err
		}
		rel := "./" + name[len(dir)+1:]
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: rel, Size: int64(len(b)), Mode: 0o644, ModTime: time.Unix(0, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
		if int64(buf.Len()) > max {
			return fmt.Errorf("memory over %d bytes", max)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if int64(buf.Len()) > max {
		return nil, fmt.Errorf("memory over %d bytes", max)
	}
	return buf.Bytes(), nil
}
