package ticket

import (
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/model"
)

// hunt is a bug hunt the verifier has checked: three findings, the second a
// behaviour change, the third a duplicate.
func hunt(t *testing.T) *Ticket {
	t.Helper()
	in := newIn("idcodec", "Bug hunt", "Look for bugs.", "clock", "ready", "")
	in.Fields = fields(t, `{"kind": "task"}`)
	tk := New("T0030", in, testNow)
	tk.Findings = []Finding{
		{Title: "Raise IdCodecFormatError for nil input in decode", Severity: "low", Kind: "bug", Verdict: VerdictConfirmed,
			Evidence: "decode(nil) raised NoMethodError", Body: "`decode(nil)` raises NoMethodError."},
		{Title: "Preserve leading zeros in the hex round trip", Severity: "medium", Kind: "bug", BehaviourChange: true,
			Verdict: VerdictConfirmed, Evidence: "decode_hex(encode_hex(\"00ff\")) == [\"ff\"]", Body: "Leading zeros are lost."},
		{Title: "Make decode_hex errors consistent", Severity: "low", Kind: "bug", Verdict: VerdictDuplicate, Duplicate: "PR #27",
			Evidence: "PR #27 changes the same lines", Body: "decode_hex has no max_length guard."},
	}
	Move(tk, string(model.ColNeedsHuman), model.ReasonFindingsVerified, "findings verified: 2 of 3 confirmed", "", "", testNow)
	return tk
}

func TestAVerifiedHuntAsksWhichFindingsToFile(t *testing.T) {
	tk := hunt(t)
	a, ok := tk.Ask.Get()
	if !ok || a.Kind != string(model.AskFindings) {
		t.Fatalf("ask = %+v", tk.Ask)
	}
	if !strings.Contains(a.Question, "2 confirmed findings") || !strings.Contains(a.Question, "“Bug hunt”") {
		t.Errorf("question %q", a.Question)
	}
	// a verifier that could not finish is a step to retry, from verifying
	tk2 := hunt(t)
	tk2.Findings = nil
	Move(tk2, string(model.ColVerifying), model.ReasonVerifyRequested, "", "", "", testNow)
	Move(tk2, string(model.ColNeedsHuman), model.ReasonVerifierBlocked, "verifier blocked: no checkout", "", "", testNow)
	b := tk2.Ask.V
	if b.Kind != string(model.AskUnstick) || b.RetryColumn.Or("") != string(model.ColVerifying) || !strings.Contains(b.Question, "verify step") {
		t.Errorf("blocked verifier ask = %+v", b)
	}
	// with every confirmed finding filed there is nothing left to ask
	tk3 := hunt(t)
	tk3.Findings[0].Filed, tk3.Findings[1].Filed = "T0040", "T0041"
	if a := AskFor(tk3, "verifying", "", clock.Stamp(testNow), model.ReasonFindingsVerified); a == nil || a.Kind != string(model.AskFindings) ||
		!strings.Contains(a.Question, "nothing left") {
		t.Errorf("all filed: %+v", a)
	}
}

