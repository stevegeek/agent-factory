package mcp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// jschema is a tool's input schema: both what tools/list shows and what is
// checked before a verb sees a call, so the two can't drift. Only what the
// tools use.
type jschema struct {
	typ        string // object, string, integer, boolean, array
	nullable   bool
	desc       string
	props      map[string]*jschema
	required   []string
	maxLen     int
	pattern    *regexp.Regexp
	format     string // date-time
	enum       []string
	min, max   *int
	items      *jschema
	minN, maxN int // array length; maxN 0: no limit
}

// JSON is the schema as tools/list shows it.
func (s *jschema) JSON() map[string]any {
	out := map[string]any{"type": s.typ}
	if s.nullable {
		out["type"] = []string{s.typ, "null"}
	}
	if s.desc != "" {
		out["description"] = s.desc
	}
	switch s.typ {
	case "object":
		props := map[string]any{}
		for k, p := range s.props {
			props[k] = p.JSON()
		}
		out["properties"] = props
		out["additionalProperties"] = false
		if len(s.required) > 0 {
			out["required"] = s.required
		}
	case "string":
		if s.maxLen > 0 {
			out["maxLength"] = s.maxLen
		}
		if s.pattern != nil {
			out["pattern"] = s.pattern.String()
		}
		if s.format != "" {
			out["format"] = s.format
		}
		if s.enum != nil {
			out["enum"] = s.enum
		}
	case "integer":
		if s.min != nil {
			out["minimum"] = *s.min
		}
		if s.max != nil {
			out["maximum"] = *s.max
		}
	case "array":
		out["items"] = s.items.JSON()
		if s.minN > 0 {
			out["minItems"] = s.minN
		}
		if s.maxN > 0 {
			out["maxItems"] = s.maxN
		}
	}
	return out
}

// check is "" when v fits the schema, else what is wrong with it (at path).
// Objects are checked in sorted key order, so the first failure is always
// the same one.
func (s *jschema) check(v any, path string) string {
	at := func(msg string) string {
		if path == "" {
			return msg
		}
		return path + " " + msg
	}
	if v == nil {
		if s.nullable {
			return ""
		}
		return at("may not be null")
	}
	switch s.typ {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return at("must be an object")
		}
		for _, k := range sortedKeys(obj) {
			p, known := s.props[k]
			if !known {
				return "unknown argument " + strconv.Quote(clip(k, 40)) + atPath(path)
			}
			if msg := p.check(obj[k], subPath(path, k)); msg != "" {
				return msg
			}
		}
		for _, k := range s.required {
			if _, ok := obj[k]; !ok {
				return subPath(path, k) + " is needed"
			}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return at("must be a string")
		}
		if s.maxLen > 0 && utf8.RuneCountInString(str) > s.maxLen {
			return at(fmt.Sprintf("is at most %d characters", s.maxLen))
		}
		if s.pattern != nil && !s.pattern.MatchString(str) {
			return at("must match " + s.pattern.String())
		}
		if s.enum != nil && !slices.Contains(s.enum, str) {
			return at("is one of " + strings.Join(s.enum, ", "))
		}
		if s.format == "date-time" {
			if _, err := time.Parse(time.RFC3339, str); err != nil {
				return at("must be an RFC 3339 date-time")
			}
		}
	case "integer":
		n, ok := v.(json.Number)
		i, err := strconv.ParseInt(string(n), 10, 64)
		if !ok || err != nil {
			return at("must be an integer")
		}
		if (s.min != nil && i < int64(*s.min)) || (s.max != nil && i > int64(*s.max)) {
			return at(fmt.Sprintf("is %s", rangeWords(s.min, s.max)))
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return at("must be true or false")
		}
	case "array":
		xs, ok := v.([]any)
		if !ok {
			return at("must be an array")
		}
		if len(xs) < s.minN || (s.maxN > 0 && len(xs) > s.maxN) {
			if s.maxN > 0 {
				return at(fmt.Sprintf("holds %d to %d items", s.minN, s.maxN))
			}
			return at(fmt.Sprintf("holds at least %d items", s.minN))
		}
		for i, x := range xs {
			if msg := s.items.check(x, fmt.Sprintf("%s[%d]", path, i)); msg != "" {
				return msg
			}
		}
	}
	return ""
}

func atPath(path string) string {
	if path == "" {
		return ""
	}
	return " in " + path
}

func subPath(path, k string) string {
	if path == "" {
		return k
	}
	return path + "." + k
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func rangeWords(lo, hi *int) string {
	switch {
	case lo != nil && hi != nil:
		return fmt.Sprintf("%d to %d", *lo, *hi)
	case lo != nil:
		return fmt.Sprintf("at least %d", *lo)
	}
	return fmt.Sprintf("at most %d", *hi)
}

func intp(n int) *int { return &n }

func sStr(max int, desc string) *jschema { return &jschema{typ: "string", maxLen: max, desc: desc} }
func sInt(lo, hi int, desc string) *jschema {
	return &jschema{typ: "integer", min: intp(lo), max: intp(hi), desc: desc}
}
func sBool(desc string) *jschema { return &jschema{typ: "boolean", desc: desc} }
func sEnum(desc string, vals ...string) *jschema {
	return &jschema{typ: "string", enum: vals, desc: desc}
}
func sArr(items *jschema, lo, hi int, desc string) *jschema {
	return &jschema{typ: "array", items: items, minN: lo, maxN: hi, desc: desc}
}
func sObj(props map[string]*jschema, required ...string) *jschema {
	return &jschema{typ: "object", props: props, required: required}
}

// The patterns the tools' arguments must match (copied from Foreman).
var (
	// re: go
	reTicketID = regexp.MustCompile(`^T[0-9]{4,6}$`)
	// re: go
	reLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	// re: go
	reIdemKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	// re: go
	reSplitKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,120}$`)
)

func sID(desc string) *jschema {
	return &jschema{typ: "string", pattern: reTicketID, desc: desc}
}
func sProject(desc string) *jschema {
	return &jschema{typ: "string", maxLen: 64, pattern: projectName, desc: desc}
}
func sLabels(desc string) *jschema {
	return sArr(&jschema{typ: "string", pattern: reLabel}, 0, 20, desc)
}
