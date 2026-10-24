// Package usage is what agent runs spend, in tokens. Claude Code writes a
// session transcript under the agent's home,
// ~/.claude/projects/<dir>/<session>.jsonl (a subagent's one level below),
// and each assistant line carries the message's id, model and usage.
// Read sums those per model, counting each message once with the largest
// of each number it was written with, since a message can be written
// several times while it streams (and a resumed session can repeat it).
// Pi writes no such transcript, so a Pi run reads as no usage.
//
// The home is the agent's to write, so the reader is built for a hostile
// one: no link is followed (a walk that opens every name relative to its
// parent, refusing links), only regular files are read, the names listed,
// files, bytes, line length and distinct messages are capped, a malformed
// line or an overlong id is skipped, every number of a message is clamped
// to what one message can spend (MaxMessageInput, MaxMessageOutput), and
// every sum to [0, MaxTokens].
//
// The record of a run is its usage.json (Usage); every finished run is also
// one line of the factory root's usage.jsonl (Record), which Sum totals.
// Numbers are tokens, never money: the agents run on a subscription.
package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/stevegeek/agent-factory/internal/fsx"
)

// Caps of one Read.
const (
	MaxFiles    = 2000      // transcript files read
	MaxDirs     = 2000      // directories walked under projects/
	MaxBytes    = 512 << 20 // bytes read over all files
	MaxLine     = 8 << 20   // a longer line is skipped
	MaxMessages = 200_000   // distinct message ids
	MaxID       = 256       // a longer message id is skipped
	// MaxDirNames is how many names of one directory are listed, and
	// MaxNames how many over the whole walk: a home of millions of files
	// costs no more than these.
	MaxDirNames = 10_000
	MaxNames    = 50_000
	// MaxDepth is how deep under projects/ a transcript may be:
	// <dir>/<session>.jsonl, <dir>/<session>/subagents/<agent>.jsonl.
	MaxDepth = 4
)

// MaxTokens caps one number of one model's sum in a run (and of one
// usage.json entry): no real run comes near it, and sums of capped numbers
// stay far from overflow (Add saturates all the same).
const MaxTokens = int64(1) << 32

// MaxMessageInput caps the input, cache write and cache read tokens of one
// message, and MaxMessageOutput its output tokens: above any model's
// context window and output limit, so only a forged number is cut.
const (
	MaxMessageInput  = 2_000_000
	MaxMessageOutput = 256_000
)

// MaxModels caps the distinct models of one Usage; the rest are summed
// under Other.
const MaxModels = 32

// MaxModelName is the longest model name kept; Unknown stands for a
// message with none, Other for the models past MaxModels.
const (
	MaxModelName = 64
	Unknown      = "unknown"
	Other        = "other"
)

// synthetic is the model Claude Code names on the messages it makes up
// itself (an error shown as a reply): no model ran, nothing is counted.
const synthetic = "<synthetic>"

// Tokens is what one model spent: input, output, cache write (cache
// creation) and cache read tokens, and the messages that spent them.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheWrite int64 `json:"cache_write"`
	CacheRead  int64 `json:"cache_read"`
	Messages   int64 `json:"messages"`
}

// Total is every token of t: input, output, cache write and cache read.
func (t Tokens) Total() int64 {
	return add(add(t.Input, t.Output), add(t.CacheWrite, t.CacheRead))
}

// Add is t and o summed, field by field, saturating at math.MaxInt64.
func (t Tokens) Add(o Tokens) Tokens {
	return Tokens{add(t.Input, o.Input), add(t.Output, o.Output), add(t.CacheWrite, o.CacheWrite),
		add(t.CacheRead, o.CacheRead), add(t.Messages, o.Messages)}
}

