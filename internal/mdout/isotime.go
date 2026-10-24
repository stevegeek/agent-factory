package mdout

import (
	"strconv"
	"time"
)

// parseISO reads the times md_time is given, as datetime.fromisoformat
// does for the forms a ticket holds: YYYY-MM-DD, optionally followed by "T"
// or a space and HH[:MM[:SS[.ffffff]]], and an optional "Z" or
// ±HH[:MM[:SS]] offset. A time with no offset is UTC. (clock.ParseISO reads
// every form; this package may not import clock.)
func parseISO(s string) (time.Time, bool) {
	p := isoReader{s: s, ok: true}
	y := p.num(4)
	dash1 := p.lit('-')
	month := p.num(2)
	dash2 := p.lit('-')
	d := p.num(2)
	if !p.ok || !dash1 || !dash2 {
		return time.Time{}, false
	}
	var h, mi, sec, nsec int
	loc := time.UTC
	if p.more() {
		if !p.lit('T') && !p.lit(' ') {
			return time.Time{}, false
		}
		h = p.num(2)
		if p.peek(':') {
			p.lit(':')
			mi = p.num(2)
			if p.peek(':') {
				p.lit(':')
				sec = p.num(2)
				if p.peek('.') || p.peek(',') {
					p.i++
					start := p.i
					for p.i < len(s) && s[p.i] >= '0' && s[p.i] <= '9' {
						p.i++
					}
					if p.i == start {
						return time.Time{}, false
					}
					frac := (s[start:p.i] + "000000")[:6] // Python keeps microseconds
					us, _ := strconv.Atoi(frac)
					nsec = us * 1000
				}
			}
		}
		switch {
		case !p.more():
		case p.peek('Z'):
			p.i++
		case p.peek('+') || p.peek('-'):
			sign := 1
			if p.peek('-') {
				sign = -1
			}
			p.i++
			off := p.num(2) * 3600
			if p.peek(':') {
				p.lit(':')
				off += p.num(2) * 60
				if p.peek(':') {
					p.lit(':')
					off += p.num(2)
				}
			}
			loc = time.FixedZone("", sign*off)
		default:
			return time.Time{}, false
		}
	}
	if !p.ok || p.more() || h > 23 || mi > 59 || sec > 59 {
		return time.Time{}, false
	}
	t := time.Date(y, time.Month(month), d, h, mi, sec, nsec, loc)
	if t.Year() != y || int(t.Month()) != month || t.Day() != d {
		return time.Time{}, false
	}
	return t, true
}

type isoReader struct {
	s  string
	i  int
	ok bool
}

func (p *isoReader) more() bool { return p.i < len(p.s) }

func (p *isoReader) peek(c byte) bool { return p.i < len(p.s) && p.s[p.i] == c }

func (p *isoReader) lit(c byte) bool {
	if p.peek(c) {
		p.i++
		return true
	}
	return false
}

// num reads exactly n ASCII digits; on failure it clears ok.
func (p *isoReader) num(n int) int {
	if p.i+n > len(p.s) {
		p.ok = false
		return 0
	}
	v := 0
	for _, c := range []byte(p.s[p.i : p.i+n]) {
		if c < '0' || c > '9' {
			p.ok = false
			return 0
		}
		v = v*10 + int(c-'0')
	}
	p.i += n
	return v
}
