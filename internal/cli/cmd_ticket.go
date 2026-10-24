package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticketapi"
	"github.com/stevegeek/agent-factory/internal/tokens"
)

// The ticket API and token commands: `factory ticket VERB --json`, which
// Foreman's API and MCP once served by running it, the maintainer's human
// forms of it, and `factory tokens`.

// ticketService is what the ticket commands need of ticketapi.Service.
type ticketService interface {
	Do(ctx context.Context, c ticketapi.Caller, verb string, req jsonx.Value, wait time.Duration) (jsonx.Object, error)
	Call(ctx context.Context, c ticketapi.Caller, verb string, req jsonx.Value, wait time.Duration) (jsonx.Object, error)
}

func (e *Env) ticketAPI() (ticketService, error) {
	if e.ticketService != nil {
		return e.ticketService(e)
	}
	return &ticketapi.Service{Board: e.Board(), Tokens: e.Tokens(),
		Config: e.ConfigSource(), Clock: e.Clock}, nil
}

// ticketUsage is what cmd_ticket says to a human form it does not have.
const ticketUsage = "factory ticket get T0042 | list [--project P] | answer T0042 N TEXT | reindex; " +
	"or any verb with --json and the request on stdin"

// runTicket is `factory ticket VERB [ARGS] [--as N] [--wait S] [--json] [--project P]`.
func runTicket(ctx context.Context, e *Env, a Args) error {
	caller := ticketapi.ForPerson()
	if a.Has("as") {
		caller = ticketapi.ForToken(a.Str("as"))
	}
	wait := lockWait(a.Float("wait"))
	if a.Bool("json") {
		return ticketJSON(ctx, e, a.Pos("verb"), caller, wait)
	}
	if _, err := e.Config(); err != nil {
		return err
	}
	svc, err := e.ticketAPI()
	if err != nil {
		return err
	}
	args := a.Rest()
	switch verb := a.Pos("verb"); {
	case verb == "get" && len(args) == 1:
		out, err := svc.Call(ctx, caller, "get", jsonx.ObjectOf(
			jsonx.Member{Key: "id", Value: jsonx.String(strings.ToUpper(args[0]))},
			jsonx.Member{Key: "format", Value: jsonx.String("md")}), wait)
		if err != nil {
			return err
		}
		md, _ := out.Get("markdown").Str()
		fmt.Fprint(e.Stdout, md)
	case verb == "list" && len(args) == 0:
		var req jsonx.Object
		if p := a.Str("project"); p != "" {
			req = append(req, jsonx.Member{Key: "project", Value: jsonx.String(p)})
		}
		req = append(req, jsonx.Member{Key: "limit", Value: jsonx.Int(200)})
		out, err := svc.Call(ctx, caller, "list", req.Value(), wait)
		if err != nil {
			return err
		}
		list, _ := out.Get("tickets").Array()
		for _, s := range list {
			line := fmt.Sprintf("%s  %s %s %s %s %s", jsonx.PyStr(s.Get("id")), ljust(jsonx.PyStr(s.Get("column")), 9),
				ljust(jsonx.PyStr(s.Get("project")), 14), ljust(jsonx.PyStr(s.Get("kind")), 6),
				ljust(jsonx.PyStr(pyOr(s.Get("assignee"), jsonx.String(""))), 16), pySlice(jsonx.PyStr(s.Get("title")), 60))
			if n := s.Get("open_questions"); jsonx.Truthy(n) {
				line += "  (" + plural(n, "open question") + ")"
			}
			fmt.Fprintln(e.Stdout, line)
		}
	case verb == "answer" && len(args) >= 3 && textx.IsDigits(args[1]):
		n, ok := pyInt(args[1])
		if !ok {
			return Refuse("%s", ticketUsage)
		}
		out, err := svc.Call(ctx, caller, "answer", jsonx.ObjectOf(
			jsonx.Member{Key: "id", Value: jsonx.String(strings.ToUpper(args[0]))},
			jsonx.Member{Key: "n", Value: jsonx.Int(n)},
			jsonx.Member{Key: "answer", Value: jsonx.String(strings.Join(args[2:], " "))}), wait)
		if err != nil {
			return err
		}
		t := out.Get("ticket")
		fmt.Fprintf(e.Stdout, "%s: answered; it is in %s\n", jsonx.PyStr(t.Get("id")), jsonx.PyStr(t.Get("column_label")))
	case verb == "reindex" && len(args) == 0:
		out, err := svc.Call(ctx, caller, "reindex", jsonx.ObjectOf(), wait)
		if err != nil {
			return err
		}
		fmt.Fprintln(e.Stdout, jsonx.PyStr(out.Get("count")))
	default:
		return Refuse("%s", ticketUsage)
	}
	return nil
}