func add(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// clamped is t with every field in [0, MaxTokens].
func (t Tokens) clamped() Tokens {
	c := func(n int64) int64 { return min(max(n, 0), MaxTokens) }
	return Tokens{c(t.Input), c(t.Output), c(t.CacheWrite), c(t.CacheRead), c(t.Messages)}
}

// Usage is one run's usage by model. Partial says a cap stopped the read,
// so the numbers are a lower bound.
type Usage struct {
	Models  map[string]Tokens `json:"models"`
	Partial bool              `json:"partial,omitempty"`
}

// Sum is every model's tokens together.
func (u Usage) Sum() Tokens { return sumModels(u.Models) }

func sumModels(m map[string]Tokens) Tokens {
	var s Tokens
	for _, t := range m {
		s = s.Add(t)
	}
	return s
}

// Empty reports a usage with no message and no token: a Pi run, a run that
// stopped before its first message.
func (u Usage) Empty() bool {
	s := u.Sum()
	return s.Messages == 0 && s.Total() == 0
}

// JSON is usage.json: indented, models in name order, a final newline.
func (u Usage) JSON() []byte {
	if u.Models == nil {
		u.Models = map[string]Tokens{}
	}
	b, _ := json.MarshalIndent(u, "", "  ") // maps and int64s always encode
	return append(b, '\n')
}

// Parse reads a usage.json written elsewhere (a helper's job), cleaned as
// a transcript is: model names cut to what a name may hold, every number
// clamped, at most MaxModels models.
func Parse(data []byte) (Usage, error) {
	var u Usage
	if err := json.Unmarshal(data, &u); err != nil {
		return Usage{}, fmt.Errorf("usage.json: %w", err)
	}
	return Usage{Models: cleanModels(u.Models), Partial: u.Partial}, nil
}

// cleanModels is m with each name cleaned and each entry clamped; names
// that clean to the same one are summed, and the models past MaxModels
// (in name order) go under Other.
func cleanModels(m map[string]Tokens) map[string]Tokens {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	slices.Sort(names)
	out := map[string]Tokens{}
	for _, k := range names {
		name := cleanName(k)
		if _, ok := out[name]; !ok && len(out) >= MaxModels {
			name = Other
		}
		out[name] = out[name].Add(m[k].clamped())
	}
	return out
}

// cleanName keeps the characters a model name has (letters, digits and
// ".-_:@/[]"), at most MaxModelName of them; nothing left is Unknown.
func cleanName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= MaxModelName {
			break
		}
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".-_:@/[]", r)) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return Unknown
	}
	return b.String()
}

// limits are Read's caps, a parameter so tests can make them small.
type limits struct {
	files, dirs, messages, dirNames, names int
	bytes, line                            int64
}

func defaultLimits() limits {
	return limits{files: MaxFiles, dirs: MaxDirs, bytes: MaxBytes, line: MaxLine, messages: MaxMessages,
		dirNames: MaxDirNames, names: MaxNames}
}

// Read is the usage of the Claude Code sessions under home: every regular
// *.jsonl file below home/.claude/projects (MaxDepth deep at most), in name
// order. No home or no transcript is a zero Usage and no error; a cap
// reached stops the read with what was counted and Partial set. Only a home
// that cannot be walked at all is an error.
func Read(home string) (Usage, error) { return read(home, defaultLimits()) }

func read(home string, lim limits) (Usage, error) {
	r := &reader{lim: lim, msgs: map[string]message{}}
	err := fsx.WalkNoFollowMax(home, lim.dirNames, r.dir)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return Usage{}, fmt.Errorf("usage of %s: %w", home, err)
	}
	models := map[string]Tokens{}
	for _, m := range r.msgs {
		t := m.tokens
		t.Messages = 1
		models[m.model] = models[m.model].Add(t)
	}
	return Usage{Models: cleanModels(models), Partial: r.partial}, nil
}

// message is what one message id spent: the largest of each number it
// was written with, and its model.
type message struct {
	model  string
	tokens Tokens
}

type reader struct {
	lim     limits
	msgs    map[string]message
	files   int
	dirs    int
	names   int
	read    int64
	partial bool
}

