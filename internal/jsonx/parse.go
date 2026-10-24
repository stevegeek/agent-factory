package jsonx

import (
	"strconv"
	"unicode/utf8"
)

// MaxDepth bounds the nesting Parse accepts. Python's json fails somewhere
// beyond 50,000 levels (RecursionError); no factory file comes near either.
const MaxDepth = 10000

// SyntaxError is a document Parse refuses, at byte Offset.
type SyntaxError struct {
	Offset int
	Msg    string
}

func (e *SyntaxError) Error() string {
	if e.Offset < 0 {
		return "jsonx: " + e.Msg
	}
	return "jsonx: " + e.Msg + " at offset " + strconv.Itoa(e.Offset)
}

// Parse reads one JSON document as Python's json.loads does: the JSON
// grammar plus NaN, Infinity and -Infinity; whitespace is only space, tab,
// CR and LF; control characters inside strings are refused (strict mode);
// a repeated key keeps its first position and takes its last value; a
// surrogate escape pair becomes one code point and a lone surrogate is kept
// (as WTF-8). Unlike json.loads it also refuses invalid UTF-8, which Python
// could not have read from the file in the first place, and a leading BOM,
// which json.loads refuses too.
func Parse(data []byte) (Value, error) {
	p := parser{data: data}
	if !utf8.Valid(data) {
		off := 0
		for off < len(data) {
			r, size := utf8.DecodeRune(data[off:])
			if r == utf8.RuneError && size == 1 {
				break
			}
			off += size
		}
		return Value{}, &SyntaxError{Offset: off, Msg: "invalid UTF-8"}
	}
	p.space()
	v, err := p.value(0)
	if err != nil {
		return Value{}, err
	}
	p.space()
	if p.i != len(p.data) {
		return Value{}, p.fail("extra data")
	}
	return v, nil
}

type parser struct {
	data []byte
	i    int
}

func (p *parser) fail(msg string) error { return &SyntaxError{Offset: p.i, Msg: msg} }

