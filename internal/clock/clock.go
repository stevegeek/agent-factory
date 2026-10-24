// Package clock is time for the factory: the clock interface every package
// receives, the factory's stamp format and a parser for the stamps Python
// writes and reads (ARCHITECTURE.md §2.3).
package clock

import (
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

// Clock tells the time. Production code takes one; tests pass Fixed or Step.
type Clock interface {
	Now() time.Time
}

// System is the real clock, in UTC.
type System struct{}

// Now returns the current time in UTC.
func (System) Now() time.Time { return time.Now().UTC() }

// Fixed always says T.
type Fixed struct{ T time.Time }

// Now returns T.
func (f Fixed) Now() time.Time { return f.T }

// Step starts at a time and moves on by a fixed step after every call. It
// is safe for concurrent use.
type Step struct {
	mu   sync.Mutex
	next time.Time
	step time.Duration
}

// NewStep returns a clock whose first Now is start.
func NewStep(start time.Time, step time.Duration) *Step {
	return &Step{next: start, step: step}
}

// Now returns the current time and advances the clock by one step.
func (s *Step) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.next
	s.next = s.next.Add(s.step)
	return t
}

// Advance moves the clock on by d without a call to Now.
func (s *Step) Advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = s.next.Add(d)
}

// Layout is the factory's stamp: UTC, whole seconds, "+00:00", never "Z".
const Layout = "2006-01-02T15:04:05+00:00"

// Stamp is Python's now() (F:201) for t: utcnow().replace(microsecond=0)
// .isoformat(). Fractions are truncated, not rounded.
func Stamp(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(Layout)
}

// FromTestNow is the clock for a FACTORY_TEST_NOW value (seam 3, §10.5):
// empty gives System; otherwise the stamp, read as Python's utcnow() reads
// it (naive means UTC), fixes the clock.
func FromTestNow(value string) (Clock, error) {
	if value == "" {
		return System{}, nil
	}
	t, err := Parse(value)
	if err != nil {
		return nil, fmt.Errorf("FACTORY_TEST_NOW: %w", err)
	}
	return Fixed{T: t}, nil
}

// ErrSyntax is wrapped by every parse error.
var ErrSyntax = errors.New("not an ISO 8601 time")

// Parse reads a time as Python's datetime.fromisoformat does and returns it
// in UTC; a time without an offset is taken as UTC (as the FACTORY_TEST_NOW
// seam does), not as local time.
func Parse(s string) (time.Time, error) {
	t, _, err := ParseISO(s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// ParseISO is datetime.fromisoformat (Python 3.11+): a date (YYYY-MM-DD,
// YYYYMMDD, YYYY-Www[-D], YYYYWww[D]), optionally followed by any one
// character and a time (HH[:MM[:SS[.f]]] or HHMM[SS[.f]], the fraction
// after "." or "," with any number of digits, cut to microseconds) and an
// offset ("Z", ±HH, ±HH:MM, ±HHMM, ±HH:MM:SS[.f]). 24:00:00 is the next
// midnight. aware reports whether an offset was given; a naive time is
// returned in UTC.
func ParseISO(s string) (t time.Time, aware bool, err error) {
	fail := func(why string) (time.Time, bool, error) {
		return time.Time{}, false, fmt.Errorf("%w: %q: %s", ErrSyntax, s, why)
	}
	p := isoParser{s: s}
	y, m, d, ok := p.date()
	if !ok {
		return fail("bad date")
	}
	if p.i == len(s) {
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), false, nil
	}
	// The separator: any one character.
	_, size := utf8.DecodeRuneInString(s[p.i:])
	p.i += size
	hh, mm, ss, us, ok := p.clock()
	if !ok {
		return fail("bad time")
	}
	loc := time.UTC
	if p.i < len(s) {
		off, ok := p.offset()
		if !ok || p.i != len(s) {
			return fail("bad offset")
		}
		aware = true
		if off != 0 {
			loc = time.FixedZone("", off)
		}
	}
	next := false
	if hh == 24 {
		if mm != 0 || ss != 0 || us != 0 {
			return fail("hour 24 with minutes")
		}
		hh, next = 0, true
	}
	t = time.Date(y, m, d, hh, mm, ss, us*1000, loc)
	if next {
		t = t.AddDate(0, 0, 1)
	}
	return t, aware, nil
}

type isoParser struct {
	s string
	i int
}

func (p *isoParser) digits(n int) (int, bool) {
	if p.i+n > len(p.s) {
		return 0, false
	}
	v := 0
	for _, c := range []byte(p.s[p.i : p.i+n]) {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int(c-'0')
	}
	p.i += n
	return v, true
}

func (p *isoParser) peek(c byte) bool { return p.i < len(p.s) && p.s[p.i] == c }

func (p *isoParser) eat(c byte) bool {
	if p.peek(c) {
		p.i++
		return true
	}
	return false
}

func (p *isoParser) date() (int, time.Month, int, bool) {
	y, ok := p.digits(4)
	if !ok || y < 1 {
		return 0, 0, 0, false
	}
	ext := p.eat('-')
	if p.eat('W') {
		w, ok := p.digits(2)
		if !ok {
			return 0, 0, 0, false
		}
		day := 1
		if ext && p.eat('-') || !ext && p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			if day, ok = p.digits(1); !ok {
				return 0, 0, 0, false
			}
		}
		if w < 1 || w > isoWeeks(y) || day < 1 || day > 7 {
			return 0, 0, 0, false
		}
		// Monday of ISO week 1 is the Monday on or before 4 January.
		jan4 := time.Date(y, 1, 4, 0, 0, 0, 0, time.UTC)
		monday := jan4.AddDate(0, 0, -((int(jan4.Weekday()) + 6) % 7))
		t := monday.AddDate(0, 0, (w-1)*7+day-1)
		return t.Year(), t.Month(), t.Day(), true
	}
	m, ok := p.digits(2)
	if !ok {
		return 0, 0, 0, false
	}
	if ext && !p.eat('-') {
		return 0, 0, 0, false
	}
	d, ok := p.digits(2)
	if !ok || m < 1 || m > 12 || d < 1 || d > daysIn(y, time.Month(m)) {
		return 0, 0, 0, false
	}
	return y, time.Month(m), d, true
}

