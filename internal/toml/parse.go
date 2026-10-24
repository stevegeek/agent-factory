package toml

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Parse reads a TOML document in the subset. It follows tomllib's grammar
// and refusals (the error messages are tomllib's where it has one), and
// refuses dates and times, dotted keys in key/value pairs and integers
// beyond 64 bits with a message that says so.
func Parse(data []byte) (*Table, error) {
	if !utf8.Valid(data) {
		bad := 0
		for bad < len(data) {
			r, n := utf8.DecodeRune(data[bad:])
			if r == utf8.RuneError && n <= 1 {
				break
			}
			bad += n
		}
		line, col := position(string(data[:bad]), bad)
		return nil, &Error{Line: line, Col: col, Msg: "the file is not valid UTF-8"}
	}
	// tomllib reads "\r\n" as "\n" before anything else.
	p := &parser{src: strings.ReplaceAll(string(data), "\r\n", "\n"), root: &Table{explicit: true}}
	if err := p.document(); err != nil {
		return nil, err
	}
	finish(p.root)
	return p.root, nil
}

// aot is an array of tables while the document is read; finish turns it
// into a []Value.
type aot struct{ tables []*Table }

func finish(t *Table) {
	for _, k := range t.keys {
		switch x := t.vals[k].(type) {
		case *aot:
			arr := make([]Value, len(x.tables))
			for i, el := range x.tables {
				finish(el)
				arr[i] = el
			}
			t.vals[k] = arr
		case *Table:
			finish(x)
		}
	}
}

type parser struct {
	src  string
	pos  int
	root *Table
}

const (
	ws         = " \t"
	wsNewline  = " \t\n"
	bareKey    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	hexDigits  = "0123456789abcdefABCDEF"
	decDigits  = "0123456789"
	keyInitial = bareKey + `"'`
)

func position(src string, pos int) (line, col int) {
	head := src[:pos]
	line = strings.Count(head, "\n") + 1
	if i := strings.LastIndexByte(head, '\n'); i >= 0 {
		head = head[i+1:]
	}
	return line, utf8.RuneCountInString(head) + 1
}

func (p *parser) errAt(pos int, format string, args ...any) error {
	line, col := position(p.src, pos)
	return &Error{Line: line, Col: col, Msg: fmt.Sprintf(format, args...)}
}

func (p *parser) eof() bool { return p.pos >= len(p.src) }

// peek is the byte at pos, or 0 at the end.
func (p *parser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.src[p.pos]
}

func (p *parser) at(s string) bool { return strings.HasPrefix(p.src[p.pos:], s) }

func (p *parser) in(set string) bool {
	return !p.eof() && p.src[p.pos] < utf8.RuneSelf && strings.IndexByte(set, p.src[p.pos]) >= 0
}

func (p *parser) skip(set string) {
	for p.in(set) {
		p.pos++
	}
}

// illegal reports the ASCII controls tomllib refuses in strings and
// comments: all of 0x00-0x1f and 0x7f except tab, and except newline where
// newlineOK.
func illegal(c byte, newlineOK bool) bool {
	if c == '\t' || (newlineOK && c == '\n') {
		return false
	}
	return c < 0x20 || c == 0x7f
}

func (p *parser) document() error {
	cur := p.root
	for {
		p.skip(ws)
		if p.eof() {
			return nil
		}
		c := p.peek()
		switch {
		case c == '\n':
			p.pos++
			continue
		case strings.IndexByte(keyInitial, c) >= 0:
			if err := p.keyValue(cur); err != nil {
				return err
			}
			p.skip(ws)
		case c == '[':
			var err error
			if p.at("[[") {
				cur, err = p.arrayHeader()
			} else {
				cur, err = p.tableHeader()
			}
			if err != nil {
				return err
			}
			p.skip(ws)
		case c != '#':
			return p.errAt(p.pos, "Invalid statement")
		}
		if err := p.comment(); err != nil {
			return err
		}
		if p.eof() {
			return nil
		}
		if p.peek() != '\n' {
			return p.errAt(p.pos, "Expected newline or end of document after a statement")
		}
		p.pos++
	}
}

func (p *parser) comment() error {
	if p.peek() != '#' {
		return nil
	}
	p.pos++
	for !p.eof() && p.peek() != '\n' {
		if illegal(p.peek(), false) {
			return p.errAt(p.pos, "Found invalid character %s in a comment", reprString(string(p.peek())))
		}
		p.pos++
	}
	return nil
}

// skipArrayWS skips whitespace, newlines and comments inside an array.
func (p *parser) skipArrayWS() error {
	for {
		start := p.pos
		p.skip(wsNewline)
		if err := p.comment(); err != nil {
			return err
		}
		if p.pos == start {
			return nil
		}
	}
}

