package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/deploy"
	"github.com/stevegeek/agent-factory/internal/prflow"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// fakeOffer records Offer's arguments, and fails when err is set.
type fakeOffer struct {
	got []string
	err error
}

func (o *fakeOffer) Offer(_ context.Context, cfg *config.Config, a deploy.Asker, rev, why, by string, hold bool) error {
	held := "go"
	if hold {
		held = "hold"
	}
	if cfg == nil || a == nil {
		return errors.New("no config or asker")
	}
	o.got = append(o.got, strings.Join([]string{rev, why, by, held}, " | "))
	return o.err
}

type fakeNotices struct{ got []string }

func (n *fakeNotices) Notice(_ context.Context, title, body string) error {
	n.got = append(n.got, title+"\n"+body)
	return nil
}

type nopAsks struct{}

func (nopAsks) DeployAsk(context.Context, string, string) error { return nil }

func mergedTicket(n int) *ticket.Ticket {
	return &ticket.Ticket{ID: "T0007", PR: ticket.Some(ticket.PR{Number: n})}
}

// A core merge and a config merge both offer main (every deploy is core
// main with config main); the why names the PR, the project and the merge.
func TestSelfDeploysRequestsCoreAndConfig(t *testing.T) {
	o := &fakeOffer{}
	s := selfDeploys{Config: &config.Config{}, Deploy: o, Asks: nopAsks{}, Notices: &fakeNotices{}}
	core := config.Project{Key: "agent-factory", SelfDeploy: config.SelfDeployCore}
	conf := config.Project{Key: "factory-config", SelfDeploy: config.SelfDeployConfig}
	if err := s.Merged(context.Background(), core, mergedTicket(12), prflow.MergedPR{Commit: strings.Repeat("a", 40)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Merged(context.Background(), conf, mergedTicket(3), prflow.MergedPR{Commit: strings.Repeat("b", 40), Hold: true}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"main | PR #12 merged (agent-factory aaaaaaaaaaaa) | factory | go",
		"main | PR #3 merged (factory-config bbbbbbbbbbbb) | factory | hold",
	}
	if !slices.Equal(o.got, want) {
		t.Errorf("offers = %q, want %q", o.got, want)
	}
}

func TestSelfDeploysFailureFilesNotice(t *testing.T) {
	o := &fakeOffer{err: errors.New("no user bus")}
	n := &fakeNotices{}
	s := selfDeploys{Config: &config.Config{}, Deploy: o, Asks: nopAsks{}, Notices: n}
	p := config.Project{Key: "agent-factory", SelfDeploy: config.SelfDeployCore}
	err := s.Merged(context.Background(), p, mergedTicket(12), prflow.MergedPR{Commit: strings.Repeat("a", 40)})
	if err == nil || !strings.Contains(err.Error(), "no user bus") {
		t.Fatalf("err = %v", err)
	}
	// Start writes the request before it fails for want of a user bus: the
	// notice does not say there is none
	if len(n.got) != 1 || !strings.HasPrefix(n.got[0], "Deploy not started: agent-factory aaaaaaaaaaaa\n") ||
		!strings.Contains(n.got[0], "no user bus") || !strings.Contains(n.got[0], "T0007") ||
		!strings.Contains(n.got[0], "A request may be waiting: `factory deploy status` shows it and `factory deploy run` runs it.") {
		t.Errorf("notices = %q", n.got)
	}
}
