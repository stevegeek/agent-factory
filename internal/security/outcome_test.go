package security

import (
	"testing"
)

func answer(concern string) Answer { return Answer{Object: true, Concern: concern, Answer: "evidence"} }

func TestOutcome(t *testing.T) {
	two := []string{"q1", "q2"}
	cases := []struct {
		name      string
		tier      int
		questions []string
		report    Report
		pass      bool
		why, rule string
	}{
		// port: TF TransitionTest.test_security_is_decided_from_every_answer_not_the_verdict
		{name: "none and low pass", tier: 4, questions: two,
			report: Report{Verdict: "pass", Answers: []Answer{answer("none"), answer("low")}}, pass: true, why: "pass",
			// port: TF AskTest.test_security_pass_records_tier_and_worst_concern
			rule: "security: tier 4 (input reaches a sink, and build or dependencies); 2 questions, none above low"},
		{name: "a medium concern fails", tier: 4, questions: two,
			report: Report{Verdict: "pass", Answers: []Answer{answer("none"), answer("medium")}},
			why:    "problems found (q2 medium)",
			rule:   "security: tier 4 (input reaches a sink, and build or dependencies); 2 questions, problems found (q2 medium)"},
		{name: "too few answers", tier: 4, questions: two,
			report: Report{Verdict: "pass", Answers: []Answer{answer("none")}}, why: "1 answers for 2 questions"},
		{name: "an unknown concern is no usable answer", tier: 4, questions: two,
			report: Report{Verdict: "pass", Answers: []Answer{answer("none"), answer("fine")}}, why: "question 2 has no usable answer"},
		// port: TF TransitionTest.test_security_fail_needs_human
		{name: "no answers to the tier's seven questions", tier: 4,
			report: Report{Verdict: "fail"}, why: "0 answers for 7 questions",
			rule: "security: tier 4 (input reaches a sink, and build or dependencies); 7 questions, 0 answers for 7 questions"},
		// every answer clean, but the agent did not pass it.
		{name: "reviewer did not pass it", tier: 1, questions: two,
			report: Report{Verdict: "fail", Answers: []Answer{answer("none"), answer("low")}}, why: "reviewer did not pass it",
			rule: "security: tier 1 (plain code); 2 questions, reviewer did not pass it"},
		{name: "no verdict", tier: 0, questions: []string{"q"},
			report: Report{Answers: []Answer{answer("none")}}, why: "reviewer did not pass it"},
		{name: "every bad concern is named, in order", tier: 2, questions: []string{"a", "b", "c"},
			report: Report{Verdict: "pass", Answers: []Answer{answer("high"), answer("low"), answer("MEDIUM")}},
			why:    "problems found (q1 high, q3 medium)"},
		{name: "an unusable answer is reported before a concern", tier: 2, questions: two,
			report: Report{Verdict: "pass", Answers: []Answer{answer("high"), {Object: false}}}, why: "question 2 has no usable answer"},
		{name: "a blank answer is no usable answer", tier: 0, questions: []string{"q"},
			report: Report{Verdict: "pass", Answers: []Answer{{Object: true, Concern: "none", Answer: " \t　"}}},
			why:    "question 1 has no usable answer"},
		{name: "answers past the questions are not read", tier: 0, questions: []string{"q"},
			report: Report{Verdict: "pass", Answers: []Answer{answer("low"), answer("high"), {}}}, pass: true, why: "pass",
			rule: "security: tier 0 (text only); 1 questions, none above low"},
		{name: "an unknown tier has no name", tier: 9, questions: []string{"q"},
			report: Report{Verdict: "pass", Answers: []Answer{answer("none")}}, pass: true, why: "pass",
			rule: "security: tier 9 (?); 1 questions, none above none"},
		{name: "a concern in capitals counts", tier: 0, questions: []string{"q"},
			report: Report{Verdict: "pass", Answers: []Answer{answer("LOW")}}, pass: true, why: "pass",
			rule: "security: tier 0 (text only); 1 questions, none above low"},
		{name: "a concern is not stripped", tier: 0, questions: []string{"q"},
			report: Report{Verdict: "pass", Answers: []Answer{answer(" none")}}, why: "question 1 has no usable answer"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Outcome(c.tier, c.questions, c.report)
			if got.Pass != c.pass || got.Why != c.why {
				t.Errorf("Outcome = %v %q, want %v %q", got.Pass, got.Why, c.pass, c.why)
			}
			if c.rule != "" && got.Rule != c.rule {
				t.Errorf("rule %q\nwant %q", got.Rule, c.rule)
			}
			if got.Pass && got.Detail != "" || !got.Pass && got.Detail != "security: "+c.why {
				t.Errorf("detail %q", got.Detail)
			}
		})
	}
}

