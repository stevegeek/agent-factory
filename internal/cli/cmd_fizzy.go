package cli

import (
	"context"
	"fmt"

	"github.com/stevegeek/agent-factory/internal/fizzy"
)

// The Fizzy import (ticket system step 5): `factory fizzy export` on the
// maintainer's desktop, where the `fizzy` CLI holds the credentials, and
// `factory fizzy import` on the dispatcher, into board projects.

// The command loads config.toml itself (no Config: true): export runs on a
// desktop that has none.
func fizzyCommand() Command {
	return Command{Name: "fizzy", Help: "export Fizzy boards with the fizzy CLI, or import an export into board projects",
		Positionals: []Positional{
			{Name: "verb", Choices: []string{"export", "import"}},
			{Name: "dir", Optional: true, Help: "import: the export's directory"},
		},
		Flags: []Flag{
			{Name: "out", Metavar: "DIR", Help: "export: the directory to write (0700; files 0600)"},
			{Name: "board", Kind: RepeatFlag, Metavar: "ID", Help: "export: only this board (repeatable; default every board)"},
			{Name: "fizzy-dir", Metavar: "DIR", Help: "export: run the fizzy CLI here (where its .fizzy.yaml is)"},
			{Name: "map", Kind: RepeatFlag, Metavar: "BOARD_ID=PROJECT", Help: "import: file this board's cards on this board project (repeatable)"},
			{Name: "dry-run", Kind: BoolFlag, Help: "import: say what it would do, write nothing"},
		},
		Run: runFizzy}
}

// runFizzy is `factory fizzy export --out DIR [--board ID]... [--fizzy-dir D]`
// or `factory fizzy import DIR --map BOARD_ID=PROJECT... [--dry-run]`.
func runFizzy(ctx context.Context, e *Env, a Args) error {
	if a.Pos("verb") == "export" {
		if a.Str("out") == "" || a.Pos("dir") != "" || a.Has("map") || a.Bool("dry-run") {
			return Usage("fizzy export takes --out DIR, and --board and --fizzy-dir")
		}
		x := fizzy.Exporter{Exec: e.Exec, Dir: a.Str("fizzy-dir"),
			Progress: func(format string, args ...any) { fmt.Fprintf(e.Stderr, format+"\n", args...) }}
		n, err := x.Export(ctx, a.Str("out"), a.List("board"))
		if err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "wrote %d boards, %d cards, %d comments to %s\n", n.Boards, n.Cards, n.Comments, a.Str("out"))
		return nil
	}
	if a.Pos("dir") == "" || a.Has("out") || a.Has("board") || a.Has("fizzy-dir") {
		return Usage("fizzy import takes DIR, --map BOARD_ID=PROJECT and --dry-run")
	}
	m, err := fizzy.ParseMap(a.List("map"))
	if err != nil {
		return Usage("%s", err.Error())
	}
	cfg, err := e.Config()
	if err != nil {
		return err
	}
	im := fizzy.Importer{Board: e.Board(), Config: cfg, Clock: e.Clock}
	if err := im.Check(m); err != nil {
		return Refuse("%s", err.Error())
	}
	r, err := im.Import(ctx, fizzy.Dump{Dir: a.Pos("dir")}, m, a.Bool("dry-run"))
	if err != nil {
		return err
	}
	fmt.Fprint(e.Stdout, r.Text(a.Bool("dry-run")))
	if a.Bool("dry-run") {
		fmt.Fprintln(e.Stdout, "dry run: nothing written")
	}
	return nil
}
