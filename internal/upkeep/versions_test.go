package upkeep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/imagebuild"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// registry is a recorded GitHub and npm: answers by URL path, in process
// (no socket), recording each request as the golden does.
type registry struct {
	mu       sync.Mutex
	answers  map[string]answer
	requests []request
}

type answer struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

type request struct {
	Path      string  `json:"path"`
	UserAgent string  `json:"user_agent"`
	Accept    *string `json:"accept"`
}

func (r *registry) Do(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rq := request{Path: req.URL.Path, UserAgent: req.Header.Get("User-Agent")}
	if a := req.Header.Values("Accept"); len(a) > 0 {
		rq.Accept = &a[0]
	}
	r.requests = append(r.requests, rq)
	a, ok := r.answers[req.URL.Path]
	if !ok {
		a = answer{Status: 404, Body: json.RawMessage(`{"message":"Not Found"}`)}
	}
	return &http.Response{
		StatusCode: a.Status,
		Status:     strconv.Itoa(a.Status) + " " + http.StatusText(a.Status),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(a.Body)),
		Request:    req,
	}, nil
}

type versionsRow struct {
	Name        string            `json:"name"`
	Config      string            `json:"config"`
	Registry    map[string]answer `json:"registry"`
	OpenNotices []string          `json:"open_notices"`
	Requests    []request         `json:"requests"`
	Log         []string          `json:"log"`
	Filed       []filed           `json:"filed"`
}

