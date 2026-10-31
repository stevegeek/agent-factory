package ticket

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func goodOptions() []QuestionOption {
	return []QuestionOption{
		{Key: "keep", Label: "Keep the old name", Recommended: true},
		{Key: "rename", Label: "Rename it", Detail: "Callers must change.", Effect: EffectClose},
		{Key: "later", Label: "Do it in a follow-up", Effect: EffectFile, Kind: "intent", Then: EffectContinue},
		{Key: "no", Label: "Not at all", Effect: EffectDrop},
	}
}

func TestAQuestionWithOptionsRoundTrips(t *testing.T) {
	tk := New("T0040", newIn("p", "x", "", "human", "needs_human", ""), testNow)
	tk.Question = Some(Question{Text: "Which name?", Stage: "work", Agent: "t0040-w1", At: "2026-10-28T10:00:00Z",
		Options: goodOptions(), Select: SelectOne, Chosen: []string{"later"}, Effect: EffectFile, Filed: "T0041"})
	tk.History = append(tk.History, Entry{At: "2026-10-28T10:01:00Z", Event: "chose", Detail: "chose later",
		Type: OptionChosen{}.EventType(), Data: OptionChosen{By: "maint", Keys: []string{"later"}, Effect: EffectFile, Filed: "T0041"}})
	data := Encode(tk)
	back, problems, err := Decode(data)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	if got, want := back.Question.Or(Question{}), tk.Question.Or(Question{}); !reflect.DeepEqual(got, want) {
		t.Errorf("question\n got %+v\nwant %+v", got, want)
	}
	if got := back.History[len(back.History)-1].Data; !reflect.DeepEqual(got, tk.History[len(tk.History)-1].Data) {
		t.Errorf("chosen event %+v", got)
	}
	if again := Encode(back); !bytes.Equal(again, data) {
		t.Error("a second encode differs")
	}
}

func TestAQuestionWithoutOptionsWritesNoNewKeys(t *testing.T) {
	tk := New("T0040", newIn("p", "x", "", "human", "needs_human", ""), testNow)
	tk.Question = Some(Question{Text: "Which name?", Stage: "work", Agent: "t0040-w1", At: "2026-10-28T10:00:00Z"})
	s := string(Encode(tk))
	for _, k := range []string{"options", "select", "chosen", "effect", "filed"} {
		if strings.Contains(s, `"`+k+`"`) {
			t.Errorf("%q written for a question with no options", k)
		}
	}
}

func TestCheckOptions(t *testing.T) {
	if err := CheckOptions("", goodOptions()); err != nil {
		t.Fatalf("good options refused: %v", err)
	}
	many := []QuestionOption{{Key: "a", Label: "A"}, {Key: "b", Label: "B", Effect: EffectContinue}}
	if err := CheckOptions(SelectMany, many); err != nil {
		t.Fatalf("many continue options refused: %v", err)
	}
	change := func(f func(o []QuestionOption) []QuestionOption) []QuestionOption { return f(goodOptions()) }
	for name, tc := range map[string]struct {
		sel  string
		opts []QuestionOption
	}{
		"one option":      {"", goodOptions()[:1]},
		"six options":     {"", append(goodOptions(), QuestionOption{Key: "x", Label: "X"}, QuestionOption{Key: "y", Label: "Y"})},
		"duplicate keys":  {"", change(func(o []QuestionOption) []QuestionOption { o[1].Key = "keep"; return o })},
		"upper-case key":  {"", change(func(o []QuestionOption) []QuestionOption { o[0].Key = "Keep"; return o })},
		"long key":        {"", change(func(o []QuestionOption) []QuestionOption { o[0].Key = strings.Repeat("k", 21); return o })},
		"empty label":     {"", change(func(o []QuestionOption) []QuestionOption { o[0].Label = ""; return o })},
		"long label":      {"", change(func(o []QuestionOption) []QuestionOption { o[0].Label = strings.Repeat("é", 121); return o })},
		"two-line label":  {"", change(func(o []QuestionOption) []QuestionOption { o[0].Label = "a\nb"; return o })},
		"long detail":     {"", change(func(o []QuestionOption) []QuestionOption { o[0].Detail = strings.Repeat("d", 1001); return o })},
		"two recommended": {"", change(func(o []QuestionOption) []QuestionOption { o[1].Recommended = true; return o })},
		"unknown effect":  {"", change(func(o []QuestionOption) []QuestionOption { o[0].Effect = "merge"; return o })},
		"file, no then":   {"", change(func(o []QuestionOption) []QuestionOption { o[2].Then = ""; return o })},
		"file, bad kind":  {"", change(func(o []QuestionOption) []QuestionOption { o[2].Kind = "task"; return o })},
		"close with then": {"", change(func(o []QuestionOption) []QuestionOption { o[1].Then = EffectClose; return o })},
		"many and close":  {SelectMany, goodOptions()},
		"unknown select":  {"some", goodOptions()},
	} {
		if err := CheckOptions(tc.sel, tc.opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
