package ops

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// Lines a look shows: what was asked, clamped as cmd_look clamps it.
const (
	MinLookLines     = 10
	MaxLookLines     = 400
	DefaultLookLines = 120
	// logTail is how much of a local agent's agent.log a look reads when
	// Scion cannot show the screen.
	logTail = 64_000
	// RelayStale is the age at which a relayed screen is no longer shown:
	// the relay writes one every few seconds while Live is open.
	RelayStale = 15 * time.Second
)

// Relayed reads the screens the relay (`factory screens`) writes for a
// process that cannot run Scion itself, Foreman (screens.Reader): an
// agent's screen and when it was written.
type Relayed interface {
	Read(name string) (string, time.Time, error)
}

// Screen is what a ticket's running agent shows now.
type Screen struct {
	Agent ticket.Agent
	Text  string
	// Note is said after the header: a relayed screen's age, or that the
	// screen is starting and the text is the agent's log.
	Note string
}

// String is `factory look`'s output without its last newline: the header
// line "name · stage[ @host] · started X[ · note]", then the screen.
func (s Screen) String() string {
	head := s.Agent.Name + " · " + s.Agent.Stage
	if h := hostOf(s.Agent); h != model.LocalHost {
		head += " @" + h
	}
	head += " · started " + s.Agent.Started
	if s.Note != "" {
		head += " · " + s.Note
	}
	return head + "\n" + s.Text
}

// ErrNoAgent is a look at a ticket with no agent running.
type ErrNoAgent struct{ ID ticket.ID }

func (e *ErrNoAgent) Error() string { return string(e.ID) + " has no agent running" }

// Look reads what a ticket's running agent shows (look): its terminal here
// through Scion, or the end of its agent.log when Scion cannot show it; on
// a helper host, the tail of the job's output. With a Relay (Foreman), a
// local agent's terminal is the screen the relay wrote, and Scion never
// runs. lines is clamped to 10..400. Read-only: nothing here can type into
// an agent.
func (s *Service) Look(ctx context.Context, id string, lines int) (Screen, error) {
	lines = max(MinLookLines, min(lines, MaxLookLines))
	t, err := s.load(id)
	if err != nil {
		return Screen{}, err
	}
	ag, ok := t.Agent.Get()
	if !ok {
		return Screen{}, &ErrNoAgent{ID: t.ID}
	}
	cfg, err := s.config()
	if err != nil {
		return Screen{}, err
	}
	if host := hostOf(ag); host != model.LocalHost {
		h, known := cfg.HostNamed(host)
		out := ""
		if known && s.Jobs != nil {
			if out, err = s.Jobs.Tail(ctx, *h, ag.Name); err != nil {
				out = "(could not read the job on " + host + ": " + err.Error() + ")"
			}
		}
		return Screen{Agent: ag, Text: ScreenText(out, lines)}, nil
	}
	if s.Relay != nil {
		return s.relayed(ag, lines), nil
	}
	if dir := s.workspace(t); dir != "" && s.Scion != nil {
		if out, err := s.Scion.Look(ctx, dir, ag.Name, lines); err == nil && textx.Strip(out) != "" {
			return Screen{Agent: ag, Text: ScreenText(out, lines)}, nil
		}
	}
	return Screen{Agent: ag, Text: ScreenText(s.agentLog(ag.Name), lines)}, nil
}

// relayed is a local agent's screen as the relay last wrote it, with its
// age; when there is none under RelayStale old (the relay starts when Live
// opens), the end of the agent's log, noted as starting.
func (s *Service) relayed(ag ticket.Agent, lines int) Screen {
	if text, at, err := s.Relay.Read(ag.Name); err == nil {
		if age := s.now().Sub(at); age < RelayStale && textx.Strip(text) != "" {
			secs := strconv.Itoa(int(max(age, 0) / time.Second))
			return Screen{Agent: ag, Text: ScreenText(text, lines), Note: "screen " + secs + " s old"}
		}
	}
	return Screen{Agent: ag, Text: ScreenText(s.agentLog(ag.Name), lines), Note: "screen starting…"}
}

// LocalAgent is a running agent here and the workspace Scion runs it in.
type LocalAgent struct {
	Name string
	Dir  string
}

