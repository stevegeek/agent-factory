// Package fizzy moves a maintainer's boards from Fizzy to board
// projects, in two halves that run on different machines:
//
//   - Export, on a workstation, drives the `fizzy` CLI (which
//     holds the credentials: this package never sees or prints a token)
//     and writes what it answers to a dump directory.
//   - Import, on the dispatcher, reads a dump and files or refreshes one
//     board ticket per card through the store, with the same locks as
//     every other ticket write.
//
// A dump is the maintainer's own data, but its text was written by people
// and integrations on another tracker and will be shown to agents: it is
// read with caps, cleaned of control characters, cut to the ticket API's
// limits and filed through the untrusted import door.
package fizzy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
)

// MaxFile is the most a dump file may hold, in bytes.
const MaxFile = 8 << 20

// From is what an imported ticket's record names as its tracker.
const From = "fizzy"

// The files of a dump, under its directory.
const (
	boardsFile   = "boards.json"
	columnsFile  = "columns.json"
	cardsFile    = "cards.json"
	cardsDir     = "cards"
	commentsDir  = "comments"
	maxCards     = 10_000
	maxComments  = 5_000
	maxSteps     = 500
	maxIDLength  = 64
	maxCardNum   = 1_000_000_000
	maxPersonLen = 64
)

// Board is a Fizzy board as the API answers it.
type Board struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Column is one of a board's columns.
type Column struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Person is a card's or a comment's creator, or an assignee.
type Person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Ref is a reference by id, with the name when the API gives one.
type Ref struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Step is one of a card's steps (only `card show` has them).
type Step struct {
	Content   string `json:"content"`
	Completed bool   `json:"completed"`
}

// Card is a Fizzy card. Fields the import does not use are ignored.
type Card struct {
	ID              string          `json:"id"`
	Number          int             `json:"number"`
	Title           string          `json:"title"`
	Description     string          `json:"description"`
	DescriptionHTML string          `json:"description_html"`
	Status          string          `json:"status"`
	Closed          bool            `json:"closed"`
	Postponed       bool            `json:"postponed"`
	Golden          bool            `json:"golden"`
	CreatedAt       string          `json:"created_at"`
	URL             string          `json:"url"`
	Board           *Ref            `json:"board"`
	Column          *Ref            `json:"column"`
	Creator         *Person         `json:"creator"`
	Assignees       []Person        `json:"assignees"`
	Tags            json.RawMessage `json:"tags"`
	Steps           []Step          `json:"steps"`
}

// Comment is a comment on a card. Its body is {plain_text, html}, or a
// string in an older shape.
type Comment struct {
	ID        string          `json:"id"`
	Body      json.RawMessage `json:"body"`
	Creator   *Person         `json:"creator"`
	CreatedAt string          `json:"created_at"`
}

// Text is the comment's text: the plain text, else the HTML as text.
func (c Comment) Text() string {
	var b struct {
		PlainText string `json:"plain_text"`
		HTML      string `json:"html"`
	}
	if json.Unmarshal(c.Body, &b) == nil {
		if b.PlainText != "" {
			return b.PlainText
		}
		return PlainText(b.HTML)
	}
	var s string
	if json.Unmarshal(c.Body, &s) == nil {
		return s
	}
	return ""
}

// TagNames are a card's tags: names, or objects with a title or a name.
func (c Card) TagNames() []string {
	var names []string
	if json.Unmarshal(c.Tags, &names) == nil {
		return names
	}
	var objs []struct {
		Title string `json:"title"`
		Name  string `json:"name"`
	}
	_ = json.Unmarshal(c.Tags, &objs)
	for _, o := range objs {
		switch {
		case o.Title != "":
			names = append(names, o.Title)
		case o.Name != "":
			names = append(names, o.Name)
		}
	}
	return names
}

// ValidID reports an id a dump may use as a file name: 1 to 64 of
// A-Z a-z 0-9 _ -.
func ValidID(s string) bool {
	if len(s) == 0 || len(s) > maxIDLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validNumber(n int) bool { return n >= 1 && n <= maxCardNum }

// ErrTooBig is a dump file over MaxFile.
var ErrTooBig = errors.New("over 8 MiB")

// readFile reads one dump file: a regular file (not a link) of at most
// MaxFile bytes. A missing file is fs.ErrNotExist.
func readFile(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	if fi.Size() > MaxFile {
		return nil, fmt.Errorf("%s: %w", path, ErrTooBig)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFile+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) > MaxFile {
		return nil, fmt.Errorf("%s: %w", path, ErrTooBig)
	}
	return data, nil
}

// readJSON reads a dump file into v.
func readJSON(path string, v any) error {
	data, err := readFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Dump is a dump directory as the import reads it.
type Dump struct{ Dir string }

// Boards are the boards of boards.json.
func (d Dump) Boards() ([]Board, error) {
	var bs []Board
	if err := readJSON(filepath.Join(d.Dir, boardsFile), &bs); err != nil {
		return nil, err
	}
	for _, b := range bs {
		if !ValidID(b.ID) {
			return nil, fmt.Errorf("%s: board id %q is not one an export writes", boardsFile, b.ID)
		}
	}
	return bs, nil
}

// Columns are a board's columns by id; a board with no columns.json has
// none (its cards carry their column's name).
func (d Dump) Columns(board string) (map[string]string, error) {
	var cs []Column
	err := readJSON(filepath.Join(d.Dir, board, columnsFile), &cs)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, c := range cs {
		out[c.ID] = c.Name
	}
	return out, nil
}

// Cards are a board's cards with their comments, by number: each from
// cards/<n>.json when the export showed it (with its steps), else from
// cards.json. A card of another board, or with an id or number no export
// writes, is refused.
func (d Dump) Cards(board string) ([]Card, map[int][]Comment, error) {
	var listed []Card
	if err := readJSON(filepath.Join(d.Dir, board, cardsFile), &listed); err != nil {
		return nil, nil, err
	}
	if len(listed) > maxCards {
		return nil, nil, fmt.Errorf("board %s: %d cards, more than %d", board, len(listed), maxCards)
	}
	seen := map[string]bool{}
	var cards []Card
	comments := map[int][]Comment{}
	for _, c := range listed {
		if !ValidID(c.ID) || !validNumber(c.Number) {
			return nil, nil, fmt.Errorf("board %s: card %q #%d is not one an export writes", board, c.ID, c.Number)
		}
		if seen[c.ID] || c.Board != nil && c.Board.ID != "" && c.Board.ID != board {
			continue
		}
		seen[c.ID] = true
		n := strconv.Itoa(c.Number)
		var shown Card
		switch err := readJSON(filepath.Join(d.Dir, board, cardsDir, n+".json"), &shown); {
		case err == nil && shown.ID == c.ID:
			c = shown
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return nil, nil, err
		}
		if len(c.Steps) > maxSteps {
			return nil, nil, fmt.Errorf("card #%d: %d steps, more than %d", c.Number, len(c.Steps), maxSteps)
		}
		var cms []Comment
		switch err := readJSON(filepath.Join(d.Dir, board, commentsDir, n+".json"), &cms); {
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return nil, nil, err
		case len(cms) > maxComments:
			return nil, nil, fmt.Errorf("card #%d: %d comments, more than %d", c.Number, len(cms), maxComments)
		}
		comments[c.Number] = cms
		cards = append(cards, c)
	}
	return cards, comments, nil
}
