package ticket

import (
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/textx"
)

// ID is a ticket id: "T" and 4 to 6 digits, "T%04d" when the factory makes it.
type ID string

// ParseID is the id a caller named, upper-cased, if it is one
// (re.fullmatch(r"T\d{4,6}", tid.upper()) in changing(): \d is any Unicode
// decimal digit, as in Python). Anything else, "../T0001" included, is not.
func ParseID(s string) (ID, bool) {
	if s == "" || (s[0] != 'T' && s[0] != 't') {
		return "", false
	}
	n := 0
	for _, r := range s[1:] {
		if !textx.IsDecimal(r) {
			return "", false
		}
		n++
	}
	if n < 4 || n > 6 {
		return "", false
	}
	return ID("T" + s[1:]), true
}

// FormatID is the id of ticket number n: T%04d.
func FormatID(n int) ID { return ID(fmt.Sprintf("T%04d", n)) }

// Num is the id's number: int(re.sub(r"\D", "", id)), with Python's reading
// of Unicode digits. It is -1 when the id holds no digit.
func (id ID) Num() int {
	n, any := 0, false
	for _, r := range string(id) {
		if !textx.IsDecimal(r) {
			continue
		}
		n, any = n*10+digitValue(r), true
	}
	if !any {
		return -1
	}
	return n
}

// Less orders ids by number, the order locks are taken in.
func (id ID) Less(other ID) bool {
	if a, b := id.Num(), other.Num(); a != b {
		return a < b
	}
	return id < other
}

// digitValue is the value of a Unicode decimal digit. Nd characters come in
// runs of whole blocks, each 0 to 9 in order (the mathematical digits are
// five blocks in a row), so it is the distance from the run's start, mod 10.
func digitValue(r rune) int {
	if r < utf8.RuneSelf {
		return int(r - '0')
	}
	start := r
	for unicode.Is(unicode.Nd, start-1) {
		start--
	}
	return int(r-start) % 10
}
