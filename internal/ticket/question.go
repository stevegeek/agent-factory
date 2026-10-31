package ticket

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/model"
)

// The effects of an agent's question's option: what choosing it does.
const (
	EffectContinue = "continue" // a new run of the asking stage carries on with the choice
	EffectClose    = "close"    // the ticket closes as done, with the option as its result
	EffectDrop     = "drop"     // the ticket is dropped
	EffectFile     = "file"     // the option is filed as a new ticket, then Then
)

// How many of a question's options may be chosen (Question.Select).
const (
	SelectOne  = "one"
	SelectMany = "many"
)

// The limits CheckOptions holds a question's options to.
const (
	MinOptions      = 2
	MaxOptions      = 5
	MaxOptionKey    = 20   // bytes of a key, from [a-z0-9_-]
	MaxOptionLabel  = 120  // characters of a label, on one line
	MaxOptionDetail = 1000 // characters of a detail
)

// EffectOf is what choosing o does.
func (o QuestionOption) EffectOf() string {
	if o.Effect == "" {
		return EffectContinue
	}
	return o.Effect
}

// KindOf is the kind of ticket a file effect files o as.
func (o QuestionOption) KindOf() string {
	if o.Kind == "" {
		return string(model.KindChange)
	}
	return o.Kind
}

// Many reports a question whose answer may choose several options.
func (q Question) Many() bool { return q.Select == SelectMany }

// Option is the option of q with key.
func (q Question) Option(key string) (QuestionOption, bool) {
	for _, o := range q.Options {
		if o.Key == key {
			return o, true
		}
	}
	return QuestionOption{}, false
}

// CheckOptions says why the options an agent offered, with sel as its
// select, are not ones the factory takes; nil when they are.
func CheckOptions(sel string, opts []QuestionOption) error {
	if sel != "" && sel != SelectOne && sel != SelectMany {
		return fmt.Errorf("select is %q, not one or many", sel)
	}
	if len(opts) < MinOptions || len(opts) > MaxOptions {
		return fmt.Errorf("%d options, not %d to %d", len(opts), MinOptions, MaxOptions)
	}
	keys := map[string]bool{}
	recommended := 0
	for i, o := range opts {
		n := i + 1
		if !optionKey(o.Key) {
			return fmt.Errorf("option %d: the key %q is not 1 to %d of a-z, 0-9, _ and -", n, o.Key, MaxOptionKey)
		}
		if keys[o.Key] {
			return fmt.Errorf("option %d: the key %q is used twice", n, o.Key)
		}
		keys[o.Key] = true
		if l := utf8.RuneCountInString(o.Label); l == 0 || l > MaxOptionLabel || strings.ContainsAny(o.Label, "\r\n") {
			return fmt.Errorf("option %d: the label is not one line of 1 to %d characters", n, MaxOptionLabel)
		}
		if utf8.RuneCountInString(o.Detail) > MaxOptionDetail {
			return fmt.Errorf("option %d: the detail is over %d characters", n, MaxOptionDetail)
		}
		if o.Recommended {
			recommended++
		}
		if err := checkEffect(o); err != nil {
			return fmt.Errorf("option %d: %w", n, err)
		}
		if sel == SelectMany && o.EffectOf() != EffectContinue {
			return fmt.Errorf("option %d: select many takes only continue options, not %s", n, o.EffectOf())
		}
	}
	if recommended > 1 {
		return fmt.Errorf("%d options are recommended, not one", recommended)
	}
	return nil
}

func checkEffect(o QuestionOption) error {
	switch o.EffectOf() {
	case EffectFile:
		if o.Then != EffectContinue && o.Then != EffectClose {
			return errors.New("a file effect needs then: continue or close")
		}
		if k := o.KindOf(); k != string(model.KindChange) && k != string(model.KindIntent) {
			return fmt.Errorf("a file effect files a change or an intent, not %q", o.Kind)
		}
	case EffectContinue, EffectClose, EffectDrop:
		if o.Kind != "" || o.Then != "" {
			return fmt.Errorf("a %s effect takes no kind or then", o.EffectOf())
		}
	default:
		return fmt.Errorf("the effect %q is not continue, close, drop or file", o.Effect)
	}
	return nil
}

func optionKey(k string) bool {
	if k == "" || len(k) > MaxOptionKey {
		return false
	}
	for _, r := range k {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
