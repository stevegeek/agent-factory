package fizzy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/execx"
)

// fakeCLI answers the fizzy CLI's calls from canned JSON, by argv, and
// records each call.
type fakeCLI struct {
	answers map[string]string
	calls   []execx.Cmd
}

func (f *fakeCLI) Run(_ context.Context, c execx.Cmd) (execx.Result, error) {
	f.calls = append(f.calls, c)
	key := strings.Join(c.Argv, " ")
	out, ok := f.answers[key]
	if !ok {
		return execx.Result{Argv: c.Argv, Code: 1, Stderr: []byte("unknown call " + key)}, nil
	}
	return execx.Result{Argv: c.Argv, Stdout: []byte(out)}, nil
}

func cannedCLI() *fakeCLI {
	q := " --quiet"
	return &fakeCLI{answers: map[string]string{
		"fizzy board list --all" + q:                                `[{"id": "b1", "name": "Product"}, {"id": "b2", "name": "Archive"}]`,
		"fizzy column list --board b1" + q:                          `[{"id": "c1", "name": "Next"}]`,
		"fizzy card list --board b1 --all" + q:                      `[{"id": "k1", "number": 1, "title": "One"}, {"id": "k2", "number": 2, "title": "Two"}]`,
		"fizzy card list --board b1 --all --indexed-by closed" + q:  `[{"id": "k3", "number": 3, "title": "Three", "closed": true}, {"id": "k1", "number": 1, "title": "One"}]`,
		"fizzy card list --board b1 --all --indexed-by not_now" + q: ``,
		"fizzy card show 1" + q:                                     `{"id": "k1", "number": 1, "title": "One", "steps": [{"content": "a", "completed": true}]}`,
		"fizzy card show 2" + q:                                     `{"id": "k2", "number": 2, "title": "Two"}`,
		"fizzy card show 3" + q:                                     `{"id": "k3", "number": 3, "title": "Three", "closed": true}`,
		"fizzy comment list --card 1 --all" + q:                     `[{"id": "m1", "body": {"plain_text": "hi"}}]`,
		"fizzy comment list --card 2 --all" + q:                     `[]`,
		"fizzy comment list --card 3 --all" + q:                     `[]`,
	}}
}

func TestExportWritesTheDumpWithTheCLI(t *testing.T) {
	cli := cannedCLI()
	out := filepath.Join(t.TempDir(), "dump")
	n, err := Exporter{Exec: cli, Dir: "/work/fizzy"}.Export(context.Background(), out, []string{"b1"})
	if err != nil {
		t.Fatal(err)
	}
	if n != (Exported{Boards: 1, Cards: 3, Comments: 1}) {
		t.Errorf("exported %+v", n)
	}
	for _, c := range cli.calls {
		if c.Dir != "/work/fizzy" || c.Argv[0] != "fizzy" || c.Argv[len(c.Argv)-1] != "--quiet" || c.Env != nil {
			t.Errorf("call %+v", c)
		}
		if slices.ContainsFunc(c.Argv, func(a string) bool { return strings.Contains(a, "token") }) {
			t.Errorf("a call names a token: %q", c.Argv)
		}
	}
	var boards []Board
	readBack(t, filepath.Join(out, "boards.json"), &boards)
	if len(boards) != 1 || boards[0].ID != "b1" {
		t.Errorf("boards.json = %+v", boards)
	}
	var cards []Card
	readBack(t, filepath.Join(out, "b1", "cards.json"), &cards)
	if len(cards) != 3 || cards[0].ID != "k1" || cards[2].ID != "k3" {
		t.Errorf("cards.json is not the union of the listings by id: %+v", cards)
	}
	var shown Card
	readBack(t, filepath.Join(out, "b1", "cards", "1.json"), &shown)
	if len(shown.Steps) != 1 || !shown.Steps[0].Completed {
		t.Errorf("cards/1.json = %+v", shown)
	}
	var cms []Comment
	readBack(t, filepath.Join(out, "b1", "comments", "1.json"), &cms)
	if len(cms) != 1 || cms[0].Text() != "hi" {
		t.Errorf("comments/1.json = %+v", cms)
	}
	err = filepath.Walk(out, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if fi.IsDir() {
			want = 0o700
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s is %v, want %v", path, fi.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The dump reads back as the import reads it.
	got, comments, err := (Dump{Dir: out}).Cards("b1")
	if err != nil || len(got) != 3 || len(got[0].Steps) != 1 || len(comments[1]) != 1 {
		t.Errorf("Cards = %+v %v %v", got, comments, err)
	}
}

func TestExportRefusesWhatItCannotTrust(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		boards []string
		change func(*fakeCLI)
		want   string
	}{
		"an unknown board":   {[]string{"b9"}, nil, "no board b9"},
		"a board id path":    {[]string{"../b1"}, nil, "not a Fizzy board id"},
		"a CLI failure":      {[]string{"b1"}, func(f *fakeCLI) { delete(f.answers, "fizzy card show 2 --quiet") }, "unknown call"},
		"an answer not JSON": {[]string{"b1"}, func(f *fakeCLI) { f.answers["fizzy card show 2 --quiet"] = "Error: nope" }, "not JSON"},
		"a list not a list":  {[]string{"b1"}, func(f *fakeCLI) { f.answers["fizzy column list --board b1 --quiet"] = `{"a": 1}` }, "not a JSON list"},
		"a card with no id":  {[]string{"b1"}, func(f *fakeCLI) { f.answers["fizzy card list --board b1 --all --quiet"] = `[{"number": 4}]` }, "no usable id"},
		"a card number path": {[]string{"b1"}, func(f *fakeCLI) {
			f.answers["fizzy card list --board b1 --all --quiet"] = `[{"id": "k", "number": -1}]`
		}, "no usable id"},
	} {
		t.Run(name, func(t *testing.T) {
			cli := cannedCLI()
			if c.change != nil {
				c.change(cli)
			}
			_, err := Exporter{Exec: cli}.Export(ctx, filepath.Join(t.TempDir(), "d"), c.boards)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
	big := cannedCLI()
	tooBig := execx.Func(func(ctx context.Context, c execx.Cmd) (execx.Result, error) {
		r, err := big.Run(ctx, c)
		if strings.Contains(strings.Join(c.Argv, " "), "card show 1") {
			r.Truncated = true
		}
		return r, err
	})
	if _, err := (Exporter{Exec: tooBig}).Export(ctx, filepath.Join(t.TempDir(), "d"), nil); err == nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Errorf("a truncated answer: %v", err)
	}
}

func readBack(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}
