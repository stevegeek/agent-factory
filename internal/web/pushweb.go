package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/stevegeek/agent-factory/internal/desk"
	"github.com/stevegeek/agent-factory/internal/present"
	"github.com/stevegeek/agent-factory/internal/push"
)

// SettingsView is the settings page.
type SettingsView struct {
	PushKey string // empty: notifications are off on this server
	Devices int    // the viewer's own devices (bug 7)
	People  *desk.PeopleView
}

func (s *Server) manifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	fmt.Fprint(w, `{"name":"Foreman","short_name":"Foreman","description":"The factory's desk: what needs you, and what happened.",`+
		`"start_url":"/","scope":"/","display":"standalone","background_color":"#111519","theme_color":"#111519",`+
		`"icons":[{"src":"/static/icons/icon-192.png","sizes":"192x192","type":"image/png"},`+
		`{"src":"/static/icons/icon-512.png","sizes":"512x512","type":"image/png"},`+
		`{"src":"/static/icons/icon-512.png","sizes":"512x512","type":"image/png","purpose":"maskable"}]}`)
}

func (s *Server) serviceWorker(w http.ResponseWriter, r *http.Request) {
	data, err := staticFS.ReadFile("static/sw.js")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

// pushUser is the name a viewer's subscriptions carry: "" for the
// maintainer.
func (s *Server) pushUser(r *http.Request) string {
	if u := s.viewer(r); u != nil {
		return u.Name
	}
	return ""
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	ts := s.visible(r)
	v := &SettingsView{People: s.peopleView(r)}
	if s.push != nil {
		v.PushKey, v.Devices = s.push.PublicKey(), s.push.CountFor(s.pushUser(r))
	}
	s.render(w, r, "settings", &Page{Title: "Settings", Tab: "settings", Settings: v}, ts)
}

func jsonReply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func pushOff(w http.ResponseWriter) {
	jsonReply(w, http.StatusNotFound, map[string]string{"error": "notifications are off on this server"})
}

func (s *Server) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		pushOff(w)
		return
	}
	var sub push.Subscription
	if err := json.Unmarshal([]byte(r.PostFormValue("subscription")), &sub); err != nil {
		jsonReply(w, http.StatusBadRequest, map[string]string{"error": "bad subscription"})
		return
	}
	sub.User = s.pushUser(r)
	if err := s.push.Add(sub); err != nil {
		jsonReply(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.logf("push: a device of %s subscribed (%s)", s.who(r), push.HostOf(sub.Endpoint))
	jsonReply(w, http.StatusOK, map[string]int{"devices": s.push.CountFor(sub.User)})
}

func (s *Server) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	n := 0
	if s.push != nil {
		user := s.pushUser(r)
		if err := s.push.RemoveFor(user, r.PostFormValue("endpoint")); err != nil {
			s.logf("%v", err)
		}
		n = s.push.CountFor(user)
	}
	jsonReply(w, http.StatusOK, map[string]int{"devices": n})
}

func (s *Server) pushTest(w http.ResponseWriter, r *http.Request) {
	if s.push == nil {
		pushOff(w)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	m := push.Message{Title: "Foreman", Body: "Notifications work. You'll hear when something needs you.", URL: "/",
		Tag: "test", Badge: present.Waiting(present.Desk(s.visible(r), s.deskViewer(r)))}
	user := s.pushUser(r)
	n := s.push.SendTo(ctx, user, m)
	jsonReply(w, http.StatusOK, map[string]int{"sent": n, "devices": s.push.CountFor(user)})
}
