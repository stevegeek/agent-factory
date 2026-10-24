package clock

import (
	"testing"
	"time"
)

func TestStampIsPythonNow(t *testing.T) {
	// now() in bin/factory: utcnow().replace(microsecond=0).isoformat().
	for _, c := range []struct {
		in   time.Time
		want string
	}{
		{time.Date(2026, 10, 17, 8, 0, 0, 0, time.UTC), "2026-10-17T08:00:00+00:00"},
		{time.Date(2026, 10, 17, 8, 0, 0, 999_999_999, time.UTC), "2026-10-17T08:00:00+00:00"}, // truncated, never rounded
		{time.Date(2026, 10, 17, 10, 30, 5, 0, time.FixedZone("x", 2*3600+30*60)), "2026-10-17T08:00:05+00:00"},
		{time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), "0001-01-01T00:00:00+00:00"},
		{time.Date(2026, 12, 31, 23, 59, 59, 500_000_000, time.FixedZone("y", -3600)), "2027-01-01T00:59:59+00:00"},
	} {
		if got := Stamp(c.in); got != c.want {
			t.Errorf("Stamp(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The expected values were produced by Python 3.14's datetime.fromisoformat
// (aware flag = an offset was given; naive stamps read as UTC, as the
// FACTORY_TEST_NOW seam does).
func TestParseFollowsPythonFromisoformat(t *testing.T) {
	type want struct {
		at     string // RFC 3339 with nanoseconds, in UTC
		offset int    // seconds east of UTC, when aware
		aware  bool
	}
	ok := func(at string, aware bool, offset int) *want { return &want{at, offset, aware} }
	for _, c := range []struct {
		in   string
		want *want // nil: Python raises ValueError
	}{
		{"2026-10-17T08:00:00+00:00", ok("2026-10-17T08:00:00Z", true, 0)},
		{"2026-10-17T08:00:00Z", ok("2026-10-17T08:00:00Z", true, 0)},
		{"2026-10-17T08:00:00z", nil},
		{"2026-10-17T08:00:00", ok("2026-10-17T08:00:00Z", false, 0)},
		{"2026-10-17", ok("2026-10-17T00:00:00Z", false, 0)},
		{"20261017", ok("2026-10-17T00:00:00Z", false, 0)},
		{"2026-10-17T08:00", ok("2026-10-17T08:00:00Z", false, 0)},
		{"2026-10-17T08", ok("2026-10-17T08:00:00Z", false, 0)},
		{"2026-10-17 08:00:00", ok("2026-10-17T08:00:00Z", false, 0)},
		{"2026-10-17x08:00:00", ok("2026-10-17T08:00:00Z", false, 0)},
		{"2026-10-17Ä08:00", ok("2026-10-17T08:00:00Z", false, 0)},
		{"2026-10-17t08:00:00", ok("2026-10-17T08:00:00Z", false, 0)},
		{"2026-10-17T08:00:00.5+00:00", ok("2026-10-17T08:00:00.5Z", true, 0)},
		{"2026-10-17T08:00:00.123456Z", ok("2026-10-17T08:00:00.123456Z", true, 0)},
		{"2026-10-17T08:00:00.1234567Z", ok("2026-10-17T08:00:00.123456Z", true, 0)},
		{"2026-10-17T08:00:00.12345678901Z", ok("2026-10-17T08:00:00.123456Z", true, 0)},
		{"2026-10-17T08:00:00.000000001+00:00", ok("2026-10-17T08:00:00Z", true, 0)},
		{"2026-10-17T08:00:00,5Z", ok("2026-10-17T08:00:00.5Z", true, 0)},
		{"2026-10-17T08:00:00.5", ok("2026-10-17T08:00:00.5Z", false, 0)},
		{"2026-10-17T08:00:00.12+01:00", ok("2026-10-17T07:00:00.12Z", true, 3600)},
		{"2026-10-17T08:00:00+02:00", ok("2026-10-17T06:00:00Z", true, 7200)},
		{"2026-10-17T08:00:00-05:30", ok("2026-10-17T13:30:00Z", true, -19800)},
		{"2026-10-17T08:00:00+0200", ok("2026-10-17T06:00:00Z", true, 7200)},
		{"2026-10-17T08:00:00+0530", ok("2026-10-17T02:30:00Z", true, 19800)},
		{"2026-10-17T08:00:00+02", ok("2026-10-17T06:00:00Z", true, 7200)},
		{"2026-10-17T08:00:00+02:00:30", ok("2026-10-17T05:59:30Z", true, 7230)},
		{"2026-10-17T08:00:00-00:00", ok("2026-10-17T08:00:00Z", true, 0)},
		{"2026-10-17T08:00Z", ok("2026-10-17T08:00:00Z", true, 0)},
		{"2026-10-17T08Z", ok("2026-10-17T08:00:00Z", true, 0)},
		{"2026-10-17T080000", ok("2026-10-17T08:00:00Z", false, 0)},
		{"2026-10-17T0800", ok("2026-10-17T08:00:00Z", false, 0)},
		{"2026-W42-6", ok("2026-10-17T00:00:00Z", false, 0)},
		{"2026W426", ok("2026-10-17T00:00:00Z", false, 0)},
		{"2026-W42", ok("2026-10-12T00:00:00Z", false, 0)},
		{"2026-10-17T24:00:00", ok("2026-10-18T00:00:00Z", false, 0)},
		{"0001-01-01T00:00:00", ok("0001-01-01T00:00:00Z", false, 0)},
		{"2026-02-30", nil},
		{"", nil},
		{"x", nil},
		{"2026-10-17T08:00:00+00:00 ", nil},
		{"2026-1-17", nil},
		{"2026-10-17T08:00:60", nil},
		{"2026-10-17T08:00:00.", nil},
		{"2026-10-17T08:00:00+24:00", nil},
		{"2026-10-17T08:00:00 UTC", nil},
		{"2026-10-17T8:00:00", nil},
		{"+2026-10-17", nil},
	} {
		at, aware, err := ParseISO(c.in)
		if c.want == nil {
			if err == nil {
				t.Errorf("ParseISO(%q) = %v, want an error", c.in, at)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseISO(%q): %v", c.in, err)
			continue
		}
		if got := at.UTC().Format(time.RFC3339Nano); got != c.want.at || aware != c.want.aware {
			t.Errorf("ParseISO(%q) = %s aware=%v, want %s aware=%v", c.in, got, aware, c.want.at, c.want.aware)
		}
		if _, off := at.Zone(); aware && off != c.want.offset {
			t.Errorf("ParseISO(%q) offset %d, want %d", c.in, off, c.want.offset)
		}
		p, err := Parse(c.in)
		if err != nil || !p.Equal(at) || p.Location() != time.UTC {
			t.Errorf("Parse(%q) = %v, %v; want %v in UTC", c.in, p, err, at)
		}
	}
}

func TestStampRoundTrips(t *testing.T) {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	p, err := Parse(Stamp(at))
	if err != nil || !p.Equal(at) {
		t.Fatalf("Parse(Stamp) = %v, %v", p, err)
	}
}

func TestClocks(t *testing.T) {
	var c Clock = System{}
	if n := c.Now(); n.Location() != time.UTC || time.Since(n) > time.Minute {
		t.Errorf("System.Now() = %v", n)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	c = Fixed{T: at}
	if !c.Now().Equal(at) || !c.Now().Equal(at) {
		t.Error("Fixed moved")
	}
	s := NewStep(at, time.Second)
	c = s
	if a, b := c.Now(), c.Now(); !a.Equal(at) || !b.Equal(at.Add(time.Second)) {
		t.Errorf("Step: %v %v", a, b)
	}
	s.Advance(time.Hour)
	if n := s.Now(); !n.Equal(at.Add(2*time.Second + time.Hour)) {
		t.Errorf("Step after Advance: %v", n)
	}
}

func TestFromEnvValue(t *testing.T) {
	// FACTORY_TEST_NOW (seam 3): a stamp holds the clock still; empty means the system clock.
	c, err := FromTestNow("2026-10-17T08:00:00.75+02:00")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Now(); got.Location() != time.UTC || got.Format(time.RFC3339Nano) != "2026-10-17T06:00:00.75Z" {
		t.Errorf("FromTestNow clock says %v", got)
	}
	if got := Stamp(c.Now()); got != "2026-10-17T06:00:00+00:00" {
		t.Errorf("stamp %q", got)
	}
	if c, err := FromTestNow(""); err != nil {
		t.Fatal(err)
	} else if _, ok := c.(System); !ok {
		t.Errorf("FromTestNow(\"\") = %T, want System", c)
	}
	if _, err := FromTestNow("soon"); err == nil {
		t.Error("FromTestNow(soon) did not fail")
	}
}
