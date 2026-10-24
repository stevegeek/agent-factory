package jobproto

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strings"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// Meta is a job's meta.json: what the caller may choose about a run. The
// helper's own config decides the image, memory, mounts and environment.
type Meta struct {
	Branch         string // the branch the agent works on (a stage name for no-branch stages)
	Start          string // where it starts: the base, the branch, a task's ref, or "none"
	Model          string // "" in what the dispatcher sends means the helper's default
	Bundle         bool   // send the branch back as out.bundle (work only)
	TimeoutMinutes int
	// Harness picks the command the job runs (§2.4.5). The dispatcher
	// leaves it out, which means DefaultHarness; Python helpers ignore it.
	Harness string
	// Kind is "" for an agent's run (KindAgent reads as ""), or KindGate
	// for Gate 1's checks on the commit in Start (a full hex commit id),
	// with the commands in MemberChecks.
	Kind string
}

// Encode is the meta.json the dispatcher sends (start_remote, F:899):
// json.dumps of {branch, start, model, bundle, timeout_minutes}, with
// "harness" after them only when it is not the default, and "kind" only
// for a gate job.
func (m Meta) Encode() []byte {
	o := jsonx.Object{
		{Key: "branch", Value: jsonx.String(m.Branch)},
		{Key: "start", Value: jsonx.String(m.Start)},
		{Key: "model", Value: jsonx.String(m.Model)},
		{Key: "bundle", Value: jsonx.Bool(m.Bundle)},
		{Key: "timeout_minutes", Value: jsonx.Int(int64(m.TimeoutMinutes))},
	}
	if m.Harness != "" && m.Harness != DefaultHarness {
		o = append(o, jsonx.Member{Key: "harness", Value: jsonx.String(m.Harness)})
	}
	if m.Kind != "" && m.Kind != KindAgent {
		o = append(o, jsonx.Member{Key: "kind", Value: jsonx.String(m.Kind)})
	}
	return jsonx.EncodePython(o.Value(), -1)
}

// commitID is a full SHA-1 or SHA-256 commit id, lowercase.
var commitID = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?\z`) // re: go

// py: ^[A-Za-z0-9._/-]{1,100}$
var branchName = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}\z`)

