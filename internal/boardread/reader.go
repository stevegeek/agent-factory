// Package boardread reads the board: ticket files, the index, Foreman's
// change-aware snapshot and the board checks of `factory check --board` and
// `--asks`. No write method exists in this package; package store writes.
//
// Every reader goes through it, so this decision lives in one place:
// a ticket file that will not read is skipped and logged (once per state of
// the file), never fatal, where Python's all_tickets crashed the whole tick.
package boardread

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// MaxTicketBytes caps one ticket file read. The ticket limits keep a real
// ticket far below it; a larger file is unreadable, not a memory blow-up.
const MaxTicketBytes = 64 << 20

var (
	// ErrBadID is an id that is not "T" and 4 to 6 digits: no path is built
	// from it.
	ErrBadID = errors.New("not a ticket id")
	// ErrUnreadable is a ticket file that is not a regular file, not UTF-8,
	// not JSON or not a JSON object.
	ErrUnreadable = errors.New("ticket file cannot be read")
)

// Logger is where a Reader reports the files it skipped (*logx.Log).
type Logger interface {
	Printf(format string, args ...any)
}

// Reader reads the board under Paths. Log may be nil. A Reader is safe for
// concurrent use; use it by pointer (it remembers what it has logged).
type Reader struct {
	Paths paths.Paths
	Log   Logger

	mu     sync.Mutex
	logged map[string]Stat // file -> the state whose problem was logged
}

// Problem is a ticket file that did not read cleanly.
type Problem struct {
	File string    // the file's path
	ID   ticket.ID // the id its name gives, "" if the name is not an id
	// Err is set when the file could not be read as a ticket at all: it
	// was skipped.
	Err error
	// Fields are values of the wrong type, read as empty and kept in the
	// file as they are.
	Fields []ticket.Problem
	// Note is anything else worth saying (an id that is not the file's).
	Note string
}

func (p Problem) String() string {
	name := filepath.Base(p.File)
	switch {
	case p.Err != nil:
		return fmt.Sprintf("%s: %v", name, p.Err)
	case len(p.Fields) > 0:
		parts := make([]string, len(p.Fields))
		for i, f := range p.Fields {
			parts[i] = f.String()
		}
		s := name + ": fields of an unexpected type read as empty: " + strings.Join(parts, "; ")
		if p.Note != "" {
			s += "; " + p.Note
		}
		return s
	}
	return name + ": " + p.Note
}

// TicketFileName is the id of a ticket file's name, re.fullmatch(
// r"(T\d{4,6})\.json", name) as ticket_index matches it (\d is any Unicode
// decimal digit, as in Python).
func TicketFileName(name string) (ticket.ID, bool) {
	stem, ok := strings.CutSuffix(name, ".json")
	if !ok || !strings.HasPrefix(stem, "T") {
		return "", false
	}
	n := 0
	for _, r := range stem[1:] {
		if !unicode.Is(unicode.Nd, r) { // re's \d: the Unicode decimals
			return "", false
		}
		n++
	}
	if n < 4 || n > 6 {
		return "", false
	}
	return ticket.ID(stem), true
}

// globbed is a name glob("T*.json") gives: what all_tickets reads.
func globbed(name string) bool {
	return strings.HasPrefix(name, "T") && strings.HasSuffix(name, ".json")
}

// Load reads one ticket (load_ticket): the id is upper-cased first, and
// anything that is not an id is ErrBadID. A missing file is fs.ErrNotExist;
// one that will not read is ErrUnreadable. Wrong-typed fields are logged,
// once, and read as empty.
func (r *Reader) Load(id ticket.ID) (*ticket.Ticket, error) {
	clean, ok := ticket.ParseID(string(id))
	if !ok {
		return nil, fmt.Errorf("load %q: %w", string(id), ErrBadID)
	}
	path := r.Paths.TicketFile(string(clean))
	t, p, err := r.read(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("load %s: %w", clean, err)
		}
		r.report(path, p)
		return nil, fmt.Errorf("load %s: %w", clean, p.Err)
	}
	r.report(path, p)
	return t, nil
}