// key reads a key (dotted where a header allows it), skipping the
// whitespace after each part.
func (p *parser) key() ([]string, error) {
	var parts []string
	for {
		part, err := p.keyPart()
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
		p.skip(ws)
		if p.peek() != '.' {
			return parts, nil
		}
		p.pos++
		p.skip(ws)
	}
}

func (p *parser) keyPart() (string, error) {
	switch c := p.peek(); {
	case p.in(bareKey):
		start := p.pos
		p.skip(bareKey)
		return p.src[start:p.pos], nil
	case c == '\'':
		return p.literalString()
	case c == '"':
		return p.basicString(false)
	}
	return "", p.errAt(p.pos, "Invalid initial character for a key part")
}

// pair reads "key = value"; a dotted key is refused.
func (p *parser) pair() (string, Value, error) {
	start := p.pos
	key, err := p.key()
	if err != nil {
		return "", nil, err
	}
	if p.peek() != '=' {
		return "", nil, p.errAt(p.pos, "Expected '=' after a key in a key/value pair")
	}
	if len(key) > 1 {
		return "", nil, p.errAt(start, "dotted keys are not supported in the factory's config (%s): write a [table] instead", strings.Join(key, "."))
	}
	p.pos++
	p.skip(ws)
	v, err := p.value()
	if err != nil {
		return "", nil, err
	}
	return key[0], v, nil
}

func (p *parser) keyValue(cur *Table) error {
	k, v, err := p.pair()
	if err != nil {
		return err
	}
	if _, ok := cur.vals[k]; ok {
		return p.errAt(p.pos, "Cannot overwrite a value")
	}
	cur.set(k, v)
	return nil
}

// parent walks a header's key to the table that holds its last part,
// creating implicit tables and entering the last table of an array of
// tables, as tomllib's get_or_create_nest does.
func (p *parser) parent(key []string, at int) (*Table, error) {
	cur := p.root
	for _, k := range key[:len(key)-1] {
		v, ok := cur.vals[k]
		if !ok {
			next := &Table{}
			cur.set(k, next)
			cur = next
			continue
		}
		switch x := v.(type) {
		case *Table:
			if x.inline {
				return nil, p.errAt(at, "Cannot mutate immutable namespace %s", dotted(key))
			}
			cur = x
		case *aot:
			cur = x.tables[len(x.tables)-1]
		case []Value:
			return nil, p.errAt(at, "Cannot mutate immutable namespace %s", dotted(key))
		default:
			return nil, p.errAt(at, "Cannot overwrite a value")
		}
	}
	return cur, nil
}

func dotted(key []string) string { return "[" + strings.Join(key, ".") + "]" }

func (p *parser) tableHeader() (*Table, error) {
	p.pos++
	p.skip(ws)
	key, err := p.key()
	if err != nil {
		return nil, err
	}
	parent, err := p.parent(key, p.pos)
	if err != nil {
		return nil, err
	}
	last := key[len(key)-1]
	var t *Table
	switch x := parent.vals[last].(type) {
	case nil:
		t = &Table{explicit: true}
		parent.set(last, t)
	case *Table:
		if x.explicit || x.inline {
			return nil, p.errAt(p.pos, "Cannot declare %s twice", dotted(key))
		}
		x.explicit = true
		t = x
	case *aot:
		return nil, p.errAt(p.pos, "Cannot declare %s twice", dotted(key))
	case []Value:
		return nil, p.errAt(p.pos, "Cannot declare %s twice", dotted(key))
	default:
		return nil, p.errAt(p.pos, "Cannot overwrite a value")
	}
	if p.peek() != ']' {
		return nil, p.errAt(p.pos, "Expected ']' at the end of a table declaration")
	}
	p.pos++
	return t, nil
}

func (p *parser) arrayHeader() (*Table, error) {
	p.pos += 2
	p.skip(ws)
	key, err := p.key()
	if err != nil {
		return nil, err
	}
	parent, err := p.parent(key, p.pos)
	if err != nil {
		return nil, err
	}
	last := key[len(key)-1]
	t := &Table{explicit: true}
	switch x := parent.vals[last].(type) {
	case nil:
		parent.set(last, &aot{tables: []*Table{t}})
	case *aot:
		x.tables = append(x.tables, t)
	case []Value:
		return nil, p.errAt(p.pos, "Cannot mutate immutable namespace %s", dotted(key))
	default:
		return nil, p.errAt(p.pos, "Cannot overwrite a value")
	}
	if !p.at("]]") {
		return nil, p.errAt(p.pos, "Expected ']]' at the end of an array declaration")
	}
	p.pos += 2
	return t, nil
}

