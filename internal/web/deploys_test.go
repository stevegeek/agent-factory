package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stevegeek/agent-factory/internal/deployrec"
)

// fakeHolder records the holds posted and answers with line or err.
type fakeHolder struct {
	mu   sync.Mutex
	got  []string // "held note by"
	line string
	err  error
}

func (f *fakeHolder) HoldDeploys(_ context.Context, held bool, note, by string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := "release"
	if held {
		s = "hold"
	}
	f.got = append(f.got, s+" "+note+" "+by)
	return f.line, f.err
}

func (f *fakeHolder) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// deploysCard is the Deploys section of the System page.
func deploysCard(body string) string {
	i := strings.Index(body, `id="deploys"`)
	if i < 0 {
		return ""
	}
	card := body[i:]
	if j := strings.Index(card, "</section>"); j >= 0 {
		card = card[:j]
	}
	return card
}

func TestSystemShowsTheDeployCard(t *testing.T) {
	h := newHarness(t, false)
	h.srv.svc.Deploys = &fakeHolder{}
	c := h.login()
	card := deploysCard(h.do("GET", "/system", nil, c).Body.String())
	if !strings.Contains(card, "Merges deploy by themselves") || !strings.Contains(card, `value="yes">Hold`) || !strings.Contains(card, "No deploy yet") {
		t.Errorf("an unheld card:\n%s", card)
	}

	if err := deployrec.SetHold(h.paths.DeployHold(), &deployrec.Hold{By: "alice", At: "2026-10-09T10:00:00+00:00", Note: "a demo tonight"}); err != nil {
		t.Fatal(err)
	}
	running := deployrec.State{Phase: deployrec.PhaseRunning, Rev: "main", Commit: "0123456789ab", Step: "deploy.sh", Started: "2026-10-09T11:50:00+00:00"}
	if err := deployrec.WriteState(h.paths.DeployState(), running); err != nil {
		t.Fatal(err)
	}
	card = deploysCard(h.do("GET", "/system", nil, c).Body.String())
	for _, want := range []string{"Held by alice since 9 Oct ", ": a demo tonight", `value="no">Release`, "Deploying", "0123456789ab", "deploy.sh", "since 9 Oct "} {
		if !strings.Contains(card, want) {
			t.Errorf("a held, running card lacks %q:\n%s", want, card)
		}
	}

	failed := deployrec.State{Phase: deployrec.PhaseDone, Rev: "main", Commit: "0123456789ab", From: "ba9876543210", ConfigCommit: "aaaabbbbcccc",
		Started: "2026-10-09T11:00:00+00:00", Finished: "2026-10-09T11:20:00+00:00", Result: deployrec.ResultRolledBack,
		Tail: "deploy.sh: <the check> failed\n", Helpers: map[string]string{"helpera": "ok", "desk": "ssh: no route to host"}}
	if err := deployrec.WriteState(h.paths.DeployState(), failed); err != nil {
		t.Fatal(err)
	}
	card = deploysCard(h.do("GET", "/system", nil, c).Body.String())
	for _, want := range []string{"0123456789ab", "rolled_back", "ba9876543210", "aaaabbbbcccc", "9 Oct ", "helpera", "ok", "desk", "ssh: no route to host",
		"deploy.sh: &lt;the check&gt; failed"} {
		if !strings.Contains(card, want) {
			t.Errorf("a failed card lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "Deploying") {
		t.Error("a finished deploy shown as running")
	}
}

func TestDeployHoldIsMaintainerOnly(t *testing.T) {
	h := newHarness(t, false)
	hold := &fakeHolder{line: "deploys held"}
	h.srv.svc.Deploys = hold
	m := h.login()
	secret, _ := h.users.Add("robin", []string{"busy"})
	robin := h.loginWith(secret, "/")
	if w := h.do("GET", "/system", nil, robin); w.Code != http.StatusNotFound {
		t.Errorf("robin's System status: %d", w.Code)
	}
	if w := h.do("POST", "/system/deploys/hold", url.Values{"csrf": {h.csrf(robin)}, "held": {"yes"}}, robin); w.Code != http.StatusNotFound {
		t.Errorf("robin's hold: %d", w.Code)
	}
	if w := h.do("POST", "/system/deploys/hold", url.Values{"held": {"yes"}}, m); w.Code != http.StatusForbidden {
		t.Errorf("no CSRF token: %d", w.Code)
	}
	if w := h.doWith("POST", "/system/deploys/hold", url.Values{"csrf": {h.csrf(m)}, "held": {"yes"}},
		map[string]string{"Sec-Fetch-Site": "cross-site"}, m); w.Code != http.StatusForbidden {
		t.Errorf("cross-site: %d", w.Code)
	}
	if got := hold.calls(); len(got) != 0 {
		t.Fatalf("held %q", got)
	}
}

func TestDeployHoldPostWritesTheSwitch(t *testing.T) {
	h := newHarness(t, false)
	hold := &fakeHolder{line: "deploys held: a merge leaves a Deploy? ask on the desk"}
	h.srv.svc.Deploys = hold
	c := h.login()
	w := h.do("POST", "/system/deploys/hold", url.Values{"csrf": {h.csrf(c)}, "held": {"yes"}, "note": {"a demo tonight"}}, c)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/system" {
		t.Fatalf("hold: %d %s", w.Code, w.Header().Get("Location"))
	}
	if f := flashOf(t, h, w); f == nil || f.Kind != "ok" || !strings.HasPrefix(f.Text, "Deploys held") {
		t.Errorf("flash %+v", f)
	}
	w = h.do("POST", "/system/deploys/hold", url.Values{"csrf": {h.csrf(c)}, "held": {"no"}}, c)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("release: %d", w.Code)
	}
	if got := hold.calls(); len(got) != 2 || got[0] != "hold a demo tonight " || got[1] != "release  " {
		t.Errorf("calls %q", got)
	}
	if w := h.do("POST", "/system/deploys/hold", url.Values{"csrf": {h.csrf(c)}, "held": {"maybe"}}, c); w.Code != http.StatusBadRequest {
		t.Errorf("a bad held: %d", w.Code)
	}
	hold.err = errors.New("the note is longer than 500 characters")
	w = h.do("POST", "/system/deploys/hold", url.Values{"csrf": {h.csrf(c)}, "held": {"yes"}}, c)
	if f := flashOf(t, h, w); f == nil || f.Kind != "error" || !strings.Contains(f.Text, "longer than") {
		t.Errorf("flash %+v", f)
	}
	// with no holder, the card offers no form
	h.srv.svc.Deploys = nil
	if card := deploysCard(h.do("GET", "/system", nil, c).Body.String()); strings.Contains(card, "/system/deploys/hold") {
		t.Errorf("a form with no holder:\n%s", card)
	}
}
