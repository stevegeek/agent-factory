package jsonx

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// validNumber reports JSON number syntax, or one of the three names
// json.loads accepts.
func validNumber(s string) bool {
	switch s {
	case "NaN", "Infinity", "-Infinity":
		return true
	}
	return numberLen(s) == len(s) && s != ""
}

// numberLen is the length of the JSON number at the start of s, or 0:
// -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][-+]?[0-9]+)?
func numberLen(s string) int {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	default:
		return 0
	}
	if i+1 < len(s) && s[i] == '.' && isDigit(s[i+1]) {
		i += 2
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && isDigit(s[j]) {
			for j < len(s) && isDigit(s[j]) {
				j++
			}
			i = j
		}
	}
	return i
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isIntLiteral(s string) bool {
	return s != "NaN" && s != "Infinity" && s != "-Infinity" && !strings.ContainsAny(s, ".eE")
}

func isRangeErr(err error) bool { return errors.Is(err, strconv.ErrRange) }

// pyNumber is how json.dumps writes the number json.loads reads from lit:
// an int keeps its digits (Python ints are exact; -0 is 0), and a float is
// the nearest double in Python's repr (an overflow is Infinity).
func pyNumber(lit string) string {
	switch lit {
	case "NaN", "Infinity", "-Infinity":
		return lit
	}
	if isIntLiteral(lit) {
		if strings.Trim(lit, "-0") == "" {
			return "0"
		}
		return lit
	}
	f, err := strconv.ParseFloat(lit, 64)
	if err != nil && !isRangeErr(err) {
		return lit // not reachable for a validated literal
	}
	return pyFloat(f)
}

// pyFloat is Python's repr(float) as json.dumps writes it: the shortest
// digits that read back as f; positional notation when the decimal point
// falls between 10^-4 and 10^16 (with ".0" for a whole number), otherwise
// d.ddde±XX with at least two exponent digits.
func pyFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // [-]d[.ddd]e±XX
	sign := ""
	if e[0] == '-' {
		sign, e = "-", e[1:]
	}
	mant, exp, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	x, _ := strconv.Atoi(exp)
	decpt := x + 1 // f = 0.digits * 10^decpt
	var b strings.Builder
	b.WriteString(sign)
	switch {
	case decpt > -4 && decpt <= 16:
		switch {
		case decpt <= 0:
			b.WriteString("0.")
			b.WriteString(strings.Repeat("0", -decpt))
			b.WriteString(digits)
		case decpt >= len(digits):
			b.WriteString(digits)
			b.WriteString(strings.Repeat("0", decpt-len(digits)))
			b.WriteString(".0")
		default:
			b.WriteString(digits[:decpt])
			b.WriteByte('.')
			b.WriteString(digits[decpt:])
		}
	default:
		b.WriteByte(digits[0])
		if len(digits) > 1 {
			b.WriteByte('.')
			b.WriteString(digits[1:])
		}
		b.WriteByte('e')
		if x < 0 {
			b.WriteByte('-')
			x = -x
		} else {
			b.WriteByte('+')
		}
		if x < 10 {
			b.WriteByte('0')
		}
		b.WriteString(strconv.Itoa(x))
	}
	return b.String()
}