func TestMaxConcern(t *testing.T) {
	r := Report{Answers: []Answer{answer("low"), {Object: false, Concern: "high"}, answer("Medium"), answer("bogus"), answer("high")}}
	for _, c := range []struct {
		asked int
		want  string
	}{{0, "none"}, {1, "low"}, {2, "low"}, {3, "medium"}, {4, "medium"}, {5, "high"}, {9, "high"}} {
		if got := MaxConcern(r, c.asked); got != c.want {
			t.Errorf("MaxConcern(asked %d) = %q, want %q", c.asked, got, c.want)
		}
	}
}

// ParseReport reads a report as Python's dict accesses do: str() of
// whatever the agent wrote, so a null concern is "None", which lowers to
// "none" (a Python quirk the port keeps).
func TestParseReport(t *testing.T) {
	cases := []struct {
		name string
		json string
		want passWhy
	}{
		{"strings", `{"verdict":"pass","answers":[{"concern":"none","answer":"x"}]}`, passWhy{true, "pass"}},
		{"a null concern reads as None", `{"verdict":"pass","answers":[{"concern":null,"answer":"x"}]}`, passWhy{true, "pass"}},
		{"a null answer reads as None", `{"verdict":"pass","answers":[{"concern":"low","answer":null}]}`, passWhy{true, "pass"}},
		{"a number is an answer", `{"verdict":"pass","answers":[{"concern":"low","answer":0}]}`, passWhy{true, "pass"}},
		{"an empty list is an answer", `{"verdict":"pass","answers":[{"concern":"low","answer":[]}]}`, passWhy{true, "pass"}},
		{"no answer key", `{"verdict":"pass","answers":[{"concern":"low"}]}`, passWhy{false, "question 1 has no usable answer"}},
		{"a boolean concern", `{"verdict":"pass","answers":[{"concern":true,"answer":"x"}]}`, passWhy{false, "question 1 has no usable answer"}},
		{"a string answer", `{"verdict":"pass","answers":["none"]}`, passWhy{false, "question 1 has no usable answer"}},
		{"answers not a list", `{"verdict":"pass","answers":{"concern":"none"}}`, passWhy{false, "0 answers for 1 questions"}},
		{"a verdict that is not a string", `{"verdict":true,"answers":[{"concern":"none","answer":"x"}]}`, passWhy{false, "reviewer did not pass it"}},
		{"the last of duplicate keys", `{"verdict":"fail","verdict":"pass","answers":[{"concern":"high","concern":"low","answer":"x"}]}`, passWhy{true, "pass"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := ParseReport([]byte(c.json))
			if err != nil {
				t.Fatal(err)
			}
			got := Outcome(0, []string{"q"}, r)
			if got.Pass != c.want.Pass || got.Why != c.want.Why {
				t.Errorf("Outcome = %v %q, want %v %q", got.Pass, got.Why, c.want.Pass, c.want.Why)
			}
		})
	}
	for _, bad := range []string{`[]`, `"x"`, `{`, ``} {
		if _, err := ParseReport([]byte(bad)); err == nil {
			t.Errorf("ParseReport(%q) is not an error", bad)
		}
	}
}

// passWhy is the part of a Result the ParseReport table checks.
type passWhy struct {
	Pass bool
	Why  string
}