// dir is the walk's callback: from the home it enters only .claude, from
// there only projects, and below that every directory to MaxDepth, reading
// each *.jsonl file in name order. A listing cut at its cap, or past the
// cap on names over the walk, makes the read partial.
func (r *reader) dir(d *fsx.Dir) error {
	r.names += len(d.Dirs) + len(d.Files)
	if r.names > r.lim.names {
		r.partial = true
		return fs.SkipAll
	}
	if d.Cut {
		r.partial = true
	}
	keep := func(name string) {
		d.Dirs = slices.DeleteFunc(d.Dirs, func(e fsx.Entry) bool { return e.Name != name })
	}
	switch d.Path {
	case ".":
		keep(".claude")
		return nil
	case ".claude":
		keep("projects")
		return nil
	}
	rel, ok := strings.CutPrefix(d.Path, ".claude/projects")
	if !ok || rel != "" && rel[0] != '/' {
		d.Dirs = nil
		return nil
	}
	r.dirs++
	if r.dirs > r.lim.dirs {
		r.partial = true
		return fs.SkipAll
	}
	if strings.Count(rel, "/") >= MaxDepth {
		d.Dirs = nil
	}
	slices.SortFunc(d.Dirs, func(a, b fsx.Entry) int { return strings.Compare(a.Name, b.Name) })
	files := slices.Clone(d.Files)
	slices.SortFunc(files, func(a, b fsx.Entry) int { return strings.Compare(a.Name, b.Name) })
	for _, e := range files {
		if !e.Mode.IsRegular() || filepath.Ext(e.Name) != ".jsonl" {
			continue
		}
		if r.files >= r.lim.files || r.read >= r.lim.bytes {
			r.partial = true
			return fs.SkipAll
		}
		r.files++
		f, err := d.Open(e)
		if err != nil {
			continue // gone, or swapped for something else since the listing
		}
		err = r.file(f)
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// file reads one transcript line by line, within the byte budget left; a
// line over the line cap is skipped whole.
func (r *reader) file(f io.Reader) error {
	left := r.lim.bytes - r.read
	lr := &io.LimitedReader{R: f, N: left}
	br := bufio.NewReaderSize(lr, 64<<10)
	defer func() {
		r.read += left - lr.N
		if lr.N <= 0 {
			r.partial = true
		}
	}()
	var buf []byte
	long := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !long {
			if int64(len(buf)+len(chunk)) > r.lim.line {
				long, buf = true, buf[:0]
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err != nil && !errors.Is(err, io.EOF):
			return nil // a read error ends this file, not the run's usage
		}
		if !long && len(buf) > 0 {
			r.line(buf)
		}
		buf, long = buf[:0], false
		if err != nil {
			return nil
		}
	}
}

// entry is the part of a transcript line that is read. The usage fields
// are raw, so that only a JSON number counts (number).
type entry struct {
	Type    string `json:"type"`
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input      json.RawMessage `json:"input_tokens"`
			Output     json.RawMessage `json:"output_tokens"`
			CacheWrite json.RawMessage `json:"cache_creation_input_tokens"`
			CacheRead  json.RawMessage `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// line counts one transcript line: an assistant message with an id (of
// at most MaxID bytes) and a usage, kept per field at the largest the id
// was written with, so the order the lines are read in does not matter.
// Anything else, or a line that does not parse, is skipped. The model is
// cleaned before it is kept, so nothing the agent wrote is held whole.
func (r *reader) line(b []byte) {
	if !bytes.Contains(b, []byte(`"usage"`)) {
		return
	}
	var e entry
	if json.Unmarshal(b, &e) != nil || e.Type != "assistant" || e.Message == nil || e.Message.ID == "" || e.Message.Usage == nil {
		return
	}
	if e.Message.Model == synthetic || len(e.Message.ID) > MaxID {
		return
	}
	prev, seen := r.msgs[e.Message.ID]
	if !seen && len(r.msgs) >= r.lim.messages {
		r.partial = true
		return
	}
	u := e.Message.Usage
	t := Tokens{Input: number(u.Input, MaxMessageInput), Output: number(u.Output, MaxMessageOutput),
		CacheWrite: number(u.CacheWrite, MaxMessageInput), CacheRead: number(u.CacheRead, MaxMessageInput)}
	m := message{model: cleanName(e.Message.Model), tokens: Tokens{Input: max(prev.tokens.Input, t.Input),
		Output: max(prev.tokens.Output, t.Output), CacheWrite: max(prev.tokens.CacheWrite, t.CacheWrite),
		CacheRead: max(prev.tokens.CacheRead, t.CacheRead)}}
	if seen && m.model == Unknown {
		m.model = prev.model // a later line without its model
	}
	r.msgs[strings.Clone(e.Message.ID)] = m
}

// number is a token count from raw JSON: a JSON number, its whole part,
// clamped to [0, most]; anything else (a string, null, missing) is 0.
func number(raw json.RawMessage, most int64) int64 {
	s := strings.TrimSpace(string(raw))
	if s == "" || !(s[0] == '-' || s[0] >= '0' && s[0] <= '9') {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return min(max(n, 0), most)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0
	}
	if f != f || f <= 0 {
		return 0
	}
	if f >= float64(most) {
		return most
	}
	return int64(f)
}
