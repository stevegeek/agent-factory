package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The Live tab: what the agents are doing right now, and the factory's own log. Read-only: nothing
// here can type into an agent. Each screen polls its own small text endpoint while the page is open.

type LiveAgent struct {
	ID, Title, Project, Role, Host, Name, Age, Href string
}

type LiveView struct {
	Agents []LiveAgent
}

var agentName = regexp.MustCompile(`^t\d{4,}-[a-z]\d{1,3}$`)

func liveAgents(ts []*Ticket, now time.Time) []LiveAgent {
	var out []LiveAgent
	for _, t := range ts {
		if t.Agent == nil {
			continue
		}
		role := roles[t.Agent.Stage]
		if role == "" {
			role = t.Agent.Stage
		}
		host := t.Agent.Host
		if host == "local" {
			host = ""
		}
		out = append(out, LiveAgent{ID: t.ID, Title: t.Title, Project: t.Project, Role: capFirst(role), Host: host,
			Name: t.Agent.Name, Age: ageSince(t.Agent.Started, now), Href: "/t/" + t.ID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func running(ts []*Ticket) int {
	n := 0
	for _, t := range ts {
		if t.Agent != nil {
			n++
		}
	}
	return n
}

func (s *Server) livePage(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	s.render(w, r, "live", &Page{Title: "Live", Tab: "live", Live: &LiveView{Agents: liveAgents(ts, s.now())}}, ts)
}

func plain(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, text)
}

// liveScreen: the running agent's screen now (`factory look`), cached for a few seconds so two open
// pages don't double the work on a small box.
func (s *Server) liveScreen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		http.NotFound(w, r)
		return
	}
	t := byID(s.visible(r))[id]
	if t == nil || t.Agent == nil {
		plain(w, "(this agent has finished; its log is on the ticket)")
		return
	}
	plain(w, s.looks.get(id, func() string {
		out, err := s.runner.Look(id)
		if err != nil {
			return "(could not read the screen: " + err.Error() + ")"
		}
		return out
	}))
}

// liveLog: the end of the factory's log, newest last.
func (s *Server) liveLog(w http.ResponseWriter, r *http.Request) {
	plain(w, tailFile(filepath.Join(s.runner.Root, "logs", "factory.log"), 48<<10, 200))
}

// agentLog: an archived agent's log, from the ticket page.
func (s *Server) agentLog(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	if !validID(id) || !agentName.MatchString(name) || !strings.HasPrefix(name, strings.ToLower(id)+"-") {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(s.runner.Root, "logs", id, name, "agent.log")
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	plain(w, tailFile(path, 1<<20, 0))
}

// agentLogs: the archived agent runs of a ticket, oldest first.
func agentLogs(root, id string) []string {
	ents, err := os.ReadDir(filepath.Join(root, "logs", id))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && agentName.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// tailFile: the last max bytes of a file, cut to whole lines; at most n lines when n > 0.
func tailFile(path string, max int64, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	off := st.Size() - max
	if off < 0 {
		off = 0
	}
	f.Seek(off, io.SeekStart)
	b, _ := io.ReadAll(io.LimitReader(f, max))
	if off > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	text := strings.TrimRight(strings.ToValidUTF8(string(b), "�"), "\n")
	if n > 0 {
		lines := strings.Split(text, "\n")
		if len(lines) > n {
			text = strings.Join(lines[len(lines)-n:], "\n")
		}
	}
	return text
}

// Look runs `factory look` outside the answer queue: a screen must never wait behind a PR being opened.
// At most two at once.
func (r *Runner) Look(id string) (string, error) {
	lookSlots <- struct{}{}
	defer func() { <-lookSlots }()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Factory, "look", id, "--lines", "150")
	cmd.Env = append(os.Environ(), "FACTORY_ROOT="+r.Root)
	cmd.WaitDelay = 2 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", &lookError{tail(msg, 300)}
	}
	return out.String(), nil
}

var lookSlots = make(chan struct{}, 2)

// Output runs a read-only factory command outside the answer queue and returns all of its output.
func (r *Runner) Output(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Factory, args...)
	cmd.Env = append(os.Environ(), "FACTORY_ROOT="+r.Root)
	cmd.WaitDelay = 2 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", &lookError{tail(strings.TrimSpace(errb.String())+" "+err.Error(), 300)}
	}
	if out.Len() > 1<<20 {
		return "", &lookError{"output too large"}
	}
	return out.String(), nil
}

type lookError struct{ msg string }

func (e *lookError) Error() string { return e.msg }

// shortCache: a value per key for a few seconds.
type shortCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	vals map[string]cachedText
}

type cachedText struct {
	at   time.Time
	text string
}

func (c *shortCache) get(key string, fill func() string) string {
	c.mu.Lock()
	if v, ok := c.vals[key]; ok && time.Since(v.at) < c.ttl {
		c.mu.Unlock()
		return v.text
	}
	c.mu.Unlock()
	text := fill()
	c.mu.Lock()
	if c.vals == nil || len(c.vals) > 64 {
		c.vals = map[string]cachedText{}
	}
	c.vals[key] = cachedText{time.Now(), text}
	c.mu.Unlock()
	return text
}
