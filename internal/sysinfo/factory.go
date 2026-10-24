package sysinfo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/model"
)

// logTail is how much of factory.log's end is searched for the rebuilds'
// verdicts; one that rotated out is carried over from the last snapshot.
const logTail = 1 << 20

// factory fills in the factory's own state: agents and hosts from the
// board and config, pauses, images.json and the last rebuilds.
func factory(s *Snapshot, d Deps, problem func(string, error)) {
	if d.Tickets != nil {
		for _, t := range d.Tickets() {
			if a, ok := t.Agent.Get(); ok {
				host := a.Host
				if host == "" {
					host = model.LocalHost
				}
				s.Agents[host]++
			}
		}
	}
	if d.Pauses != nil {
		s.Pauses = d.Pauses()
	}
	if d.Switches != nil {
		if sw, err := d.Switches(); err != nil {
			problem("project switches", err)
		} else if len(sw) > 0 {
			s.Switches = sw
		}
	}
	if d.Hold != nil {
		if h, err := d.Hold(); err != nil {
			problem("deploy hold", fmt.Errorf("%w (deploys are held)", err))
		} else {
			s.DeployHold = h
		}
	}
	if d.Config != nil {
		s.Hosts = append(s.Hosts, Host{Name: model.LocalHost, Slots: d.Config.MaxAgents, Agents: s.Agents[model.LocalHost]})
		for _, h := range d.Config.Hosts {
			s.Hosts = append(s.Hosts, Host{Name: h.Name, Slots: h.Slots, Agents: s.Agents[h.Name], Pull: h.Pull()})
		}
		for i := range s.Hosts {
			for _, p := range s.Pauses {
				if p.Host == s.Hosts[i].Name {
					until := p.Until
					s.Hosts[i].PausedUntil = &until
				}
			}
		}
	}
	if b, err := fsx.ReadNoFollow(d.Paths.ImagesState(), maxFile); err == nil {
		var im map[string]Image
		if err := json.Unmarshal(b, &im); err != nil {
			problem("images.json", err)
		} else {
			s.Images = im
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		problem("images.json", err)
	}
	s.Rebuilds = rebuilds(d)
}

// rebuilds are logs/rebuild-<kind>.log, each with its factory.log verdict.
func rebuilds(d Deps) []Rebuild {
	logs, _ := filepath.Glob(filepath.Join(d.Paths.Logs, "rebuild-*.log"))
	sort.Strings(logs)
	var tail string
	if len(logs) > 0 {
		tail = readTail(d.Paths.FactoryLog, logTail)
	}
	var out []Rebuild
	for _, l := range logs {
		fi, err := os.Lstat(l)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		kind := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(l), "rebuild-"), ".log")
		r := Rebuild{Kind: kind, When: fi.ModTime().UTC()}
		r.Verdict, r.Summary = verdict(tail, kind)
		if r.Verdict == "" && d.Previous != nil {
			for _, p := range d.Previous.Rebuilds {
				if p.Kind == kind && p.When.Equal(r.When) {
					r.Verdict, r.Summary = p.Verdict, p.Summary
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// verdict is the last "rebuild <kind>: ok: ..." or "...: FAILED: ..." line
// imagebuild logged.
func verdict(log, kind string) (string, string) {
	lead := "rebuild " + kind + ": "
	lines := strings.Split(log, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		_, rest, ok := strings.Cut(lines[i], lead)
		if !ok {
			continue
		}
		for _, v := range []string{"ok", "FAILED"} {
			if sum, ok := strings.CutPrefix(rest, v+": "); ok {
				return v, strings.TrimSpace(sum)
			}
		}
	}
	return "", ""
}

// readTail is the last n bytes of a regular file, never through a link;
// "" when it cannot be read.
func readTail(path string, n int64) string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	if off := fi.Size() - n; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return ""
		}
	}
	b, _ := io.ReadAll(io.LimitReader(f, n))
	return string(b)
}