func (p *parser) space() {
	for p.i < len(p.data) {
		switch p.data[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) word(w string) bool {
	if len(p.data)-p.i >= len(w) && string(p.data[p.i:p.i+len(w)]) == w {
		p.i += len(w)
		return true
	}
	return false
}

func (p *parser) value(depth int) (Value, error) {
	if p.i >= len(p.data) {
		return Value{}, p.fail("expecting value")
	}
	switch c := p.data[p.i]; {
	case c == '{':
		if depth >= MaxDepth {
			return Value{}, p.fail("nested too deeply")
		}
		return p.object(depth + 1)
	case c == '[':
		if depth >= MaxDepth {
			return Value{}, p.fail("nested too deeply")
		}
		return p.array(depth + 1)
	case c == '"':
		s, err := p.str()
		if err != nil {
			return Value{}, err
		}
		return String(s), nil
	case p.word("null"):
		return Value{}, nil
	case p.word("true"):
		return Bool(true), nil
	case p.word("false"):
		return Bool(false), nil
	case p.word("NaN"):
		return Value{kind: KindNumber, s: "NaN"}, nil
	case p.word("Infinity"):
		return Value{kind: KindNumber, s: "Infinity"}, nil
	case p.word("-Infinity"):
		return Value{kind: KindNumber, s: "-Infinity"}, nil
	case c == '-' || isDigit(c):
		n := numberLen(string(p.data[p.i:min(len(p.data), p.i+numberScanCap(p.data[p.i:]))]))
		if n == 0 {
			return Value{}, p.fail("expecting value")
		}
		lit := string(p.data[p.i : p.i+n])
		p.i += n
		return Value{kind: KindNumber, s: lit}, nil
	}
	return Value{}, p.fail("expecting value")
}

// numberScanCap is the length of the run of number characters at the start
// of b, so that numberLen does not copy the rest of the document.
func numberScanCap(b []byte) int {
	n := 0
	for n < len(b) {
		switch c := b[n]; {
		case isDigit(c), c == '-', c == '+', c == '.', c == 'e', c == 'E':
			n++
		default:
			return n
		}
	}
	return n
}

func (p *parser) array(depth int) (Value, error) {
	p.i++ // [
	p.space()
	vs := []Value{}
	if p.i < len(p.data) && p.data[p.i] == ']' {
		p.i++
		return ArrayOf(vs...), nil
	}
	for {
		v, err := p.value(depth)
		if err != nil {
			return Value{}, err
		}
		vs = append(vs, v)
		p.space()
		if p.i >= len(p.data) {
			return Value{}, p.fail("expecting ',' delimiter")
		}
		switch p.data[p.i] {
		case ',':
			p.i++
			p.space()
		case ']':
			p.i++
			return ArrayOf(vs...), nil
		default:
			return Value{}, p.fail("expecting ',' delimiter")
		}
	}
}

func (p *parser) object(depth int) (Value, error) {
	p.i++ // {
	p.space()
	o := Object{}
	if p.i < len(p.data) && p.data[p.i] == '}' {
		p.i++
		return o.Value(), nil
	}
	for {
		if p.i >= len(p.data) || p.data[p.i] != '"' {
			return Value{}, p.fail("expecting property name enclosed in double quotes")
		}
		k, err := p.str()
		if err != nil {
			return Value{}, err
		}
		p.space()
		if p.i >= len(p.data) || p.data[p.i] != ':' {
			return Value{}, p.fail("expecting ':' delimiter")
		}
		p.i++
		p.space()
		v, err := p.value(depth)
		if err != nil {
			return Value{}, err
		}
		o.Set(k, v)
		p.space()
		if p.i >= len(p.data) {
			return Value{}, p.fail("expecting ',' delimiter")
		}
		switch p.data[p.i] {
		case ',':
			p.i++
			p.space()
		case '}':
			p.i++
			return o.Value(), nil
		default:
			return Value{}, p.fail("expecting ',' delimiter")
		}
	}
}

// str reads a string at p.i (on its opening quote).
func (p *parser) str() (string, error) {
	start := p.i
	p.i++
	// Fast path: no escapes.
	for j := p.i; j < len(p.data); j++ {
		c := p.data[j]
		if c == '"' {
			s := string(p.data[p.i:j])
			p.i = j + 1
			return s, nil
		}
		if c == '\\' || c < 0x20 {
			break
		}
	}
	var b []byte
	for {
		if p.i >= len(p.data) {
			return "", &SyntaxError{Offset: start, Msg: "unterminated string"}
		}
		c := p.data[p.i]
		switch {
		case c == '"':
			p.i++
			return string(b), nil
		case c < 0x20:
			return "", p.fail("invalid control character")
		case c != '\\':
			b = append(b, c)
			p.i++
			continue
		}
		// An escape.
		if p.i+1 >= len(p.data) {
			return "", &SyntaxError{Offset: start, Msg: "unterminated string"}
		}
		e := p.data[p.i+1]
		p.i += 2
		switch e {
		case '"', '\\', '/':
			b = append(b, e)
		case 'b':
			b = append(b, '\b')
		case 'f':
			b = append(b, '\f')
		case 'n':
			b = append(b, '\n')
		case 'r':
			b = append(b, '\r')
		case 't':
			b = append(b, '\t')
		case 'u':
			r, ok := p.hex4()
			if !ok {
				return "", p.fail("invalid \\u escape")
			}
			if r >= 0xD800 && r <= 0xDBFF && p.i+1 < len(p.data) && p.data[p.i] == '\\' && p.data[p.i+1] == 'u' {
				save := p.i
				p.i += 2
				lo, ok := p.hex4()
				if ok && lo >= 0xDC00 && lo <= 0xDFFF {
					b = utf8.AppendRune(b, 0x10000+(r-0xD800)<<10+(lo-0xDC00))
					continue
				}
				if !ok {
					return "", p.fail("invalid \\u escape")
				}
				p.i = save // a lone high surrogate; the next escape is read on its own
			}
			b = appendWTF8(b, r)
		default:
			p.i -= 2
			return "", p.fail("invalid escape")
		}
	}
}

func (p *parser) hex4() (rune, bool) {
	if p.i+4 > len(p.data) {
		return 0, false
	}
	var r rune
	for _, c := range p.data[p.i : p.i+4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	p.i += 4
	return r, true
}

// appendWTF8 is utf8.AppendRune that also writes a surrogate as its
// three-byte generalized UTF-8 form.
func appendWTF8(b []byte, r rune) []byte {
	if r >= 0xD800 && r <= 0xDFFF {
		return append(b, byte(0xE0|r>>12), byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F))
	}
	return utf8.AppendRune(b, r)
}

// surrogateAt reports a WTF-8 surrogate at the start of s.
func surrogateAt(s string) (rune, bool) {
	if len(s) >= 3 && s[0] == 0xED && s[1] >= 0xA0 && s[1] <= 0xBF && s[2] >= 0x80 && s[2] <= 0xBF {
		return 0xD000 | rune(s[1]&0x3F)<<6 | rune(s[2]&0x3F), true
	}
	return 0, false
}