func isoWeeks(y int) int {
	// A year has 53 ISO weeks when 28 December falls in week 53.
	_, w := time.Date(y, 12, 28, 0, 0, 0, 0, time.UTC).ISOWeek()
	return w
}

func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// clock reads HH[:MM[:SS[.f]]] or HHMM[SS[.f]]; the caller checks that an
// hour of 24 has nothing after it.
func (p *isoParser) clock() (hh, mm, ss, us int, ok bool) {
	if hh, ok = p.digits(2); !ok || hh > 24 {
		return 0, 0, 0, 0, false
	}
	if p.atOffset() {
		return hh, 0, 0, 0, true
	}
	ext := p.eat(':')
	if mm, ok = p.digits(2); !ok || mm > 59 {
		return 0, 0, 0, 0, false
	}
	if p.atOffset() {
		return hh, mm, 0, 0, true
	}
	if ext && !p.eat(':') {
		return 0, 0, 0, 0, false
	}
	if ss, ok = p.digits(2); !ok || ss > 59 {
		return 0, 0, 0, 0, false
	}
	if p.eat('.') || p.eat(',') {
		n := 0
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			if n < 6 {
				us = us*10 + int(p.s[p.i]-'0')
			}
			n++
			p.i++
		}
		if n == 0 {
			return 0, 0, 0, 0, false
		}
		for ; n < 6; n++ {
			us *= 10
		}
	}
	return hh, mm, ss, us, true
}

func (p *isoParser) atOffset() bool {
	return p.i == len(p.s) || p.peek('Z') || p.peek('+') || p.peek('-')
}

// offset reads Z or ±HH[:MM[:SS[.f]]] / ±HHMM[SS], in seconds east of UTC.
// Python allows sub-second offsets; they are dropped here, as no factory
// stamp has one.
func (p *isoParser) offset() (int, bool) {
	if p.eat('Z') {
		return 0, true
	}
	sign := 1
	switch {
	case p.eat('+'):
	case p.eat('-'):
		sign = -1
	default:
		return 0, false
	}
	h, ok := p.digits(2)
	if !ok || h > 23 {
		return 0, false
	}
	off := h * 3600
	if p.i == len(p.s) {
		return sign * off, true
	}
	ext := p.eat(':')
	m, ok := p.digits(2)
	if !ok || m > 59 {
		return 0, false
	}
	off += m * 60
	if p.i == len(p.s) {
		return sign * off, true
	}
	if ext && !p.eat(':') {
		return 0, false
	}
	sec, ok := p.digits(2)
	if !ok || sec > 59 {
		return 0, false
	}
	off += sec
	if p.eat('.') {
		n := 0
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
			n++
		}
		if n == 0 {
			return 0, false
		}
	}
	return sign * off, true
}