// read reads and decodes one file. A missing file is returned as the error
// (fs.ErrNotExist); every other failure is in the Problem, whose Err is
// also returned.
func (r *Reader) read(path string) (*ticket.Ticket, Problem, error) {
	p := Problem{File: path}
	p.ID, _ = TicketFileName(filepath.Base(path))
	data, err := readFile(path, MaxTicketBytes)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, p, err
		}
		p.Err = fmt.Errorf("%w: %v", ErrUnreadable, err)
		return nil, p, p.Err
	}
	t, err := decode(data)
	if err != nil {
		p.Err = err
		return nil, p, err
	}
	p.Fields = t.problems
	return t.Ticket, p, nil
}

type decoded struct {
	*ticket.Ticket
	problems []ticket.Problem
}

// decode is json.load as Python's text-mode open reads it: the bytes must
// be UTF-8 (escaped lone surrogates are fine), the JSON an object.
func decode(data []byte) (decoded, error) {
	if !utf8.Valid(data) {
		return decoded{}, fmt.Errorf("%w: not UTF-8", ErrUnreadable)
	}
	t, problems, err := ticket.Decode(data)
	if err != nil {
		return decoded{}, fmt.Errorf("%w: %v", ErrUnreadable, err)
	}
	return decoded{t, problems}, nil
}

// readFile reads a regular file without following a link in its last
// component.
func readFile(path string, max int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ELOOP {
			return nil, errors.New("a link, not a file")
		}
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("over %d bytes", max)
	}
	return b, nil
}

// List reads every ticket, in Python's sorted(glob("T*.json")) order. A
// file that cannot be read is skipped; it is in the problems and logged
// once per state of the file. So is a ticket with wrong-typed fields, which
// is still listed. A missing board is empty.
func (r *Reader) List() ([]*ticket.Ticket, []Problem) {
	files, err := r.files()
	if err != nil {
		p := Problem{File: r.Paths.Tickets, Err: err}
		r.report(p.File, p)
		return nil, []Problem{p}
	}
	var out []*ticket.Ticket
	var problems []Problem
	for _, f := range files {
		if f.p.Err != nil || len(f.p.Fields) > 0 {
			problems = append(problems, f.p)
		}
		if f.t != nil {
			out = append(out, f.t)
		}
	}
	return out, problems
}

// file is one T*.json as List read it: its ticket, or nil and why not.
type file struct {
	name string
	t    *ticket.Ticket
	p    Problem
}

// files reads every T*.json in sorted order, reporting each problem.
func (r *Reader) files() ([]file, error) {
	names, err := r.names()
	if err != nil {
		return nil, err
	}
	out := make([]file, 0, len(names))
	for _, name := range names {
		path := filepath.Join(r.Paths.Tickets, name)
		t, p, err := r.read(path)
		if err != nil && errors.Is(err, fs.ErrNotExist) {
			continue // gone since the listing
		}
		r.report(path, p)
		out = append(out, file{name: name, t: t, p: p})
	}
	return out, nil
}

// names are the board's T*.json names, sorted.
func (r *Reader) names() ([]string, error) {
	entries, err := os.ReadDir(r.Paths.Tickets)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("list the board: %w", err)
	}
	var out []string
	for _, e := range entries {
		if globbed(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// report logs a problem once per state of its file.
func (r *Reader) report(path string, p Problem) {
	if r.Log == nil || (p.Err == nil && len(p.Fields) == 0) {
		return
	}
	st, _ := lstat(path)
	r.mu.Lock()
	if r.logged == nil {
		r.logged = map[string]Stat{}
	}
	prev, seen := r.logged[path]
	r.logged[path] = st
	r.mu.Unlock()
	if seen && prev == st {
		return
	}
	r.Log.Printf("board: %s", p)
}

func lstat(path string) (Stat, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Stat{}, err
	}
	return StatOf(fi), nil
}