// py: ^[A-Za-z0-9._:/-]{1,80}$
var modelName = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,80}\z`)

// ErrNotObject is a meta.json that is not a JSON object.
var ErrNotObject = errors.New("meta.json is not an object")

// ParseMeta reads a meta.json and checks it (CheckMeta).
func ParseMeta(b []byte) (Meta, error) {
	v, err := jsonx.Parse(b)
	if err != nil {
		return Meta{}, fmt.Errorf("meta.json: %w", err)
	}
	return CheckMeta(v)
}

// CheckMeta is check_meta (R:72), the helper's validation of a job's
// meta, as the shared definition of what a dispatcher may send:
//   - branch and start (str() of the value, "" when absent) match
//     ^[A-Za-z0-9._/-]{1,100}$, hold no "..", and start with neither "-"
//     nor "/";
//   - the model is str(model or DefaultModel), ^[A-Za-z0-9._:/-]{1,80}$,
//     not starting with "-";
//   - timeout_minutes is int() of the value clamped to 1..120, and 45 when
//     int() refuses it; bundle is the value's truth.
//
// Two differences from Python, both refusals of what no dispatcher sends:
// a trailing newline is refused where Python's `$` lets it through, and a
// timeout int() would overflow on (an infinite float) is an error here and
// a crash there. A harness, when present, must be a plain lowercase name;
// the helper refuses one it has no entry for. A kind, when present, is
// KindAgent or KindGate, and a gate job's start is a full commit id.
func CheckMeta(v jsonx.Value) (Meta, error) {
	o, ok := v.Object()
	if !ok {
		return Meta{}, ErrNotObject
	}
	var m Meta
	for _, k := range []string{"branch", "start"} {
		s := ""
		if val, ok := o.Lookup(k); ok {
			s = jsonx.PyStr(val)
		}
		if !branchName.MatchString(s) || strings.Contains(s, "..") || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") {
			return Meta{}, fmt.Errorf("bad %s in meta.json", k)
		}
		if k == "branch" {
			m.Branch = s
		} else {
			m.Start = s
		}
	}
	m.Model = DefaultModel
	if val := o.Get("model"); jsonx.Truthy(val) {
		m.Model = jsonx.PyStr(val)
	}
	if !modelName.MatchString(m.Model) || strings.HasPrefix(m.Model, "-") {
		return Meta{}, errors.New("bad model in meta.json")
	}
	m.TimeoutMinutes = DefaultTimeoutMinutes
	if val, ok := o.Lookup("timeout_minutes"); ok {
		n, err := pyInt(val)
		switch {
		case errors.Is(err, errOverflow):
			return Meta{}, errors.New("bad timeout_minutes in meta.json")
		case err == nil:
			m.TimeoutMinutes = clampMinutes(n)
		}
	}
	m.Bundle = jsonx.Truthy(o.Get("bundle"))
	m.Harness = DefaultHarness
	if val := o.Get("harness"); jsonx.Truthy(val) {
		h := jsonx.PyStr(val)
		if !harnessOK(h) {
			return Meta{}, errors.New("bad harness in meta.json")
		}
		m.Harness = h
	}
	if val, ok := o.Lookup("kind"); ok {
		switch k := jsonx.PyStr(val); k {
		case KindAgent:
		case KindGate:
			m.Kind = k
		default:
			return Meta{}, errors.New("bad kind in meta.json")
		}
	}
	if m.Kind == KindGate && !commitID.MatchString(m.Start) {
		return Meta{}, errors.New("bad start in meta.json: a gate job starts from a commit id")
	}
	return m, nil
}

func harnessOK(h string) bool {
	if h == "" || len(h) > 32 || h[0] < 'a' || h[0] > 'z' {
		return false
	}
	for i := 1; i < len(h); i++ {
		c := h[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// clampMinutes is min(max(n, 1), 120) on Python's unbounded int.
func clampMinutes(n *big.Int) int {
	switch {
	case n.Cmp(big.NewInt(1)) < 0:
		return 1
	case n.Cmp(big.NewInt(MaxTimeoutMinutes)) > 0:
		return MaxTimeoutMinutes
	}
	return int(n.Int64())
}

var (
	errNotInt   = errors.New("not an int")
	errOverflow = errors.New("int() of an infinite float")
)

// pyInt is Python int(v) for a decoded JSON value: bools are 0 and 1,
// ints are themselves, floats are truncated (NaN refused, infinity an
// overflow), and strings are read as int(str) does (surrounding
// whitespace, a sign, Unicode decimal digits with single underscores
// between them). null, arrays and objects are refused.
func pyInt(v jsonx.Value) (*big.Int, error) {
	switch v.Kind() {
	case jsonx.KindBool:
		b, _ := v.Bool()
		if b {
			return big.NewInt(1), nil
		}
		return big.NewInt(0), nil
	case jsonx.KindNumber:
		lit, _ := v.Number()
		if v.IsInt() {
			n, ok := new(big.Int).SetString(lit, 10)
			if !ok {
				return nil, errNotInt
			}
			return n, nil
		}
		f, ok := v.Float64()
		switch {
		case !ok || math.IsNaN(f):
			return nil, errNotInt
		case math.IsInf(f, 0):
			return nil, errOverflow
		}
		n, _ := big.NewFloat(math.Trunc(f)).Int(nil)
		return n, nil
	case jsonx.KindString:
		s, _ := v.Str()
		return intFromString(s)
	}
	return nil, errNotInt
}

func intFromString(s string) (*big.Int, error) {
	s = textx.Strip(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	var digits strings.Builder
	prevDigit := false
	for _, r := range s {
		switch {
		case r == '_' && prevDigit:
			prevDigit = false
		case textx.IsDecimal(r):
			d, ok := decimalValue(r)
			if !ok {
				return nil, errNotInt
			}
			digits.WriteByte(byte('0' + d))
			prevDigit = true
		default:
			return nil, errNotInt
		}
	}
	if digits.Len() == 0 || !prevDigit {
		return nil, errNotInt
	}
	n, ok := new(big.Int).SetString(digits.String(), 10)
	if !ok {
		return nil, errNotInt
	}
	if neg {
		n.Neg(n)
	}
	return n, nil
}

// decimalValue is the digit a Unicode decimal (Nd) character stands for.
// Unicode keeps every Nd character in runs of ten from zero to nine, some
// runs back to back (the mathematical digits), so the value is the
// distance from the start of its contiguous run, modulo ten.
func decimalValue(r rune) (int, bool) {
	if !textx.IsDecimal(r) {
		return 0, false
	}
	start := r
	for textx.IsDecimal(start - 1) {
		start--
	}
	return int((r - start) % 10), true
}