func (p *parser) value() (Value, error) {
	switch c := p.peek(); {
	case c == '"':
		if p.at(`"""`) {
			return p.multilineString('"')
		}
		return p.basicString(false)
	case c == '\'':
		if p.at("'''") {
			return p.multilineString('\'')
		}
		return p.literalString()
	case p.at("true"):
		p.pos += 4
		return true, nil
	case p.at("false"):
		p.pos += 5
		return false, nil
	case c == '[':
		return p.array()
	case c == '{':
		return p.inlineTable()
	}
	if p.dateOrTime() {
		return nil, p.errAt(p.pos, "dates and times are not supported in the factory's config: write it as a string")
	}
	if v, ok, err := p.number(); ok || err != nil {
		return v, err
	}
	for _, s := range []string{"inf", "nan", "+inf", "-inf", "+nan", "-nan"} {
		if p.at(s) {
			p.pos += len(s)
			if strings.HasSuffix(s, "nan") {
				return math.NaN(), nil
			}
			f, _ := strconv.ParseFloat(s, 64)
			return f, nil
		}
	}
	return nil, p.errAt(p.pos, "Invalid value")
}

// dateOrTime spots the start of a TOML date (four digits and "-") or time
// (two digits and ":").
func (p *parser) dateOrTime() bool {
	rest := p.src[p.pos:]
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	return (digits == 4 && strings.HasPrefix(rest[4:], "-")) || (digits == 2 && strings.HasPrefix(rest[2:], ":"))
}

// digitsFrom scans digits from set at i with single underscores between
// them, as tomllib's [0-9](?:_?[0-9])*; it returns the end, or -1 when no
// digit starts at i.
func digitsFrom(s string, i int, set string) int {
	isDigit := func(j int) bool { return j < len(s) && strings.IndexByte(set, s[j]) >= 0 }
	if !isDigit(i) {
		return -1
	}
	i++
	for {
		switch {
		case isDigit(i):
			i++
		case i < len(s) && s[i] == '_' && isDigit(i+1):
			i += 2
		default:
			return i
		}
	}
}

// number reads what tomllib's RE_NUMBER matches at pos: 0x/0o/0b integers,
// or a signed decimal integer with an optional fraction and exponent.
func (p *parser) number() (Value, bool, error) {
	s, start := p.src, p.pos
	if strings.HasPrefix(s[start:], "0") && start+1 < len(s) {
		set := map[byte]string{'x': hexDigits, 'o': "01234567", 'b': "01"}[s[start+1]]
		base := map[byte]int{'x': 16, 'o': 8, 'b': 2}[s[start+1]]
		if set != "" {
			if end := digitsFrom(s, start+2, set); end > 0 {
				p.pos = end
				n, err := strconv.ParseInt(strings.ReplaceAll(s[start+2:end], "_", ""), base, 64)
				if err != nil {
					return nil, true, p.errAt(start, "integers beyond 64 bits are not supported in the factory's config")
				}
				return n, true, nil
			}
		}
	}
	i := start
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		i = digitsFrom(s, i, decDigits)
	default:
		return nil, false, nil
	}
	isFloat := false
	if i < len(s) && s[i] == '.' {
		if end := digitsFrom(s, i+1, decDigits); end > 0 {
			i, isFloat = end, true
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if end := digitsFrom(s, j, decDigits); end > 0 {
			i, isFloat = end, true
		}
	}
	p.pos = i
	text := strings.ReplaceAll(s[start:i], "_", "")
	if isFloat {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, true, p.errAt(start, "Invalid value")
		}
		return f, true, nil
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil, true, p.errAt(start, "integers beyond 64 bits are not supported in the factory's config")
	}
	return n, true, nil
}

func (p *parser) array() (Value, error) {
	p.pos++
	arr := []Value{}
	if err := p.skipArrayWS(); err != nil {
		return nil, err
	}
	if p.peek() == ']' {
		p.pos++
		return arr, nil
	}
	for {
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
		if err := p.skipArrayWS(); err != nil {
			return nil, err
		}
		switch p.peek() {
		case ']':
			p.pos++
			return arr, nil
		case ',':
		default:
			return nil, p.errAt(p.pos, "Unclosed array")
		}
		p.pos++
		if err := p.skipArrayWS(); err != nil {
			return nil, err
		}
		if p.peek() == ']' {
			p.pos++
			return arr, nil
		}
	}
}

