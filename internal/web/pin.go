package web

import (
	"context"
	"net/http"
	"time"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/sysinfo"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// HostPinner pins a factory ticket's stages to a host, or unpins it with
// "auto" (ops.Service.Host, as `factory host`).
type HostPinner interface {
	Host(ctx context.Context, id, host string) error
}

// RunOn is where a ticket runs: its pin, why that host takes no agent now,
// and, on an open factory ticket, the hosts to choose
// from besides "auto" (any host).
type RunOn struct {
	Pin     string
	Waiting string // said after the host's name ("is paused until 15:40")
	Hosts   []string
}

// runOn is nil for a board ticket, and for a closed ticket with no pin.
func (s *Server) runOn(r *http.Request, t *ticket.Ticket) *RunOn {
	if t.Board() {
		return nil
	}
	v := &RunOn{Pin: t.Host.Or("")}
	if v.Pin != "" {
		v.Waiting = s.hostWaiting(s.now())(v.Pin)
	}
	if s.svc.Pins != nil && !model.Closed(t.Column) {
		v.Hosts = s.hostChoices()
	}
	if v.Pin == "" && v.Hosts == nil {
		return nil
	}
	return v
}

// hostChoices are the hosts a ticket may be pinned to: this one, then
// [hosts] in config order.
func (s *Server) hostChoices() []string {
	out := []string{model.LocalHost}
	if s.svc.Config == nil {
		return out
	}
	if cfg, err := s.svc.Config.Current(); err == nil && cfg != nil {
		for _, h := range cfg.Hosts {
			out = append(out, h.Name)
		}
	}
	return out
}

// hostWaiting tells why a host takes no new agent now, said after its
// name, or "" when it does or nothing can be told.
func (s *Server) hostWaiting(now time.Time) func(string) string {
	plan, _ := s.flightPlan(now)
	if !plan.Known {
		return func(string) string { return "" }
	}
	down := map[string]string{model.LocalHost: plan.LocalDown}
	known := map[string]bool{model.LocalHost: true}
	for _, h := range plan.Hosts {
		down[h.Name], known[h.Name] = h.Down, true
	}
	if s.paths.Root != "" {
		if snap, err := sysinfo.Read(sysinfo.File(s.paths)); err == nil {
			for _, m := range machines(snap, now.Sub(snap.Taken) <= StaleAfter, now) {
				if m.State == "offline" && down[m.Name] == "" {
					down[m.Name] = "is offline"
				}
			}
		}
	}
	return func(name string) string {
		if !known[name] {
			return "is not in config.toml"
		}
		return down[name]
	}
}

func (s *Server) pinHost(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t := findTicket(s.visible(r), id)
	if !validID(id) || t == nil {
		http.NotFound(w, r)
		return
	}
	back := "/t/" + id
	if s.svc.Pins == nil {
		s.flashRedirect(w, r, back, "error", "Pinning a ticket to a host is off on this server.")
		return
	}
	host := r.PostFormValue("host")
	if host == "" {
		http.Error(w, "host is a host's name or auto", http.StatusBadRequest)
		return
	}
	err := s.call(r, fileTimeout, func(ctx context.Context) error { return s.svc.Pins.Host(ctx, id, host) })
	if err != nil {
		s.logf("host %s %q by %s: %v", id, host, s.who(r), err)
		s.flashRedirect(w, r, back, "error", sentence(err.Error()))
		return
	}
	s.logf("host %s %s by %s", id, host, s.who(r))
	if host == "auto" {
		s.flashRedirect(w, r, back, "ok", id+" runs on any host.")
		return
	}
	s.flashRedirect(w, r, back, "ok", id+" is pinned to "+host+".")
}

// pinWaits says on each pinned card of bv's open lanes why its host
// takes no agent now.
func (s *Server) pinWaits(bv *BoardView) {
	var waiting func(string) string
	for i := range bv.Lanes {
		for j := range bv.Lanes[i].Tickets {
			lt := &bv.Lanes[i].Tickets[j]
			if lt.Host == "" {
				continue
			}
			if waiting == nil {
				waiting = s.hostWaiting(s.now())
			}
			lt.HostWaiting = waiting(lt.Host)
		}
	}
}
