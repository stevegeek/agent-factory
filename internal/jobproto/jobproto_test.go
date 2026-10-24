package jobproto

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The Python patterns this package ports, run over the regex corpus
// (internal/textx/testdata/py/regex.json, written by the Python goldens tool).
// Only the trailing-newline cases differ, on purpose.
func TestNamesMatchTheRegexGolden(t *testing.T) {
	b, err := os.ReadFile("../textx/testdata/py/regex.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Patterns []struct {
			Name  string
			Cases []struct {
				In  string
				Out any
			}
		}
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	ours := map[string]func(string) bool{
		"job_name":    JobNameOK,
		"branch_name": branchName.MatchString,
		"model_name":  modelName.MatchString,
	}
	seen := 0
	for _, p := range doc.Patterns {
		match, ok := ours[p.Name]
		if !ok {
			continue
		}
		seen++
		for _, c := range p.Cases {
			want := c.Out != nil
			if strings.HasSuffix(c.In, "\n") {
				want = false // Python's $ lets a final newline through; we refuse it
			}
			if got := match(c.In); got != want {
				t.Errorf("%s(%q) = %v, want %v", p.Name, c.In, got, want)
			}
		}
	}
	if seen != len(ours) {
		t.Fatalf("found %d of %d patterns in the golden", seen, len(ours))
	}
}

func TestSSHArgvIsRemotes(t *testing.T) {
	h := Host{Name: "pi", SSH: "factory@pi", Key: "/home/f/.ssh/factory-remote"}
	got, err := SSHArgv(h, VerbStatus, "t0003-x1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "IdentitiesOnly=yes",
		"-i", "/home/f/.ssh/factory-remote", "factory@pi", "status", "t0003-x1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
	got, err = SSHArgv(h, VerbList, "")
	if err != nil || got[len(got)-1] != "list" {
		t.Errorf("list: %q %v", got, err)
	}
	got, err = SSHArgv(h, VerbVitals, "")
	if err != nil || got[len(got)-1] != "vitals" || got[len(got)-2] != "factory@pi" {
		t.Errorf("vitals: %q %v", got, err)
	}
	for _, c := range []struct {
		h         Host
		verb, job string
	}{
		{h, VerbRun, "t0001-t1"},     // run is argv only, never sent
		{h, "rm", "t0001-t1"},        // not a verb
		{h, VerbStatus, "../../etc"}, // not a job name
		{h, VerbStatus, "t0001-t1\n"},
		{h, VerbStatus, "-x"},
		{h, VerbStatus, ""},
		{h, VerbList, "t0001-t1"},
		{h, VerbVitals, "t0001-t1"},
		{Host{Name: "x", SSH: "-oProxyCommand=sh", Key: "k"}, VerbStatus, "t0001-t1"},
		{Host{Name: "x", SSH: "", Key: "k"}, VerbStatus, "t0001-t1"},
		{Host{Name: "x", SSH: "a@b"}, VerbStatus, "t0001-t1"},
	} {
		if argv, err := SSHArgv(c.h, c.verb, c.job); err == nil {
			t.Errorf("%+v %q %q: %q", c.h, c.verb, c.job, argv)
		}
	}
}

func TestRemoteErrorsReadAsPythons(t *testing.T) {
	long := strings.Repeat("é", 400)
	for _, c := range []struct {
		err  *RemoteError
		want string
	}{
		{Failed("factory@pi", "start", 2, []byte("  pi already exists\n")), "factory@pi start failed (2): pi already exists"},
		{Failed("h", "status", 255, []byte("\x1cbad\xe2\x82\x1f")), "h status failed (255): bad�"},
		{Failed("h", "start", -9, []byte(long)), "h start failed (-9): " + strings.Repeat("é", 300)},
		{TimedOut("factory@pi", "collect"), "factory@pi collect timed out"},
	} {
		if c.err.Error() != c.want {
			t.Errorf("got %q\nwant %q", c.err, c.want)
		}
	}
}

func TestDecodeTextIsPythonsReplace(t *testing.T) {
	// The outputs of bytes.decode(errors="replace") in CPython 3.14.
	for in, want := range map[string]string{
		"\xe2\x82A":         "�A",
		"\xed\xa0\x80":      "���",
		"\xf0\x9f\x98":      "�",
		"\xc3":              "�",
		"\x80\x80":          "��",
		"a\xff\xfeb":        "a��b",
		"\xe0\x80\x80":      "���",
		"\xf4\x90\x80\x80":  "����",
		"\xf0\x9f\x98\x80x": "\U0001f600x",
		"\xc2\xa9":          "©",
		"plain":             "plain",
	} {
		if got := DecodeText([]byte(in)); got != want {
			t.Errorf("%q: got %+q want %+q", in, got, want)
		}
	}
}

func TestTarRoundTripAndOrder(t *testing.T) {
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	files := []File{{MemberPrompt, []byte("p")}, {MemberRole, []byte("# r\n")}, {MemberMeta, []byte("{}")},
		{"untracked/Gemfile.lock", []byte("GEM\n")}, {"memory/notes/x.md", nil}}
	b, err := TarFlat(files, at)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(b))
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
		if h.Mode != 0o644 || !h.ModTime.Equal(at) || h.Uid != 0 || h.Uname != "" || h.Typeflag != tar.TypeReg {
			t.Errorf("%s: header %+v", h.Name, h)
		}
	}
	if !reflect.DeepEqual(names, []string{"prompt.md", "role.md", "meta.json", "untracked/Gemfile.lock", "memory/notes/x.md"}) {
		t.Errorf("order %q", names)
	}
	flat, err := UntarFlat(b)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{"prompt.md": []byte("p"), "role.md": []byte("# r\n"), "meta.json": []byte("{}")}
	if !reflect.DeepEqual(flat, want) {
		t.Errorf("flat %q", flat)
	}
}

