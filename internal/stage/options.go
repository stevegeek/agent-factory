package stage

import (
	"errors"
	"fmt"

	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// readOptions is a question report's select and options, held to
// ticket.CheckOptions. A report with no options has none, and no error; an
// error says why the options the report gave are not taken.
func readOptions(r Report) (string, []ticket.QuestionOption, error) {
	sel, okSel := r.Value("select")
	raw, okOpts := r.Value("options")
	if !okOpts || raw.IsNull() {
		return "", nil, nil
	}
	s := ""
	if okSel && !sel.IsNull() {
		var ok bool
		if s, ok = sel.Str(); !ok {
			return "", nil, errors.New("select is not a string")
		}
	}
	items, ok := raw.Array()
	if !ok {
		return "", nil, errors.New("options is not a list")
	}
	opts := make([]ticket.QuestionOption, 0, len(items))
	for i, it := range items {
		o, err := readOption(it)
		if err != nil {
			return "", nil, fmt.Errorf("option %d: %w", i+1, err)
		}
		opts = append(opts, o)
	}
	if err := ticket.CheckOptions(s, opts); err != nil {
		return "", nil, err
	}
	return s, opts, nil
}

func readOption(v jsonx.Value) (ticket.QuestionOption, error) {
	o, ok := v.Object()
	if !ok {
		return ticket.QuestionOption{}, errors.New("not an object")
	}
	str := func(key string) (string, error) {
		x, ok := o.Lookup(key)
		if !ok || x.IsNull() {
			return "", nil
		}
		s, ok := x.Str()
		if !ok {
			return "", fmt.Errorf("%s is not a string", key)
		}
		return s, nil
	}
	var out ticket.QuestionOption
	for _, f := range []struct {
		key string
		to  *string
	}{{"key", &out.Key}, {"label", &out.Label}, {"detail", &out.Detail}, {"effect", &out.Effect}, {"kind", &out.Kind}, {"then", &out.Then}} {
		s, err := str(f.key)
		if err != nil {
			return out, err
		}
		*f.to = s
	}
	out.Label, out.Detail = textx.Fold(out.Label), textx.Strip(out.Detail)
	if x, ok := o.Lookup("recommended"); ok && !x.IsNull() {
		b, ok := x.Bool()
		if !ok {
			return out, errors.New("recommended is not true or false")
		}
		out.Recommended = b
	}
	return out, nil
}
