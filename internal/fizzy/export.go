package fizzy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/execx"
	"github.com/stevegeek/agent-factory/internal/fsx"
)

// Exporter drives the `fizzy` CLI to write a dump. The CLI holds the
// credentials (its own config, or a .fizzy.yaml in Dir): Exporter passes
// none, runs it with argv only and `--quiet` (the API's JSON, no
// envelope), and keeps nothing but what it answers on stdout.
type Exporter struct {
	Exec    execx.Execer
	Program string        // "" is "fizzy", found on PATH
	Dir     string        // the CLI's working directory; "" is this one
	Timeout time.Duration // per call; 0 is 5 minutes
	// Progress, when set, is told each board as it is exported.
	Progress func(format string, args ...any)
}

// Exported counts what an export wrote.
type Exported struct {
	Boards, Cards, Comments int
}

// Export writes the dump of the boards (none: every board) to out:
// boards.json, and for each board <id>/columns.json, <id>/cards.json (the
// union of the open, closed and not-now listings, one entry per id),
// <id>/cards/<n>.json (`card show`, with steps) and
// <id>/comments/<n>.json. Directories are 0700 and files 0600.
func (x Exporter) Export(ctx context.Context, out string, boards []string) (Exported, error) {
	var n Exported
	for _, b := range boards {
		if !ValidID(b) {
			return n, fmt.Errorf("board %q is not a Fizzy board id", b)
		}
	}
	all, err := x.list(ctx, "board", "list", "--all")
	if err != nil {
		return n, err
	}
	var keep []json.RawMessage
	var ids []string
	want := map[string]bool{}
	for _, b := range boards {
		want[b] = true
	}
	for _, raw := range all {
		var b Board
		if err := json.Unmarshal(raw, &b); err != nil || !ValidID(b.ID) {
			return n, fmt.Errorf("fizzy board list: a board with no usable id")
		}
		if len(boards) == 0 || want[b.ID] {
			keep, ids = append(keep, raw), append(ids, b.ID)
			delete(want, b.ID)
		}
	}
	for _, b := range boards {
		if want[b] {
			return n, fmt.Errorf("fizzy has no board %s", b)
		}
	}
	if err := mkdir(out); err != nil {
		return n, err
	}
	if err := write(filepath.Join(out, boardsFile), keep); err != nil {
		return n, err
	}
	for _, id := range ids {
		cards, comments, err := x.board(ctx, out, id)
		if err != nil {
			return n, err
		}
		n.Boards++
		n.Cards += cards
		n.Comments += comments
		if x.Progress != nil {
			x.Progress("board %s: %d cards, %d comments", id, cards, comments)
		}
	}
	return n, nil
}

// board writes one board's files.
func (x Exporter) board(ctx context.Context, out, id string) (cards, comments int, err error) {
	dir := filepath.Join(out, id)
	for _, d := range []string{dir, filepath.Join(dir, cardsDir), filepath.Join(dir, commentsDir)} {
		if err := mkdir(d); err != nil {
			return 0, 0, err
		}
	}
	cols, err := x.list(ctx, "column", "list", "--board", id)
	if err != nil {
		return 0, 0, err
	}
	if err := write(filepath.Join(dir, columnsFile), cols); err != nil {
		return 0, 0, err
	}
	var union []json.RawMessage
	var numbers []int
	seen := map[string]bool{}
	for _, lane := range [][]string{nil, {"--indexed-by", "closed"}, {"--indexed-by", "not_now"}} {
		listed, err := x.list(ctx, append([]string{"card", "list", "--board", id, "--all"}, lane...)...)
		if err != nil {
			return 0, 0, err
		}
		for _, raw := range listed {
			var c Card
			if err := json.Unmarshal(raw, &c); err != nil || !ValidID(c.ID) || !validNumber(c.Number) {
				return 0, 0, fmt.Errorf("board %s: a card with no usable id or number", id)
			}
			if !seen[c.ID] {
				seen[c.ID] = true
				union = append(union, raw)
				numbers = append(numbers, c.Number)
			}
		}
	}
	if len(union) > maxCards {
		return 0, 0, fmt.Errorf("board %s: %d cards, more than %d", id, len(union), maxCards)
	}
	if err := write(filepath.Join(dir, cardsFile), union); err != nil {
		return 0, 0, err
	}
	for _, num := range numbers {
		s := strconv.Itoa(num)
		shown, err := x.run(ctx, "card", "show", s)
		if err != nil {
			return 0, 0, err
		}
		if err := fsx.WriteAtomic(filepath.Join(dir, cardsDir, s+".json"), shown, 0o600); err != nil {
			return 0, 0, err
		}
		cms, err := x.list(ctx, "comment", "list", "--card", s, "--all")
		if err != nil {
			return 0, 0, err
		}
		if err := write(filepath.Join(dir, commentsDir, s+".json"), cms); err != nil {
			return 0, 0, err
		}
		comments += len(cms)
	}
	return len(union), comments, nil
}

// run is one call of the CLI that answers an object: its stdout, JSON of
// at most MaxFile bytes.
func (x Exporter) run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := x.call(ctx, args...)
	if err != nil {
		return nil, err
	}
	if !json.Valid(out) {
		return nil, fmt.Errorf("fizzy %s: the answer is not JSON", strings.Join(args, " "))
	}
	return out, nil
}

// list is a call that answers a list; an empty answer is an empty list.
func (x Exporter) list(ctx context.Context, args ...string) ([]json.RawMessage, error) {
	out, err := x.call(ctx, args...)
	if err != nil {
		return nil, err
	}
	items := []json.RawMessage{}
	if len(out) > 0 && json.Unmarshal(out, &items) != nil {
		return nil, fmt.Errorf("fizzy %s: the answer is not a JSON list", strings.Join(args, " "))
	}
	return items, nil
}

// call runs the CLI with args and --quiet: its stdout, trimmed, refused
// past MaxFile.
func (x Exporter) call(ctx context.Context, args ...string) ([]byte, error) {
	prog := x.Program
	if prog == "" {
		prog = "fizzy"
	}
	timeout := x.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	argv := append(append([]string{prog}, args...), "--quiet")
	what := "fizzy " + strings.Join(args, " ")
	r, err := x.Exec.Run(ctx, execx.Cmd{Argv: argv, Dir: x.Dir, Timeout: timeout, MaxOut: MaxFile + 1})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if err := execx.CheckErr(r); err != nil {
		return nil, err
	}
	if r.Truncated || len(r.Stdout) > MaxFile {
		return nil, fmt.Errorf("%s: the answer is %w", what, ErrTooBig)
	}
	return bytes.TrimSpace(r.Stdout), nil
}

func mkdir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// write writes a list as JSON, one item per line, 0600.
func write(path string, items []json.RawMessage) error {
	var b bytes.Buffer
	b.WriteString("[")
	for i, it := range items {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("\n")
		b.Write(it)
	}
	b.WriteString("\n]\n")
	return fsx.WriteAtomic(path, b.Bytes(), 0o600)
}
