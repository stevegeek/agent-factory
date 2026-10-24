package imagebuild

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
)

// what the weekly rebuild builds for one tool.
func TestDecide(t *testing.T) {
	cases := []struct {
		name   string
		floor  string
		st     ToolState
		auto   bool
		latest string
		want   Decision
	}{
		{"latest newer than the floor", "2.1.296", ToolState{}, true, "2.1.302",
			Decision{Target: "2.1.302", From: "2.1.296", Update: true}},
		{"latest newer than images.json", "2.1.200", ToolState{Version: "2.1.296"}, true, "2.1.302",
			Decision{Target: "2.1.302", From: "2.1.296", Update: true}},
		{"floor newer than latest", "2.2.0", ToolState{Version: "2.1.296"}, true, "2.1.302",
			Decision{Target: "2.2.0", From: "2.2.0"}},
		{"images.json newer than the floor, nothing newer out", "2.1.200", ToolState{Version: "2.1.302"}, true, "2.1.302",
			Decision{Target: "2.1.302", From: "2.1.302"}},
		{"auto-update off", "2.1.296", ToolState{}, false, "2.1.302", Decision{Target: "2.1.296", From: "2.1.296"}},
		{"no lookup", "2.1.296", ToolState{}, true, "", Decision{Target: "2.1.296", From: "2.1.296"}},
		{"a pre-release", "2.1.296", ToolState{}, true, "2.2.0-beta.1",
			Decision{Target: "2.1.296", From: "2.1.296", Why: "2.2.0-beta.1 is a pre-release"}},
		{"held", "2.1.200", ToolState{Version: "2.1.296", Held: "2.1.302"}, true, "2.1.302",
			Decision{Target: "2.1.296", From: "2.1.296", Why: "2.1.302 is held (rolled back); `factory rebuild --unhold` to try it again"}},
		{"newer than the held one", "2.1.200", ToolState{Version: "2.1.296", Held: "2.1.302"}, true, "2.1.303",
			Decision{Target: "2.1.303", From: "2.1.296", Update: true}},
		{"nothing pinned yet", "", ToolState{}, true, "0.9.1", Decision{Target: "0.9.1", Update: true}},
	}
	for _, c := range cases {
		if got := Decide(c.floor, c.st, c.auto, c.latest); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestStateRoundTripAndPromotion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.json")
	st, err := LoadState(path)
	if err != nil || len(st) != 0 {
		t.Fatalf("missing file: %v %v", st, err)
	}
	now := time.Date(2026, 10, 18, 2, 30, 0, 0, time.UTC)
	Promoted(st, "claude", Decision{Target: "2.1.302", From: "2.1.296", Update: true}, now)
	if err := SaveState(path, st); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := "{\n  \"claude\": {\n    \"version\": \"2.1.302\",\n    \"since\": \"2026-10-18T02:30:00+00:00\",\n    \"previous\": \"2.1.296\"\n  }\n}\n"
	if string(b) != want {
		t.Errorf("images.json\n%s\nwant\n%s", b, want)
	}
	again, err := LoadState(path)
	if err != nil || !reflect.DeepEqual(again, st) {
		t.Errorf("reload %v %v", again, err)
	}
	os.WriteFile(path, []byte("{broken"), 0o644)
	if _, err := LoadState(path); err == nil {
		t.Error("a broken images.json read")
	}
}

type tagger struct {
	calls [][2]string
	fail  bool
}

func (g *tagger) Tag(_ context.Context, src, dst string) (execx.Result, error) {
	g.calls = append(g.calls, [2]string{src, dst})
	if g.fail {
		return execx.Result{Code: 125, Stderr: []byte("no such image")}, nil
	}
	return execx.Result{}, nil
}

// rollback retags :previous and holds the version it leaves; unhold
// clears that.
func TestRollbackAndUnhold(t *testing.T) {
	st := State{"claude": {Version: "2.1.302", Since: "x", Previous: "2.1.296"}}
	g := &tagger{}
	now := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	got, err := Rollback(context.Background(), g, st, "claude", "localhost/factory-agent:scion", now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g.calls, [][2]string{{"localhost/factory-agent:previous", "localhost/factory-agent:scion"}}) {
		t.Errorf("tags %v", g.calls)
	}
	// previous still names what :previous holds (the retag leaves it)
	want := ToolState{Version: "2.1.296", Since: "2026-10-19T09:00:00+00:00", Previous: "2.1.296", Held: "2.1.302"}
	if got != want || st["claude"] != want {
		t.Errorf("state %+v", st["claude"])
	}
	if d := Decide("2.1.200", st["claude"], true, "2.1.302"); d.Update {
		t.Errorf("a held version is tried again: %+v", d)
	}
	if !Unhold(st, "claude") || st["claude"].Held != "" || Unhold(st, "claude") {
		t.Errorf("unhold: %+v", st["claude"])
	}
	if _, err := Rollback(context.Background(), &tagger{fail: true}, st, "claude", "x:y", now); err == nil {
		t.Error("a failed retag rolled back")
	}
}

// Rebuilt keeps Previous on what :previous holds after a rebuild with no
// update, and makes no entry for a kind that has none.
func TestRebuiltTracksWhatPreviousHolds(t *testing.T) {
	st := State{"claude": {Version: "2.1.302", Since: "x", Previous: "2.1.296"}}
	if !Rebuilt(st, "claude", "2.1.302") || st["claude"].Previous != "2.1.302" || st["claude"].Version != "2.1.302" {
		t.Errorf("after a rebuild with no update: %+v", st["claude"])
	}
	if Rebuilt(st, "claude", "2.1.302") {
		t.Error("an unchanged entry reported a change")
	}
	if Rebuilt(st, "pi", "0.70.0") || len(st) != 1 {
		t.Errorf("an entry was made: %+v", st)
	}
	g := &tagger{}
	got, err := Rollback(context.Background(), g, st, "claude", "localhost/factory-agent:scion", time.Now())
	if err != nil || got.Held != "" || got.Version != "2.1.302" || len(g.calls) != 1 {
		t.Errorf("rollback to the same version: %+v %v %v", got, err, g.calls)
	}
}

type serve struct{ h http.Handler }

func (s serve) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func TestNPMLatest(t *testing.T) {
	var asked string
	c := &http.Client{Transport: serve{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.String()
		if strings.Contains(r.URL.Path, "missing") {
			w.WriteHeader(404)
			return
		}
		io.WriteString(w, `{"name": "@anthropic-ai/claude-code", "version": "2.1.302"}`)
	})}}
	v, err := NPMLatest(context.Background(), c, "https://npm.example/", "@anthropic-ai/claude-code")
	if err != nil || v != "2.1.302" || asked != "https://npm.example/@anthropic-ai/claude-code/latest" {
		t.Errorf("%q %v %q", v, err, asked)
	}
	if _, err := NPMLatest(context.Background(), c, "https://npm.example", "missing"); err == nil {
		t.Error("a 404 answered")
	}
}