func (p *parser) inlineTable() (Value, error) {
	p.pos++
	t := &Table{inline: true, explicit: true, vals: map[string]Value{}}
	p.skip(ws)
	if p.peek() == '}' {
		p.pos++
		return t, nil
	}
	for {
		k, v, err := p.pair()
		if err != nil {
			return nil, err
		}
		if _, ok := t.vals[k]; ok {
			return nil, p.errAt(p.pos, "Duplicate inline table key %s", reprString(k))
		}
		t.set(k, v)
		p.skip(ws)
		switch p.peek() {
		case '}':
			p.pos++
			return t, nil
		case ',':
		default:
			return nil, p.errAt(p.pos, "Unclosed inline table")
		}
		p.pos++
		p.skip(ws)
	}
}

func (p *parser) literalString() (string, error) {
	p.pos++
	start := p.pos
	for {
		if p.eof() {
			return "", p.errAt(p.pos, "Unterminated string")
		}
		c := p.peek()
		if c == '\'' {
			s := p.src[start:p.pos]
			p.pos++
			return s, nil
		}
		if illegal(c, false) {
			return "", p.errAt(p.pos, "Illegal character %s", reprString(string(c)))
		}
		p.pos++
	}
}

// basicString reads a "..." string, or the body of a """...""" one when
// multiline (pos just past the opening quotes and the trimmed newline); it
// stops after the closing quote(s).
func (p *parser) basicString(multiline bool) (string, error) {
	if !multiline {
		p.pos++
	}
	var b strings.Builder
	start := p.pos
	for {
		if p.eof() {
			return "", p.errAt(p.pos, "Unterminated string")
		}
		c := p.peek()
		switch {
		case c == '"':
			if !multiline {
				b.WriteString(p.src[start:p.pos])
				p.pos++
				return b.String(), nil
			}
			if p.at(`"""`) {
				b.WriteString(p.src[start:p.pos])
				p.pos += 3
				return b.String(), nil
			}
			p.pos++
		case c == '\\':
			b.WriteString(p.src[start:p.pos])
			s, err := p.escape(multiline)
			if err != nil {
				return "", err
			}
			b.WriteString(s)
			start = p.pos
		case illegal(c, multiline):
			return "", p.errAt(p.pos, "Illegal character %s", reprString(string(c)))
		default:
			p.pos++
		}
	}
}

func (p *parser) escape(multiline bool) (string, error) {
	id := p.src[p.pos:min(p.pos+2, len(p.src))]
	p.pos += len(id)
	if multiline && (id == `\ ` || id == "\\\t" || id == "\\\n") {
		if id != "\\\n" {
			p.skip(ws)
			if p.eof() {
				return "", nil
			}
			if p.peek() != '\n' {
				return "", p.errAt(p.pos, `Unescaped '\' in a string`)
			}
			p.pos++
		}
		p.skip(wsNewline)
		return "", nil
	}
	switch id {
	case `\u`:
		return p.hexChar(4)
	case `\U`:
		return p.hexChar(8)
	case `\b`:
		return "\b", nil
	case `\t`:
		return "\t", nil
	case `\n`:
		return "\n", nil
	case `\f`:
		return "\f", nil
	case `\r`:
		return "\r", nil
	case `\"`:
		return `"`, nil
	case `\\`:
		return `\`, nil
	}
	return "", p.errAt(p.pos, `Unescaped '\' in a string`)
}

func (p *parser) hexChar(n int) (string, error) {
	h := p.src[p.pos:min(p.pos+n, len(p.src))]
	if len(h) != n || strings.Trim(h, hexDigits) != "" {
		return "", p.errAt(p.pos, "Invalid hex value")
	}
	p.pos += n
	v, _ := strconv.ParseUint(h, 16, 32)
	if v > utf8.MaxRune || (v >= 0xd800 && v <= 0xdfff) {
		return "", p.errAt(p.pos, "Escaped character is not a Unicode scalar value")
	}
	return string(rune(v)), nil
}

// multilineString reads """...""" or ”'...”': a newline right after the
// opening quotes is dropped, and one or two quotes right after the closing
// ones belong to the string.
func (p *parser) multilineString(q byte) (string, error) {
	p.pos += 3
	if p.peek() == '\n' {
		p.pos++
	}
	var s string
	if q == '"' {
		var err error
		if s, err = p.basicString(true); err != nil {
			return "", err
		}
	} else {
		start := p.pos
		for !p.at("'''") {
			if p.eof() {
				return "", p.errAt(p.pos, "Unterminated string")
			}
			if c := p.peek(); illegal(c, true) {
				return "", p.errAt(p.pos, "Illegal character %s", reprString(string(c)))
			}
			p.pos++
		}
		s = p.src[start:p.pos]
		p.pos += 3
	}
	for range 2 {
		if p.peek() != q {
			break
		}
		s += string(q)
		p.pos++
	}
	return s, nil
}
