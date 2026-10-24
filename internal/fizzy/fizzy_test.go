package fizzy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
)

func TestPlaceOfMapsFizzyColumns(t *testing.T) {
	for _, c := range []struct {
		column            string
		closed, postponed bool
		want              Place
	}{
		{"", false, false, Place{Column: model.BoardInbox}},
		{"Maybe?", false, false, Place{Column: model.BoardInbox}},
		{"maybe", false, false, Place{Column: model.BoardInbox}},
		{"Next", false, false, Place{Column: model.BoardNext}},
		{"Ideas for later", false, false, Place{Column: model.BoardNext}},
		{"In progress", false, false, Place{Column: model.BoardDoing}},
		{"DOING", false, false, Place{Column: model.BoardDoing}},
		{"Review", false, false, Place{Column: model.BoardDoing, Label: "review"}},
		{"Waiting on vendor", false, false, Place{Column: model.BoardWaiting, Wait: "Waiting on vendor"}},
		{"Blocked: legal", false, false, Place{Column: model.BoardWaiting, Wait: "Blocked: legal"}},
		{"Not Now", false, false, Place{Column: model.BoardWaiting, Wait: NotNow}},
		{"Next", false, true, Place{Column: model.BoardWaiting, Wait: NotNow}},
		{"Done", false, false, Place{Column: model.BoardDone}},
		{"In progress", true, false, Place{Column: model.BoardDone}},
		{"", true, true, Place{Column: model.BoardDone}},
	} {
		if got := PlaceOf(c.column, c.closed, c.postponed); got != c.want {
			t.Errorf("PlaceOf(%q, closed %v, postponed %v) = %+v, want %+v", c.column, c.closed, c.postponed, got, c.want)
		}
	}
}

func TestLabelNormalisesTags(t *testing.T) {
	for tag, want := range map[string]string{
		"Bug": "bug", "UI Polish": "ui-polish", "  --hot!!fix--  ": "hot-fix", "café": "caf", "日本": "",
		strings.Repeat("ab", 20): strings.Repeat("ab", 16), "a" + strings.Repeat("-", 40) + "b": "a-b",
		strings.Repeat("x", 31) + " y": strings.Repeat("x", 31),
	} {
		if got := Label(tag); got != want {
			t.Errorf("Label(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestPlainTextKeepsMarkdown(t *testing.T) {
	got := PlainText(`<h1>Plan</h1><p>Use <strong>this</strong> &amp; <a href="https://x.example/a">that</a>, ` +
		`not <a href="javascript:alert(1)">this</a>.</p><ul><li>one</li><li>two</li></ul><pre>a  b</pre>`)
	want := "## Plan\n\nUse **this** & [that](https://x.example/a), not this.\n\n- one\n- two\n\n```\na  b\n```"
	if got != want {
		t.Errorf("PlainText:\n%q\nwant\n%q", got, want)
	}
}

func TestCleanStripsControlCharacters(t *testing.T) {
	got := clean(" a\x00b\x1b[31mc\r\nd\re\u2028f\u202eg\u0085h\tz\xff ")
	if got != "ab[31mc\nd\ne\nfgh\tz\uFFFD" {
		t.Errorf("clean = %q", got)
	}
	if got := line("one\n two\t three"); got != "one two three" {
		t.Errorf("line = %q", got)
	}
	if got := cutBytes(strings.Repeat("é", 20), 5+len(cutMark)); got != "éé"+cutMark || len(got) > 5+len(cutMark) {
		t.Errorf("cutBytes = %q", got)
	}
}

func TestParseMap(t *testing.T) {
	m, err := ParseMap([]string{"b1=notes", " b2 = other "})
	if err != nil || m["b1"] != "notes" || m["b2"] != "other" {
		t.Fatalf("ParseMap = %v, %v", m, err)
	}
	for _, bad := range [][]string{nil, {"b1"}, {"=p"}, {"../x=p"}, {"b1=p", "b1=q"}} {
		if _, err := ParseMap(bad); err == nil {
			t.Errorf("ParseMap(%q) took it", bad)
		}
	}
}

func TestDumpRefusesBigAndOddFiles(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, MaxFile+1)
	for i := range big {
		big[i] = ' '
	}
	big[0] = '['
	big[len(big)-1] = ']'
	writeFile(t, filepath.Join(dir, boardsFile), string(big))
	if _, err := (Dump{Dir: dir}).Boards(); err == nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Errorf("a big boards.json: %v", err)
	}
	writeFile(t, filepath.Join(dir, "real.json"), `[{"id": "b1"}]`)
	os.Remove(filepath.Join(dir, boardsFile))
	if err := os.Symlink(filepath.Join(dir, "real.json"), filepath.Join(dir, boardsFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := (Dump{Dir: dir}).Boards(); err == nil {
		t.Error("a linked boards.json was read")
	}
	os.Remove(filepath.Join(dir, boardsFile))
	writeFile(t, filepath.Join(dir, boardsFile), `[{"id": "../etc"}]`)
	if _, err := (Dump{Dir: dir}).Boards(); err == nil {
		t.Error("a board id with a path in it was read")
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(b))
}