// LocalAgents are the running agents here among ts, each with its
// workspace: what the relay looks at. An agent on another host, or whose
// ticket has no workspace here, is left out.
func (s *Service) LocalAgents(ts []*ticket.Ticket) []LocalAgent {
	var out []LocalAgent
	for _, t := range ts {
		ag, ok := t.Agent.Get()
		if !ok || hostOf(ag) != model.LocalHost {
			continue
		}
		if dir := s.workspace(t); dir != "" {
			out = append(out, LocalAgent{Name: ag.Name, Dir: dir})
		}
	}
	return out
}

// workspace is the directory Scion runs a ticket's agents in (project_for),
// "" for a task with no repository here. Look only reads: it never creates
// the manager's workspace.
func (s *Service) workspace(t *ticket.Ticket) string {
	if t.KindName() == string(model.KindManage) {
		return s.Board.Paths.Manager
	}
	cfg, err := s.config()
	if err != nil {
		return ""
	}
	if p, ok := cfg.Project(t.Project); ok {
		return p.Path
	}
	return ""
}

// agentLog is the last 64 KB of a local agent's agent.log, "" when there is
// none.
func (s *Service) agentLog(name string) string {
	if strings.ContainsAny(name, "/*?[\\") || name == "" || name[0] == '.' {
		return ""
	}
	homes, _ := filepath.Glob(strings.ReplaceAll(s.Board.Paths.AgentHomes, "{name}", name))
	if len(homes) == 0 {
		return ""
	}
	f, err := os.Open(filepath.Join(homes[0], "agent.log"))
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	if off := fi.Size() - logTail; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return ""
		}
	}
	data, _ := io.ReadAll(io.LimitReader(f, logTail))
	return decodeReplace(data)
}

// ScreenText is screen_text: Scion's warning lines dropped, each line's
// trailing whitespace cut, trailing blank lines dropped, and the last lines
// kept.
func ScreenText(text string, lines int) string {
	var keep []string
	for _, line := range textx.SplitLines(text) {
		if scionNoise(line) {
			continue
		}
		keep = append(keep, textx.RStrip(line))
	}
	for len(keep) > 0 && keep[len(keep)-1] == "" {
		keep = keep[:len(keep)-1]
	}
	if len(keep) > lines {
		keep = keep[len(keep)-lines:]
	}
	return strings.Join(keep, "\n")
}

// scionNoise is SCION_NOISE.match(line), a warning Scion prints before the
// screen: r"^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d WARN " (written by hand: the
// pattern ends in a space, which a "// py:" note cannot carry). \d is any
// Unicode decimal digit, as in Python.
func scionNoise(line string) bool {
	const shape = "dddd/dd/dd dd:dd:dd WARN "
	rs := []rune(line)
	if len(rs) < len([]rune(shape)) {
		return false
	}
	for i, want := range []rune(shape) {
		if want == 'd' {
			if !textx.IsDecimal(rs[i]) {
				return false
			}
		} else if rs[i] != want {
			return false
		}
	}
	return true
}

func hostOf(a ticket.Agent) string {
	if a.Host == "" {
		return model.LocalHost
	}
	return a.Host
}

// decodeReplace is bytes.decode(errors="replace"): each maximal invalid
// subsequence becomes one U+FFFD, as Python's UTF-8 decoder does.
func decodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out strings.Builder
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || n > 1 {
			out.WriteRune(r)
			i += n
			continue
		}
		out.WriteRune(utf8.RuneError)
		i += invalidLen(b[i:])
	}
	return out.String()
}

// invalidLen is the length of the maximal subpart of an ill-formed sequence
// at the start of b (Unicode 3.9, table 3-7): a lead byte followed by the
// continuation bytes it allows, one byte at least.
func invalidLen(b []byte) int {
	c := b[0]
	var need int
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		need, lo = 2, 0xA0
	case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
		need = 2
	case c == 0xED:
		need, hi = 2, 0x9F
	case c == 0xF0:
		need, lo = 3, 0x90
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	case c == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1
	}
	n := 1
	for k := 0; k < need && n < len(b); k++ {
		if b[n] < lo || b[n] > hi {
			break
		}
		lo, hi = 0x80, 0xBF
		n++
	}
	return n
}