func TestFileFindingMakesALinkedUntrustedTicket(t *testing.T) {
	tk := hunt(t)
	c, err := FileFinding("T0040", tk, 1, string(model.KindChange), "maintainer", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if c.Project != "idcodec" || c.Door != string(model.DoorFinding) || c.Column != string(model.ColTriage) || c.Kind.Present() {
		t.Errorf("change: project %q door %q column %q kind %v", c.Project, c.Door, c.Column, c.Kind)
	}
	if c.Title != "Raise IdCodecFormatError for nil input in decode" || c.Parent.Or("") != "T0030" {
		t.Errorf("title %q parent %v", c.Title, c.Parent)
	}
	for _, want := range []string{"`decode(nil)` raises NoMethodError.", "decode(nil) raised NoMethodError", "Found by T0030 (Bug hunt)", "Severity: low"} {
		if !strings.Contains(c.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, c.Body)
		}
	}
	if r, ok := c.FoundIn.Get(); !ok || r.Ticket != "T0030" || r.N != 1 {
		t.Errorf("found_in = %+v", c.FoundIn)
	}
	if f := tk.Findings[0]; f.Filed != "T0040" || f.FiledAs != string(model.KindChange) {
		t.Errorf("finding 1 after filing: %+v", f)
	}
	if len(tk.Children) != 1 || tk.Children[0] != "T0040" {
		t.Errorf("children %v", tk.Children)
	}
	if e := tk.History[len(tk.History)-1]; e.Event != "filed" || !strings.HasPrefix(e.Detail, "T0040: finding 1, a change: ") {
		t.Errorf("last event %+v", e)
	}
	// an intent goes to its designer, through the same door
	i, err := FileFinding("T0041", tk, 2, string(model.KindIntent), "maintainer", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if i.Column != string(model.ColReady) || i.KindName() != string(model.KindIntent) || i.Door != string(model.DoorFinding) {
		t.Errorf("intent: column %q kind %q door %q", i.Column, i.KindName(), i.Door)
	}
	// never twice, never an unconfirmed one, never an unknown kind or number
	if _, err := FileFinding("T0042", tk, 1, string(model.KindChange), "maintainer", testNow); err == nil {
		t.Error("finding 1 filed twice")
	}
	if _, err := FileFinding("T0042", tk, 3, string(model.KindChange), "maintainer", testNow); err == nil {
		t.Error("a duplicate was filed")
	}
	if _, err := FileFinding("T0042", tk, 4, string(model.KindChange), "maintainer", testNow); err == nil {
		t.Error("finding 4 does not exist")
	}
	if _, err := FileFinding("T0042", tk, 2, string(model.KindTask), "maintainer", testNow); err == nil {
		t.Error("a finding is filed as a change or an intent only")
	}
	if len(tk.Children) != 2 {
		t.Errorf("children %v", tk.Children)
	}
}

func TestClosingAHuntListsWhatItFiled(t *testing.T) {
	tk := hunt(t)
	if _, err := FileFinding("T0040", tk, 1, string(model.KindChange), "maintainer", testNow); err != nil {
		t.Fatal(err)
	}
	if _, err := FileFinding("T0041", tk, 2, string(model.KindIntent), "maintainer", testNow); err != nil {
		t.Fatal(err)
	}
	CloseFindings(tk, "maintainer", "", testNow)
	m, _ := tk.History[len(tk.History)-1].Data.(Moved)
	if tk.Column != string(model.ColDone) || m.Reason != model.ReasonFindingsFiled || m.By != "maintainer" {
		t.Errorf("closed in %s by %+v", tk.Column, m)
	}
	if d := tk.History[len(tk.History)-1].Detail; !strings.HasSuffix(d, "findings filed: T0040 (change), T0041 (intent)") {
		t.Errorf("detail %q", d)
	}
	none := hunt(t)
	CloseFindings(none, "maintainer", "", testNow)
	m, _ = none.History[len(none.History)-1].Data.(Moved)
	if none.Column != string(model.ColDone) || m.Reason != model.ReasonFindingsDismissed {
		t.Errorf("nothing filed: %s %+v", none.Column, m)
	}
}

func TestFindingsSurviveARoundTrip(t *testing.T) {
	tk := hunt(t)
	tk.FindingsAsked = Some(true)
	if _, err := FileFinding("T0040", tk, 1, string(model.KindChange), "maintainer", testNow); err != nil {
		t.Fatal(err)
	}
	back, problems, err := Decode(Encode(tk))
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	if len(back.Findings) != 3 || back.Findings[0].Filed != "T0040" || !back.Findings[1].BehaviourChange || back.Findings[2].Duplicate != "PR #27" ||
		!back.FindingsAsked.Or(false) {
		t.Errorf("after a round trip: %+v", back.Findings)
	}
	if got := back.Findings[1].DefaultKind(); got != string(model.KindIntent) {
		t.Errorf("a behaviour change is designed first: %q", got)
	}
	if got := back.Findings[0].DefaultKind(); got != string(model.KindChange) {
		t.Errorf("anything else is a change: %q", got)
	}
}
