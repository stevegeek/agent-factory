package usage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/fsx"
)

// LogName is the log of every run's usage, in the factory root.
const LogName = "usage.jsonl"

// MaxLogRead is how much of the log a reader reads: its end, so the newest
// runs count when it has grown past that. A line is a few hundred bytes,
// so this is tens of thousands of runs. It is streamed a line at a time,
// and only the records in the reader's window are kept.
const MaxLogRead = 16 << 20

// MaxLogLine is the longest line a reader takes; a longer one is skipped.
const MaxLogLine = 64 << 10

// LockWait bounds the wait for another appender.
const LockWait = fsx.Wait(30 * time.Second)

// Record is one finished run's line in usage.jsonl.
type Record struct {
	At      string            `json:"at"` // clock.Stamp of when the run was recorded
	Ticket  string            `json:"ticket"`
	Project string            `json:"project"`
	Stage   string            `json:"stage"`
	Run     string            `json:"run"` // the agent's name, logs/<ticket>/<run>/
	Host    string            `json:"host"`
	Models  map[string]Tokens `json:"models"`
	// Partial says a cap stopped the read of the run's transcript, so its
	// numbers are a lower bound.
	Partial bool `json:"partial,omitempty"`
}

// Sum is every model's tokens of the run together.
func (r Record) Sum() Tokens { return sumModels(r.Models) }

// Append adds r to the log at path as one line, under path's lock
// (<path>.lock), in one write to a file opened for appending, so lines
// from runs that finish together never mix. A log whose last line was cut
// short (a full disk, a crash mid-write) gets its newline first, so only
// that torn line is lost, never the one after it. The log is never
// rewritten.
func Append(path string, r Record) error {
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("usage record: %w", err)
	}
	lock, err := fsx.Lock(context.Background(), path+".lock", LockWait)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return fmt.Errorf("usage log: %w", err)
	}
	torn, err := tornEnd(f)
	if err != nil {
		f.Close()
		return fmt.Errorf("usage log: %w", err)
	}
	if torn {
		line = append([]byte{'\n'}, line...)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("usage log: %w", err)
	}
	return f.Close()
}

// tornEnd reports a file that does not end in a newline: one that is not
// empty, whose last write was cut short.
func tornEnd(f *os.File) (bool, error) {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return false, err
	}
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, st.Size()-1); err != nil {
		return false, err
	}
	return b[0] != '\n', nil
}

// Load is the log's records at or after since, oldest first: its last
// MaxLogRead bytes, read a line at a time, the line the cut lands in
// dropped (a whole one when the cut falls just after a newline), and every
// line that does not parse, is over MaxLogLine, or is from before since
// skipped, with names cleaned and numbers clamped. A run recorded twice (a
// tick cut off between recording a run and saving its ticket finishes it
// again) counts once, as its last line. No log is no records.
func Load(path string, since time.Time) ([]Record, error) { return load(path, MaxLogRead, since) }

func load(path string, max int64, since time.Time) ([]Record, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("usage log: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("usage log: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("usage log %s: %w", path, fsx.ErrNotRegular)
	}
	// from the byte before the cut: the first line read is then the one
	// the cut lands in, or just that newline when it lands on a line's start
	cut := st.Size() > max
	if cut {
		if _, err := f.Seek(st.Size()-max-1, io.SeekStart); err != nil {
			return nil, fmt.Errorf("usage log: %w", err)
		}
	}
	br := bufio.NewReaderSize(io.LimitReader(f, max+1), 64<<10)
	var out []Record
	at := map[[2]string]int{} // (ticket, run) -> its index in out
	first := cut
	err = eachLine(br, MaxLogLine, func(line []byte) {
		if first {
			first = false
			return
		}
		var r Record
		if json.Unmarshal(line, &r) != nil {
			return
		}
		if t, err := clock.Parse(r.At); err != nil || t.Before(since) {
			return
		}
		r.Models = cleanModels(r.Models)
		key := [2]string{r.Ticket, r.Run}
		if i, ok := at[key]; ok && r.Ticket != "" && r.Run != "" {
			out[i] = r
			return
		}
		at[key] = len(out)
		out = append(out, r)
	})
	if err != nil {
		return nil, fmt.Errorf("usage log: %w", err)
	}
	return out, nil
}

// eachLine calls fn with each line br holds, its newline cut; a line over
// max bytes is passed as an empty one, so it still counts as a line.
func eachLine(br *bufio.Reader, max int, fn func(line []byte)) error {
	var buf []byte
	long := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !long {
			if len(buf)+len(chunk) > max+1 {
				long, buf = true, buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(buf) > 0 || long {
			fn(bytes.TrimSuffix(buf, []byte{'\n'}))
		}
		buf, long = buf[:0], false
		if err != nil {
			return nil
		}
	}
}

// The keys Sum groups by.
const (
	ByProject = "project"
	ByStage   = "stage"
	ByTicket  = "ticket"
	ByHost    = "host"
	ByModel   = "model"
)

// Bys are the keys Sum groups by, in the order help lists them.
func Bys() []string { return []string{ByProject, ByStage, ByTicket, ByHost, ByModel} }

// ValidBy reports whether Sum groups by by.
func ValidBy(by string) bool { return slices.Contains(Bys(), by) }

// Row is one group's total: its tokens, the runs that spent them, and
// how many of those runs were read only in part (Record.Partial), which
// makes the tokens a lower bound.
type Row struct {
	Key     string
	Tokens  Tokens
	Runs    int
	Partial int
}

// Sum totals the records at or after since by the key by (one of Bys):
// the rows largest first (ties by key), and the window's total. A record
// whose time does not read is left out; by model, a run counts once in
// each of its models' rows.
func Sum(recs []Record, since time.Time, by string) ([]Row, Tokens) {
	rows := map[string]*Row{}
	var total Tokens
	for _, rec := range Since(recs, since) {
		put := func(key string, t Tokens) {
			if key == "" {
				key = "(none)"
			}
			r := rows[key]
			if r == nil {
				r = &Row{Key: key}
				rows[key] = r
			}
			r.Tokens = r.Tokens.Add(t)
			r.Runs++
			if rec.Partial {
				r.Partial++
			}
		}
		total = total.Add(rec.Sum())
		switch by {
		case ByModel:
			for m, t := range rec.Models {
				put(m, t)
			}
		case ByStage:
			put(rec.Stage, rec.Sum())
		case ByTicket:
			put(rec.Ticket, rec.Sum())
		case ByHost:
			put(rec.Host, rec.Sum())
		default:
			put(rec.Project, rec.Sum())
		}
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	slices.SortFunc(out, func(a, b Row) int {
		if x, y := a.Tokens.Total(), b.Tokens.Total(); x != y {
			if x > y {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	return out, total
}

// Since is the records at or after since, in their order; one whose time
// does not read is left out.
func Since(recs []Record, since time.Time) []Record {
	var out []Record
	for _, r := range recs {
		at, err := clock.Parse(r.At)
		if err != nil || at.Before(since) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// PartialRuns is how many of recs were read only in part.
func PartialRuns(recs []Record) int {
	n := 0
	for _, r := range recs {
		if r.Partial {
			n++
		}
	}
	return n
}

// OfTicket is the records of one ticket, in their order.
func OfTicket(recs []Record, id string) []Record {
	var out []Record
	for _, r := range recs {
		if r.Ticket == id {
			out = append(out, r)
		}
	}
	return out
}

// Commas is n in digits grouped by thousands: 1234567 is "1,234,567".
func Commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
