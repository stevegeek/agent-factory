package security

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/stevegeek/agent-factory/internal/textx"
)

// Concerns are the levels an answer may give, lowest first (CONCERNS).
func Concerns() []string { return []string{"none", "low", "medium", "high"} }

func concernRank(c string) int {
	for i, k := range Concerns() {
		if c == k {
			return i
		}
	}
	return -1
}

// Report is what the outcome reads of a security agent's report.json.
type Report struct {
	// Verdict is the "verdict" value when it is a string ("" otherwise:
	// only the string "pass" counts).
	Verdict string
	// Answers is the "answers" list, nil when it is not a list.
	Answers []Answer
}

// Answer is one entry of "answers". Concern and Answer hold Python's
// str() of a.get("concern", "") and a.get("answer", ""): the text of a
// string, "" for a missing key, "None" for null, and the repr of anything
// else (which is never blank and never a concern level).
type Answer struct {
	Object  bool // the entry is a JSON object; nothing else is an answer
	Concern string
	Answer  string
}

// Result is the security stage's outcome.
type Result struct {
	Pass bool
	// Why is security_outcome's reason: "pass", or why it failed.
	Why string
	// Asked is the number of questions the answers were judged against.
	Asked int
	// Rule is the move's rule sentence: "security: tier N (name); K
	// questions, none above <worst>" on a pass, the reason after the comma
	// otherwise.
	Rule string
	// Detail is the move's detail: "" on a pass (the move to the gate has
	// none), "security: <why>" on a failure (to needs_human).
	Detail string
}

// Outcome is security_outcome and the rule finish_stage records: decided
// from the answers, not the agent's own verdict. tier is the ticket's
// security_tier (4 when it has none, as Python's t.get("security_tier", 4));
// frozen its security_questions.
//
// Every question asked needs an answer (an object whose concern is a
// known level and whose answer is not blank), none may be medium or high,
// and only then must the verdict be "pass".
func Outcome(tier int, frozen []string, r Report) Result {
	asked := len(Questions(frozen, tier))
	name, _ := TierName(tier)
	rule := fmt.Sprintf("security: tier %d (%s); %d questions, ", tier, name, asked)
	pass, why := decide(asked, r)
	res := Result{Pass: pass, Why: why, Asked: asked}
	if pass {
		res.Rule = rule + "none above " + MaxConcern(r, asked)
	} else {
		res.Rule, res.Detail = rule+why, "security: "+why
	}
	return res
}

func decide(asked int, r Report) (bool, string) {
	if len(r.Answers) < asked {
		return false, fmt.Sprintf("%d answers for %d questions", len(r.Answers), asked)
	}
	var worst []string
	for i, a := range r.Answers[:asked] {
		concern := ""
		if a.Object {
			concern = textx.Lower(a.Concern)
		}
		if concernRank(concern) < 0 || textx.Strip(a.Answer) == "" {
			return false, fmt.Sprintf("question %d has no usable answer", i+1)
		}
		if concern == "medium" || concern == "high" {
			worst = append(worst, fmt.Sprintf("q%d %s", i+1, concern))
		}
	}
	if len(worst) > 0 {
		return false, "problems found (" + strings.Join(worst, ", ") + ")"
	}
	if r.Verdict != "pass" {
		return false, "reviewer did not pass it"
	}
	return true, "pass"
}

// MaxConcern is max_concern(report, asked): the highest known concern
// among the first asked answers that are objects, "none" when there is
// none.
func MaxConcern(r Report, asked int) string {
	best := 0
	for i, a := range r.Answers {
		if i >= asked {
			break
		}
		if !a.Object {
			continue
		}
		if k := concernRank(textx.Lower(a.Concern)); k > best {
			best = k
		}
	}
	return Concerns()[best]
}

// ParseReport reads a report.json object into a Report, as Python's dict
// accesses read it. A duplicate key reads as its last value, as json.loads
// does.
func ParseReport(data []byte) (Report, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return Report{}, fmt.Errorf("security report: %w", err)
	}
	if dec.More() {
		return Report{}, errors.New("security report: data after the object")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return Report{}, errors.New("security report: not a JSON object")
	}
	var r Report
	r.Verdict, _ = m["verdict"].(string)
	list, ok := m["answers"].([]any)
	if !ok {
		return r, nil
	}
	r.Answers = make([]Answer, len(list))
	for i, e := range list {
		o, ok := e.(map[string]any)
		if !ok {
			continue
		}
		r.Answers[i] = Answer{Object: true, Concern: pyStr(o, "concern"), Answer: pyStr(o, "answer")}
	}
	return r, nil
}

// pyStr is str(o.get(key, "")) for a value encoding/json decoded, as far as
// the outcome can tell it apart: strings exactly, null as "None", booleans
// and numbers as Python prints them, and lists and objects as a non-blank
// stand-in for their repr.
func pyStr(o map[string]any, key string) string {
	v, ok := o[key]
	if !ok {
		return ""
	}
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	case json.Number:
		return x.String()
	case []any:
		return "[...]"
	default:
		return "{...}"
	}
}
