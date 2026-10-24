// Package memstore holds the agents' memory stores (ARCHITECTURE.md §4.8,
// inventory-features §1): one bare git repository per (project, role) under
// memory/, and nothing else. There is no work tree an agent could plant a
// hook, a nested repository or a filter in. Every read and write goes
// through git objects with a stripped environment (gitx.Bare); a run's copy
// is written into a fresh directory and read back without following links.
//
// This package is the mechanism: names and limits, git plumbing, and the
// file copies. The policy (which role, prompts, what a settle records on the
// ticket) is package memory.
package memstore

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The limits (F:996-1001).
const (
	MaxFile         = 256 * 1024      // one file
	MaxFiles        = 2000            // files in one copy
	MaxBytes        = 3 * 1024 * 1024 // a store in all: under the 4 MB memory.tar a helper job collects
	ContextMaxBytes = 512 * 1024      // the project context, which rides in the run's copy
	ContextTop      = "context"       // where a run's copy holds the context; never read back
	maxKey          = 80
	maxPath         = 300
)

// ErrName is returned for a project or role that is not a plain name.
var ErrName = errors.New("names must be plain")

// Files is a copy of a store: slash-separated path to contents.
type Files map[string][]byte

// KeyOK is MEMORY_KEY, ^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$: a project or role
// a store may be named after. Unlike Python's `$`, a trailing newline is
// refused.
func KeyOK(s string) bool {
	if len(s) == 0 || len(s) > maxKey || !alnum(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !alnum(c) && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func alnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// nameError is memory_repo's ValueError.
func nameError(project, role string) error {
	return fmt.Errorf("no memory for %s/%s: %w", PyRepr(project), PyRepr(role), ErrName)
}

// PathOK is memory_path_ok (F:1054): valid UTF-8 of 1 to 300 characters, no
// control character, backslash, double quote or DEL, and no component that
// is empty, ".", ".." or ".git" in any case.
func PathOK(p string) bool {
	if !utf8.ValidString(p) {
		return false
	}
	if n := utf8.RuneCountInString(p); n == 0 || n > maxPath {
		return false
	}
	for _, r := range p {
		if r < 32 || r == 127 || r == '\\' || r == '"' {
			return false
		}
	}
	for _, part := range strings.Split(p, "/") {
		// casefold: no non-ASCII character folds to one of ".git"
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, ".git") && len(part) == 4 {
			return false
		}
	}
	return true
}

// InContext is in_context: the path is the context folder or inside it.
func InContext(p string) bool {
	return p == ContextTop || strings.HasPrefix(p, ContextTop+"/")
}

// Capped is capped() (F:1063): the files in path order, leaving out any
// that is also a directory of another and any that would take the total
// past MaxBytes. What is left out is appended to skipped.
func Capped(files Files, skipped []string) (Files, []string) {
	dirs := map[string]bool{}
	for p := range files {
		for i := 0; i < len(p); i++ {
			if p[i] == '/' {
				dirs[p[:i]] = true
			}
		}
	}
	out, total := Files{}, 0
	for _, p := range sortedPaths(files) {
		data := files[p]
		if dirs[p] || total+len(data) > MaxBytes {
			skipped = append(skipped, p)
			continue
		}
		out[p] = data
		total += len(data)
	}
	return out, skipped
}

// sortedPaths is sorted(files): byte order is code point order for UTF-8.
func sortedPaths(files Files) []string {
	out := make([]string, 0, len(files))
	for p := range files {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Size is the total of the files' lengths.
func Size(files Files) int {
	n := 0
	for _, d := range files {
		n += len(d)
	}
	return n
}

// BlobSHA is git's object name for a blob holding data (blob_sha).
func BlobSHA(data []byte) string {
	h := sha1.New()
	h.Write([]byte("blob " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// PyRepr is Python's repr() of a str, as the messages that name a path or a
// store use it. A byte that is not UTF-8 shows as the surrogate escape
// Python's file system decoding gives it (\udcXX).
func PyRepr(s string) string {
	q := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteRune(q)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&b, `\udc%02x`, s[i])
			i++
			continue
		}
		i += size
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == q:
			b.WriteRune('\\')
			b.WriteRune(q)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r >= 0x80 && !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S):
			switch {
			case r <= 0xff:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteRune(q)
	return b.String()
}

// decode is bytes.decode(errors="replace"): each byte that does not start
// a valid sequence becomes U+FFFD.
func decode(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var s strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		s.WriteRune(r)
		b = b[size:]
	}
	return s.String()
}