// Only flat regular files come back from a helper: links, directories,
// devices and anything with a "/" are dropped (untar_bytes).
func TestUntarKeepsOnlyFlatRegularFiles(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(h *tar.Header, data string) {
		h.Size = int64(len(data))
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
			data = ""
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(data))
	}
	add(&tar.Header{Name: "report.json", Typeflag: tar.TypeReg, Mode: 0o644}, `{"a": 1}`)
	add(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644}, "x")
	add(&tar.Header{Name: "/etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644}, "x")
	add(&tar.Header{Name: "./head", Typeflag: tar.TypeReg, Mode: 0o644}, "x")
	add(&tar.Header{Name: "agent.log", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}, "")
	add(&tar.Header{Name: "prompt.md", Typeflag: tar.TypeLink, Linkname: "report.json"}, "")
	add(&tar.Header{Name: "dir", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: "dev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}, "")
	add(&tar.Header{Name: "head", Typeflag: tar.TypeReg, Mode: 0o4755}, "abc\n")
	add(&tar.Header{Name: "report.json", Typeflag: tar.TypeReg, Mode: 0o644}, `{"b": 2}`) // a later one wins
	tw.Close()
	got, err := UntarFlat(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{"report.json": []byte(`{"b": 2}`), "head": []byte("abc\n")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}

func TestUntarErrorsLikeTarfile(t *testing.T) {
	if _, err := UntarFlat(nil); !errors.Is(err, ErrEmptyTar) {
		t.Errorf("empty: %v", err)
	}
	if _, err := UntarFlat([]byte("not a tar at all, just some text that is long enough")); err == nil {
		t.Error("garbage accepted")
	}
	good, _ := TarFlat([]File{{"report.json", []byte("{}")}, {"agent.log", []byte(strings.Repeat("x", 600))}}, time.Unix(0, 0))
	// A bad header after the first member ends the archive quietly.
	bad := append([]byte{}, good...)
	copy(bad[1024:], bytes.Repeat([]byte{0xff}, 512))
	got, err := UntarFlat(bad)
	if err != nil || len(got) != 1 || string(got["report.json"]) != "{}" {
		t.Errorf("bad second header: %q %v", got, err)
	}
	// A member cut short is an error.
	if _, err := UntarFlat(good[:512+512+512+100]); err == nil {
		t.Error("truncated member accepted")
	}
	// Only zero blocks: no members, no error.
	if got, err := UntarFlat(make([]byte, 1024)); err != nil || len(got) != 0 {
		t.Errorf("zero blocks: %q %v", got, err)
	}
}

func TestParseStatus(t *testing.T) {
	for in, want := range map[string]string{
		`{"state": "done"}`: StateDone, `{"state": "running"}`: StateRunning, `{"state": "missing"}`: StateMissing,
		`{"state": 3}`: "", `{"state": "odd", "x": 1}`: "odd",
	} {
		if got, err := ParseStatus([]byte(in)); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, in := range []string{``, `{}`, `[]`, `{"started": "t1"}`, `nope`} {
		if _, err := ParseStatus([]byte(in)); !errors.Is(err, ErrStatus) {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestContractTables(t *testing.T) {
	if !reflect.DeepEqual(SSHVerbs(), []string{"start", "status", "collect", "tail", "remove", "list", "vitals", "build", "install"}) {
		t.Error(SSHVerbs())
	}
	if IsSSHVerb(VerbRun) {
		t.Error("run is an SSH verb")
	}
	caps := map[string]int64{}
	var total int64
	for _, c := range CollectCaps() {
		caps[c.Name] = c.Max
		total += c.Max
	}
	want := map[string]int64{"report.json": 1_000_000, "prompt.md": 1_000_000, "head": 1_000, "agent.log": 2_000_000,
		"out.bundle": 100 * 1024 * 1024, "memory.tar": 4_000_000}
	if !reflect.DeepEqual(caps, want) {
		t.Errorf("caps %v", caps)
	}
	if int64(OutputCap(VerbCollect)) < total+64*1024 {
		t.Errorf("collect cap %d under the members' %d", OutputCap(VerbCollect), total)
	}
	if Timeout(VerbCollect) != 300*time.Second || Timeout(VerbTail) != 20*time.Second || Timeout(VerbStart) != 120*time.Second ||
		Timeout(VerbVitals) != 20*time.Second {
		t.Error("timeouts")
	}
	if OutputCap(VerbVitals) != MaxVitals || MaxVitals != 64<<10 {
		t.Errorf("vitals cap %d", OutputCap(VerbVitals))
	}
}

// A build is one synchronous call: a source tar in, the binaries out.
func TestBuildVerbLimits(t *testing.T) {
	if Timeout(VerbBuild) != 20*time.Minute || OutputCap(VerbBuild) != 96<<20 || MaxBuildInput != 32<<20 {
		t.Errorf("build limits: %v %d %d", Timeout(VerbBuild), OutputCap(VerbBuild), MaxBuildInput)
	}
	if !reflect.DeepEqual(BuildFiles(), []string{"factory", "foreman", "factory-remote", "BUILD"}) {
		t.Error(BuildFiles())
	}
	if pin, err := ToolchainPin([]byte("module x\n\ngo 1.25\n\ntoolchain go1.27.1 // pinned\n")); pin != "go1.27.1" || err != nil {
		t.Errorf("pin %q %v", pin, err)
	}
	for _, bad := range []string{"module x\ngo 1.25\n", "toolchain go\n", "toolchain go1/../x\n"} {
		if pin, err := ToolchainPin([]byte(bad)); err == nil {
			t.Errorf("%q: pin %q", bad, pin)
		}
	}
	for in, want := range map[string]bool{"35956371f2ff": true, "35956371F2FF": false, "35956371f2f": false, "-5956371f2ff": false, "35956371f2ffa": false} {
		if Commit12OK(in) != want {
			t.Errorf("Commit12OK(%q) = %v", in, !want)
		}
	}
}

// An install is one synchronous call: the master's bundle in, a line out.
func TestInstallVerbLimits(t *testing.T) {
	if Timeout(VerbInstall) != 5*time.Minute || OutputCap(VerbInstall) != 1<<20 || MaxInstallInput != 64<<20 {
		t.Errorf("install limits: %v %d %d", Timeout(VerbInstall), OutputCap(VerbInstall), MaxInstallInput)
	}
	if !reflect.DeepEqual(InstallFiles(), []string{"bin/factory-remote", "bin/BUILD", "bin/pi-run", "config.toml"}) {
		t.Error(InstallFiles())
	}
	if !reflect.DeepEqual(InstallDirs(), []string{"systemd", "images/pi", "images/scion-agent"}) {
		t.Error(InstallDirs())
	}
	if AcceptInstalls != "accept-installs" {
		t.Error(AcceptInstalls)
	}
	h := Host{Name: "pi", SSH: "factory@pi", Key: "/k"}
	argv, err := SSHArgv(h, VerbInstall, "35956371f2ff")
	if err != nil || argv[len(argv)-2] != "install" || argv[len(argv)-1] != "35956371f2ff" {
		t.Errorf("install argv: %q %v", argv, err)
	}
}
