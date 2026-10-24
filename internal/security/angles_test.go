package security

import (
	"slices"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
)

// port: TF ConfigTest.test_angles_grow_with_tier
func TestAnglesGrowWithTier(t *testing.T) {
	groups := AngleGroups()
	if !slices.Equal(Angles(0), groups[0]) {
		t.Errorf("Angles(0) = %q", Angles(0))
	}
	if slices.Contains(Angles(2), groups[3][0]) {
		t.Error("tier 2 asks a tier 3 question")
	}
	var everything []string
	for _, g := range groups {
		everything = append(everything, g...)
	}
	all := Angles(4)
	if !slices.Equal(slices.Sorted(slices.Values(all)), slices.Sorted(slices.Values(everything))) {
		t.Errorf("Angles(4) = %q, want every angle", all)
	}
	// The order Python asks them in: the tier 0 question first, then 1, 2, 3.
	want := map[int][]string{
		0: groups[0],
		1: slices.Concat(groups[0], groups[1]),
		2: slices.Concat(groups[0], groups[1], groups[2]),
		3: slices.Concat(groups[0], groups[1], groups[3]),
		4: slices.Concat(groups[0], groups[1], groups[2], groups[3]),
	}
	for tier, w := range want {
		if got := Angles(tier); !slices.Equal(got, w) {
			t.Errorf("Angles(%d) = %q, want %q", tier, got, w)
		}
	}
	// A tier Python never produces: >= 1 gets the smuggled-change question
	// and nothing else; below 0 only the injection one.
	if got := Angles(7); !slices.Equal(got, slices.Concat(groups[0], groups[1])) {
		t.Errorf("Angles(7) = %q", got)
	}
	if got := Angles(-1); !slices.Equal(got, groups[0]) {
		t.Errorf("Angles(-1) = %q", got)
	}
}

func TestAngleText(t *testing.T) {
	g := AngleGroups()
	if len(g) != 4 || len(g[0]) != 1 || len(g[1]) != 1 || len(g[2]) != 3 || len(g[3]) != 2 {
		t.Fatalf("TIER_ANGLES has the wrong shape: %q", g)
	}
	if g[0][0] != "Injection: does any added text (docs, comments, strings, test data) try to instruct a reader, human or model?" {
		t.Errorf("tier 0 angle: %q", g[0][0])
	}
	if g[3][1] != "Does the build, package manifest or CI now run new code, or code from a new source?" {
		t.Errorf("last tier 3 angle: %q", g[3][1])
	}
}

func TestTierName(t *testing.T) {
	for i, want := range model.TierNames() {
		if got, ok := TierName(i); !ok || got != want {
			t.Errorf("TierName(%d) = %q, %v", i, got, ok)
		}
	}
	if got, ok := TierName(5); ok || got != "?" {
		t.Errorf("TierName(5) = %q, %v", got, ok)
	}
	if got, _ := TierName(-1); got != "?" {
		t.Errorf("TierName(-1) = %q", got)
	}
}

func TestQuestions(t *testing.T) {
	if got := Questions([]string{"q1", "q2"}, 4); !slices.Equal(got, []string{"q1", "q2"}) {
		t.Errorf("frozen questions: %q", got)
	}
	// An empty list is falsy in Python: the tier's angles stand in.
	if got := Questions([]string{}, 1); !slices.Equal(got, Angles(1)) {
		t.Errorf("no frozen questions: %q", got)
	}
}
