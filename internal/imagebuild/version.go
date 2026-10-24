package imagebuild

import (
	"strings"
	"unicode"
)

// The version comparison of the version check and the automatic updates
// (§15.1): Python's version_tuple, so both agree with `factory versions`.

// Newer is version_tuple(latest) > version_tuple(pinned): the first three
// runs of decimal digits (Python's \d, any script) as integers, compared as
// tuples (a shorter tuple that is a prefix is the smaller).
func Newer(latest, pinned string) bool {
	a, b := versionTuple(latest), versionTuple(pinned)
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := cmpInt(a[i], b[i]); c != 0 {
			return c > 0
		}
	}
	return len(a) > len(b)
}

// versionTuple is version_tuple (F:2832): up to three integers, each as
// its ASCII digits with no leading zeros (Python ints have no size limit).
func versionTuple(v string) []string {
	var out []string
	var cur []byte
	in := false
	flush := func() {
		if in {
			n := strings.TrimLeft(string(cur), "0")
			if n == "" {
				n = "0"
			}
			out = append(out, n)
		}
		cur, in = cur[:0], false
	}
	for _, r := range v {
		if len(out) == 3 {
			break
		}
		d, ok := digitValue(r)
		if !ok {
			flush()
			continue
		}
		cur = append(cur, byte('0'+d))
		in = true
	}
	if len(out) < 3 {
		flush()
	}
	return out
}

// cmpInt compares two non-negative integers written without leading zeros.
func cmpInt(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

// digitValue is the value of a Unicode decimal digit (category Nd). Each
// such script's digits are a contiguous run from 0 to 9 (a Unicode
// stability guarantee), so the value is the distance from the run's start,
// modulo ten for blocks that hold several runs back to back.
func digitValue(r rune) (int, bool) {
	if r >= '0' && r <= '9' {
		return int(r - '0'), true
	}
	if !unicode.Is(unicode.Nd, r) {
		return 0, false
	}
	start := r
	for start > 0 && unicode.Is(unicode.Nd, start-1) {
		start--
	}
	return int(r-start) % 10, true
}
