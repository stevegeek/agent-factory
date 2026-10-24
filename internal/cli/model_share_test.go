package cli

import (
	"slices"
	"testing"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/tokens"
)

// tokens may not import model, so it keeps copies of two model
// lists. One definition of the model means the copies never
// drift: this test ties them together.
func TestTokensShareTheModel(t *testing.T) {
	var caps []string
	for _, c := range model.Capabilities() {
		caps = append(caps, string(c))
	}
	if got := tokens.Capabilities(); !slices.Equal(got, caps) {
		t.Errorf("tokens.Capabilities() = %v, model.Capabilities() = %v", got, caps)
	}
	if got, want := tokens.ReservedNames(), model.ReservedNames(); !slices.Equal(got, want) {
		t.Errorf("tokens.ReservedNames() = %v, model.ReservedNames() = %v", got, want)
	}
}