// lockWait is --wait as a lock wait. Python's deadline was now+wait, so a
// negative or NaN wait tried once; ticketapi reads a negative wait as
// "as long as the context allows", so it becomes zero here.
func lockWait(seconds float64) time.Duration {
	switch {
	case math.IsNaN(seconds) || seconds <= 0:
		return 0
	case seconds > float64(math.MaxInt64/int64(time.Second)):
		return -1 // inf: wait for as long as the process lives
	}
	return time.Duration(seconds * float64(time.Second))
}

// ticketJSON is ticket_json: one JSON object in on stdin, one line out on
// stdout. Exit 0 done; 1 refused, with the refusal on stdout; 3 for any
// failure of the factory's own (a config that will not load, an I/O error,
// a crash), with nothing on stdout and the error on stderr.
func ticketJSON(ctx context.Context, e *Env, verb string, caller ticketapi.Caller, wait time.Duration) (err error) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(e.Stderr, "factory: panic: %v\n%s", r, debug.Stack())
			err = Exit(3)
		}
	}()
	crash := func(err error) error {
		fmt.Fprintf(e.Stderr, "factory: %v\n", err)
		return Exit(3)
	}
	if _, err := e.Config(); err != nil {
		return crash(err)
	}
	svc, err := e.ticketAPI()
	if err != nil {
		return crash(err)
	}
	limit := ticketapi.DefaultLimits().Request
	raw, err := io.ReadAll(io.LimitReader(e.Stdin, int64(limit)+1))
	if err != nil {
		return crash(fmt.Errorf("reading the request: %w", err))
	}
	req, err := ticketapi.Request(raw, limit)
	var out jsonx.Object
	if err == nil {
		out, err = svc.Do(ctx, caller, verb, req, wait)
	}
	var ref *ticketapi.Refused
	switch {
	case errors.As(err, &ref):
		e.Stdout.Write(ref.Reply())
		fmt.Fprintln(e.Stdout)
		return Exit(1)
	case err != nil:
		return crash(err)
	}
	e.Stdout.Write(ticketapi.Reply(out))
	fmt.Fprintln(e.Stdout)
	return nil
}

// plural is plural(n, word): "1 open question", "2 open questions".
func plural(n jsonx.Value, word string) string {
	s := jsonx.PyStr(n) + " " + word
	if i, ok := n.Int64(); ok && n.IsInt() && i == 1 {
		return s
	}
	return s + "s"
}

// pyInt is int(s) for a string of Unicode decimal digits (str.isdigit
// strings that are not decimal, such as "²", fail as int() does).
func pyInt(s string) (int64, bool) {
	var b strings.Builder
	for _, r := range s {
		if !textx.IsDecimal(r) {
			return 0, false
		}
		// Unicode's decimal digits come in runs of ten from 0 to 9.
		zero := r
		for textx.IsDecimal(zero - 1) {
			zero--
		}
		b.WriteByte(byte('0' + (r-zero)%10))
	}
	n, err := strconv.ParseInt(b.String(), 10, 64)
	return n, err == nil
}

// runTokens is cmd_tokens: add, list or revoke an API token.
func runTokens(ctx context.Context, e *Env, a Args) error {
	store := e.Tokens()
	verb := a.Pos("verb")
	if verb == "list" {
		if a.Bool("json") {
			rows, err := store.Rows()
			if err != nil {
				return err
			}
			e.Stdout.Write(jsonx.EncodePython(rows, -1))
			fmt.Fprintln(e.Stdout)
			return nil
		}
		lines, err := store.Lines()
		if err != nil {
			return err
		}
		for _, l := range lines {
			fmt.Fprintln(e.Stdout, l)
		}
		return nil
	}
	name := a.Pos("name")
	if name == "" {
		return Refuse("factory tokens %s NAME", verb)
	}
	if verb == "revoke" {
		if err := store.Revoke(ctx, name); err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "revoked %s\n", name)
		return nil
	}
	cfg, _ := e.Config()
	known := make([]string, len(cfg.Projects))
	for i, p := range cfg.Projects {
		known[i] = p.Key
	}
	added, err := store.Add(ctx, tokens.AddIn{Name: name, Projects: a.List("project"), Can: a.Str("can"),
		People: cfg.People(), Known: known, Rand: e.Rand})
	if err != nil {
		return err
	}
	fmt.Fprintln(e.Stdout, added.Token)
	fmt.Fprintln(e.Stderr, added.Note)
	return nil
}