type filed struct {
	Project string `json:"project"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Door    string `json:"door"`
	Column  string `json:"column"`
}

// check_versions against a recorded GitHub and npm registry, golden from
// Python (§10.3): the requests, the log lines and the notices it files.
func TestVersionsGolden(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "py", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Rows []versionsRow `json:"rows"`
	}
	if err := json.Unmarshal(data, &golden); err != nil {
		t.Fatal(err)
	}
	if len(golden.Rows) == 0 {
		t.Fatal("no rows")
	}
	for _, row := range golden.Rows {
		t.Run(row.Name, func(t *testing.T) {
			f := newFixture(t)
			for _, title := range row.OpenNotices {
				if _, err := f.board.FileTicket(t.Context(), ticket.NewIn{Project: "factory", Title: title, Body: "b", Door: "clock", Column: "needs_human"}); err != nil {
					t.Fatal(err)
				}
			}
			before := len(f.tickets(t))
			reg := &registry{answers: row.Registry}
			s := Service{
				Releases: &github.Client{BaseURL: "http://registry.test/gh", NPMURL: "http://registry.test/npm", HTTP: reg},
				Notices:  f.doors,
				Log:      f.log,
			}
			if err := s.Versions(t.Context(), parse(t, row.Config)); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reg.requests, row.Requests) {
				t.Errorf("requests\n%+v\nwant\n%+v", reg.requests, row.Requests)
			}
			var logs []string
			for _, l := range f.log.all() {
				if !strings.Contains(l, " created door=") {
					logs = append(logs, l)
				}
			}
			if strings.Join(logs, "\n") != strings.Join(row.Log, "\n") {
				t.Errorf("log %q, want %q", logs, row.Log)
			}
			var got []filed
			for _, tk := range f.tickets(t)[before:] {
				got = append(got, filed{tk.Project, tk.Title, tk.Body, tk.Door, tk.Column})
			}
			if len(got) != len(row.Filed) {
				t.Fatalf("filed %d, want %d: %+v", len(got), len(row.Filed), got)
			}
			for i := range got {
				if got[i] != row.Filed[i] {
					t.Errorf("filed[%d]\n%+v\nwant\n%+v", i, got[i], row.Filed[i])
				}
			}
		})
	}
}

// fakeReleases answers every release and npm lookup from a function.
type fakeReleases struct {
	github func(repo string) (string, error)
	npm    func(pkg string) (string, error)
	asked  []string
}

func (f *fakeReleases) LatestRelease(_ context.Context, repo string) (string, error) {
	f.asked = append(f.asked, repo)
	if f.github == nil {
		return "", errors.New("no github")
	}
	return f.github(repo)
}

func (f *fakeReleases) NPMLatest(_ context.Context, pkg string) (string, error) {
	f.asked = append(f.asked, pkg)
	if f.npm == nil {
		return "", errors.New("no npm")
	}
	return f.npm(pkg)
}

// titles records the notices asked for.
type titles struct{ got []string }

func (n *titles) Notice(_ context.Context, title, _ string) (*ticket.Ticket, error) {
	n.got = append(n.got, title)
	return nil, nil
}

// port: TF ProjectFactsTest.test_a_new_crystal_release_files_a_notice_only_when_a_project_uses_crystal
func TestCrystalNoticeOnlyWhenAProjectUsesCrystal(t *testing.T) {
	n := &titles{}
	s := Service{
		Releases: &fakeReleases{github: func(string) (string, error) { return "1.22.0", nil }},
		Notices:  n,
		Log:      &memLog{},
	}
	ruby := "[images]\ncrystal = \"1.20.1\"\n[projects.a]\nlanguage = \"Ruby gem\"\n"
	if err := s.Versions(t.Context(), parse(t, ruby)); err != nil {
		t.Fatal(err)
	}
	if len(n.got) != 0 {
		t.Fatalf("notices %v without a crystal project", n.got)
	}
	if err := s.Versions(t.Context(), parse(t, ruby+"[projects.b]\nlanguage = \"Crystal shard\"\n")); err != nil {
		t.Fatal(err)
	}
	// ameba has no pin: its default (1.7.0) is what the images have
	if want := []string{"Update Crystal 1.20.1 -> 1.22.0", "Update ameba 1.7.0 -> 1.22.0"}; !reflect.DeepEqual(n.got, want) {
		t.Fatalf("notices %v, want %v", n.got, want)
	}
}

// port: TF UpkeepTest.test_newer_release_files_one_notice_and_tick_ignores_it
// (the version-check half: one notice per title while it is open; the tick
// ignoring it is pipeline's)
func TestNewerReleaseFilesOneNotice(t *testing.T) {
	f := newFixture(t)
	latest := map[string]string{"@anthropic-ai/claude-code": "2.1.300", "@earendil-works/pi-coding-agent": "1.2.0"}
	s := Service{
		Releases: &fakeReleases{npm: func(p string) (string, error) { return latest[p], nil }},
		Notices:  f.doors,
		Log:      f.log,
	}
	cfg := parse(t, "[images]\nclaude = \"2.1.296\"\npi = \"1.2.0\"\n")
	for range 2 {
		if err := s.Versions(t.Context(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	type row struct{ title, column, project string }
	var got []row
	for _, tk := range f.tickets(t) {
		got = append(got, row{tk.Title, tk.Column, tk.Project})
	}
	if want := []row{{"Update Claude Code 2.1.296 -> 2.1.300", "needs_human", "factory"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("tickets %v", got)
	}
	if h := f.tickets(t)[0].History; h[len(h)-1].Event != "created" {
		t.Fatalf("history %+v", h)
	}
}

// §15.1: the pin is a floor. Once the weekly rebuild has updated an image
// past it (images.json), versions compares the release with that version:
// no notice for what the image already has, and a notice for a newer one
// says what the image has.
func TestVersionsComparesWithImagesJSON(t *testing.T) {
	f := newFixture(t)
	if err := imagebuild.SaveState(f.board.Paths.ImagesState(), imagebuild.State{
		"claude": {Version: "2.1.302", Since: "2026-10-12T04:00:00+00:00", Previous: "2.1.296"},
	}); err != nil {
		t.Fatal(err)
	}
	latest := "2.1.302"
	s := Service{
		Board:    f.board,
		Releases: &fakeReleases{npm: func(string) (string, error) { return latest, nil }},
		Notices:  f.doors,
		Log:      f.log,
	}
	cfg := parse(t, "[images]\nclaude = \"2.1.296\"\n")
	if err := s.Versions(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if ts := f.tickets(t); len(ts) != 0 {
		t.Fatalf("a notice for the version the image has: %q", ts[0].Title)
	}
	latest = "2.1.310"
	if err := s.Versions(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	ts := f.tickets(t)
	if len(ts) != 1 || ts[0].Title != "Update Claude Code 2.1.302 -> 2.1.310" ||
		!strings.Contains(ts[0].Body, "the images have 2.1.302 (the weekly rebuild updated them; the [images] pin 2.1.296 is a floor)") {
		t.Fatalf("notices %+v", ts)
	}
}

func TestVersionsSkipsAnImageWithoutAPin(t *testing.T) {
	rel := &fakeReleases{npm: func(string) (string, error) { return "9.9.9", nil }}
	n := &titles{}
	s := Service{Releases: rel, Notices: n, Log: &memLog{}}
	if err := s.Versions(t.Context(), parse(t, "[images]\npi = \"1.0.0\"\n")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"@earendil-works/pi-coding-agent"}; !reflect.DeepEqual(rel.asked, want) {
		t.Fatalf("asked %v", rel.asked)
	}
	if want := []string{"Update Pi 1.0.0 -> 9.9.9"}; !reflect.DeepEqual(n.got, want) {
		t.Fatalf("notices %v", n.got)
	}
}

func TestVersionsLogsAFailureAndGoesOn(t *testing.T) {
	l := &memLog{}
	n := &titles{}
	s := Service{
		Releases: &fakeReleases{
			github: func(repo string) (string, error) {
				if repo == "crystal-lang/crystal" {
					return "", errors.New("HTTP Error 500: Internal Server Error")
				}
				return "2.0", nil
			},
			npm: func(string) (string, error) { return "", errors.New("no version") },
		},
		Notices: n, Log: l,
	}
	if err := s.Versions(t.Context(), parse(t, "[images]\nclaude = \"1.0.0\"\n[projects.c]\nlanguage = \"CRYSTAL\"\n")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"versions Crystal: HTTP Error 500: Internal Server Error", "versions Claude Code: no version"}; !reflect.DeepEqual(l.all(), want) {
		t.Fatalf("log %q", l.all())
	}
	if want := []string{"Update ameba 1.7.0 -> 2.0"}; !reflect.DeepEqual(n.got, want) {
		t.Fatalf("notices %v", n.got)
	}
}

func TestVersionTuple(t *testing.T) {
	cases := []struct {
		latest, pinned string
		newer          bool
	}{
		{"2.1.300", "2.1.296", true},
		{"2.1.296", "2.1.296", false},
		{"2.10.0", "2.9.9", true},
		{"1.0.0-beta", "1.0", true}, // (1, 0, 0) > (1, 0): Python compares tuples
		{"1.0", "1.0.0", false},
		{"v1.2.3.4", "1.2.3.9", false}, // only the first three numbers
		{"1.2.3", "", true},            // () is less than anything
		{"", "", false},
		{"x", "", false},
		{"١.٢.٤", "1.2.3", true},               // \d is Unicode: Arabic-Indic digits count
		{"99999999999999999999999", "1", true}, // Python ints do not overflow
		{"007", "7", false},
	}
	for _, tc := range cases {
		if got := newer(tc.latest, tc.pinned); got != tc.newer {
			t.Errorf("newer(%q, %q) = %v, want %v", tc.latest, tc.pinned, got, tc.newer)
		}
	}
}

// The toolchain notices follow the project's kind, not its language's
// wording.
func TestVersionsByToolchain(t *testing.T) {
	n := &titles{}
	s := Service{
		Releases: &fakeReleases{github: func(string) (string, error) { return "1.22.0", nil }},
		Notices:  n,
		Log:      &memLog{},
	}
	ruby := "[images]\ncrystal = \"1.20.1\"\n[projects.a]\nkind = \"ruby-gem\"\n"
	if err := s.Versions(t.Context(), parse(t, ruby)); err != nil {
		t.Fatal(err)
	}
	if len(n.got) != 0 {
		t.Fatalf("notices %v for a Ruby-only config", n.got)
	}
	if err := s.Versions(t.Context(), parse(t, ruby+"[projects.b]\nkind = \"crystal-shard\"\nlanguage = \"Framework extension\"\n")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Update Crystal 1.20.1 -> 1.22.0", "Update ameba 1.7.0 -> 1.22.0"}; !reflect.DeepEqual(n.got, want) {
		t.Fatalf("notices %v, want %v", n.got, want)
	}
}

func TestToolchainNamesAgree(t *testing.T) {
	for _, tool := range Tools() {
		if !slices.Contains(config.ToolchainNames(), tool.Toolchain) {
			t.Errorf("%s: toolchain %q is not one of %q", tool.Key, tool.Toolchain, config.ToolchainNames())
		}
	}
}

// P5 B3: where the latest-toolchain trial runs (it is on, a gate image is
// configured, and an enabled Crystal project has a checkout here), its
// notice replaces the Crystal
// and ameba ones; the harness notices stay. With it off, or no checkout
// here, the notices are as before.
func TestVersionsLeavesTheToolchainsToTheTrial(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, doc string
		want      []string
	}{
		{"trial on", "[projects.s]\npath = \"" + dir + "\"\nkind = \"crystal-shard\"\n",
			[]string{"Update Claude Code 2.1.296 -> 2.1.300"}},
		{"no gate image to try", "no-image[projects.s]\npath = \"" + dir + "\"\nkind = \"crystal-shard\"\n",
			[]string{"Update Crystal 1.20.1 -> 1.22.0", "Update ameba 1.7.0 -> 1.22.0", "Update Claude Code 2.1.296 -> 2.1.300"}},
		{"trial off", "[upkeep]\nlatest_every_days = 0\n[projects.s]\npath = \"" + dir + "\"\nkind = \"crystal-shard\"\n",
			[]string{"Update Crystal 1.20.1 -> 1.22.0", "Update ameba 1.7.0 -> 1.22.0", "Update Claude Code 2.1.296 -> 2.1.300"}},
		{"paused", "[projects.s]\npath = \"" + dir + "\"\nkind = \"crystal-shard\"\nenabled = false\n",
			[]string{"Update Crystal 1.20.1 -> 1.22.0", "Update ameba 1.7.0 -> 1.22.0", "Update Claude Code 2.1.296 -> 2.1.300"}},
		{"no checkout here", "[projects.s]\npath = \"/nonexistent/checkout\"\nkind = \"crystal-shard\"\n",
			[]string{"Update Crystal 1.20.1 -> 1.22.0", "Update ameba 1.7.0 -> 1.22.0", "Update Claude Code 2.1.296 -> 2.1.300"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &titles{}
			s := Service{
				Releases: &fakeReleases{
					github: func(string) (string, error) { return "1.22.0", nil },
					npm:    func(string) (string, error) { return "2.1.300", nil },
				},
				Notices: n,
				Log:     &memLog{},
			}
			doc := "image = \"localhost/factory-agent:scion\"\n[images]\nclaude = \"2.1.296\"\ncrystal = \"1.20.1\"\n" + tc.doc
			if rest, ok := strings.CutPrefix(tc.doc, "no-image"); ok {
				doc = "[images]\nclaude = \"2.1.296\"\ncrystal = \"1.20.1\"\n" + rest
			}
			if err := s.Versions(t.Context(), parse(t, doc)); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(n.got, tc.want) {
				t.Fatalf("notices %q, want %q", n.got, tc.want)
			}
		})
	}
}
