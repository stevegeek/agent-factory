package helper

import (
	"errors"
	"strings"
)

// shlexSplit is Python's shlex.split(s) (posix mode, no comments), which
// bin/factory-remote applies to SSH_ORIGINAL_COMMAND: words split on space,
// tab, CR and LF; single quotes keep everything; double quotes keep
// everything but a backslash before `"` or `\`; outside quotes a backslash
// keeps the next character. An empty quoted word is a word. It never
// expands anything: no shell ever sees the line.
func shlexSplit(s string) ([]string, error) {
	const (
		between = iota // whitespace between words
		word
		single
		double
		escape       // after a backslash outside quotes
		doubleEscape // after a backslash inside double quotes
	)
	var (
		out    []string
		tok    strings.Builder
		state  = between
		quoted bool // the word so far holds a quote, so "" is a word
	)
	emit := func() {
		if tok.Len() > 0 || quoted {
			out = append(out, tok.String())
		}
		tok.Reset()
		quoted = false
	}
	for _, r := range s {
		switch state {
		case between, word:
			switch {
			case r == ' ' || r == '\t' || r == '\r' || r == '\n':
				if state == word {
					emit()
				}
				state = between
			case r == '\'':
				state, quoted = single, true
			case r == '"':
				state, quoted = double, true
			case r == '\\':
				state = escape
			default:
				tok.WriteRune(r)
				state = word
			}
		case single:
			if r == '\'' {
				state = word
			} else {
				tok.WriteRune(r)
			}
		case double:
			switch r {
			case '"':
				state = word
			case '\\':
				state = doubleEscape
			default:
				tok.WriteRune(r)
			}
		case escape:
			tok.WriteRune(r)
			state = word
		case doubleEscape:
			if r != '"' && r != '\\' {
				tok.WriteByte('\\')
			}
			tok.WriteRune(r)
			state = double
		}
	}
	switch state {
	case single, double:
		return nil, errors.New("No closing quotation")
	case escape, doubleEscape:
		return nil, errors.New("No escaped character")
	case word:
		emit()
	}
	return out, nil
}
