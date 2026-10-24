// Package onboard checks whether a factory project is ready to switch on:
// its kind, its commands, its checkout, its context, the App's reach, the
// base's rules and, when asked, a green main. It is read-only and makes
// no podman, git or GitHub call of its own: everything it asks goes
// through Deps. factory onboard prints a Report; the operator reads its
// JSON.
package onboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/github"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// An item's status. Only fail makes a report not OK.
const (
	StatusOK   = "ok"
	StatusWarn = "warn"
	StatusFail = "fail"
	StatusSkip = "skip"
)

// mainTail is how much of a red main's output an item keeps, in code
// points.
const mainTail = 1500

// Reach is what the check asks the GitHub App (github.Client does it).
type Reach interface {
	CanReach(ctx context.Context, repo string) (bool, error)
	BranchRules(ctx context.Context, repo, branch string) ([]string, error)
}

// Deps is everything the check reads.
type Deps struct {
	Config *config.Config
	Paths  paths.Paths
	// Head is the base's head in the project's checkout; nil skips the
	// checkout (and so main).
	Head func(ctx context.Context, p config.Project) (string, error)
	// GitHub is the App; nil skips github and rules.
	GitHub Reach
	// Checks runs the gate's checks on ref; nil skips main.
	Checks func(ctx context.Context, p config.Project, ref string) (ok bool, tail string, err error)
}

// Item is one check's outcome. The names are stable: the operator reads
// them.
type Item struct{ Check, Status, Detail string }

// Report is one project's check.
type Report struct {
	Project, Kind string
	Inferred      bool
	Items         []Item
}

// OK reports whether no item failed.
func (r Report) OK() bool {
	return !slices.ContainsFunc(r.Items, func(it Item) bool { return it.Status == StatusFail })
}

// JSON is {"project", "kind", "inferred", "ok", "items": [{"check",
// "status", "detail"}]} on one line, with no newline.
func (r Report) JSON() []byte {
	items := make([]jsonx.Value, len(r.Items))
	for i, it := range r.Items {
		items[i] = jsonx.ObjectOf(
			jsonx.Member{Key: "check", Value: jsonx.String(it.Check)},
			jsonx.Member{Key: "status", Value: jsonx.String(it.Status)},
			jsonx.Member{Key: "detail", Value: jsonx.String(it.Detail)})
	}
	return jsonx.EncodePython(jsonx.ObjectOf(
		jsonx.Member{Key: "project", Value: jsonx.String(r.Project)},
		jsonx.Member{Key: "kind", Value: jsonx.String(r.Kind)},
		jsonx.Member{Key: "inferred", Value: jsonx.Bool(r.Inferred)},
		jsonx.Member{Key: "ok", Value: jsonx.Bool(r.OK())},
		jsonx.Member{Key: "items", Value: jsonx.ArrayOf(items...)}), -1)
}

// Check runs the checks on the factory project key, in order: kind,
// commands, checkout, context, github, rules, main. A failed check does
// not stop the others, except that main needs the checkout's head and
// rules needs the App's reach. The error is for a key that is not a
// factory project, or a cancelled context.
func Check(ctx context.Context, d Deps, key string) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	p, ok := d.Config.Project(key)
	if !ok {
		return Report{}, fmt.Errorf("no project %s in config", key)
	}
	switch p.Mode {
	case config.ModeFactory:
	case config.ModeBoard:
		return Report{}, fmt.Errorf("%s is a board project: the factory does not work on it", key)
	default:
		return Report{}, fmt.Errorf("%s has mode %q: not a factory project", key, p.Mode)
	}
	r := Report{Project: key}
	add := func(check, status, detail string) {
		r.Items = append(r.Items, Item{Check: check, Status: status, Detail: detail})
	}

	k, inferred := d.Config.KindOf(*p)
	r.Kind, r.Inferred = k.Name, inferred
	switch {
	case k.Name == "":
		r.Kind = p.Kind
		add("kind", StatusFail, fmt.Sprintf("no such kind %q (factory kinds lists them)", p.Kind))
	case inferred:
		detail := fmt.Sprintf("inferred from language; set kind = %q", k.Name)
		// an inferred kind is always the built-in (KindOf): a config kind
		// of that name is what setting it would switch the project to
		if c, ok := d.Config.KindNamed(k.Name); ok && c.Source == config.KindConfig {
			detail += fmt.Sprintf(" (note: [kinds.%s] in config overrides the built-in the project uses now)", k.Name)
		}
		add("kind", StatusWarn, detail)
	default:
		add("kind", StatusOK, k.Name)
	}

	var missing []string
	for _, c := range []string{"install", "test", "lint"} {
		if !p.Has(c) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		add("commands", StatusFail, "missing: "+strings.Join(missing, ", "))
	} else {
		add("commands", StatusOK, "install, test and lint set")
	}

	head := ""
	if d.Head == nil {
		add("checkout", StatusSkip, "no checkout reader")
	} else if h, err := d.Head(ctx, *p); err != nil {
		if ctx.Err() != nil {
			return Report{}, ctx.Err()
		}
		add("checkout", StatusFail, fmt.Sprintf("%s in %s: %v", p.Base, p.Path, err))
	} else if h == "" {
		add("checkout", StatusFail, fmt.Sprintf("%s in %s: no head", p.Base, p.Path))
	} else {
		head = h
		add("checkout", StatusOK, p.Base+" at "+textx.Prefix(h, 12))
	}

	index := filepath.Join(d.Paths.ProjectContext(key), "INDEX.md")
	if _, err := os.Stat(index); err != nil {
		add("context", StatusWarn, "no "+index+": agents start with no project context")
	} else {
		add("context", StatusOK, index)
	}

	reached, noRules := false, "the App does not reach the repository"
	switch {
	case p.GitHub == "":
		noRules = "no github key"
		add("github", StatusWarn, "no github key: no PRs or sweep")
	case d.GitHub == nil:
		noRules = "no [github] App in config"
		add("github", StatusSkip, noRules)
	default:
		can, err := d.GitHub.CanReach(ctx, p.GitHub)
		if ctx.Err() != nil {
			return Report{}, ctx.Err()
		}
		switch {
		case err != nil:
			add("github", StatusFail, err.Error())
		case !can:
			add("github", StatusFail, "grant the App access to "+p.GitHub)
		default:
			reached = true
			add("github", StatusOK, p.GitHub)
		}
	}

	if !reached {
		add("rules", StatusSkip, noRules)
	} else {
		types, err := d.GitHub.BranchRules(ctx, p.GitHub, p.Base)
		if ctx.Err() != nil {
			return Report{}, ctx.Err()
		}
		var he *github.HTTPError
		switch {
		case errors.As(err, &he) && (he.Code == http.StatusForbidden || he.Code == http.StatusNotFound):
			add("rules", StatusWarn, "cannot read the branch's rules: a private repo without GitHub Pro has none")
		case err != nil:
			add("rules", StatusFail, err.Error())
		case slices.Contains(types, "pull_request"):
			add("rules", StatusOK, p.Base+" takes changes only through pull requests")
		default:
			add("rules", StatusWarn, "no pull_request rule on "+p.Base+": anyone with write access can push to it")
		}
	}

	switch {
	case head == "":
		add("main", StatusSkip, "no checkout to check")
	case d.Checks == nil:
		add("main", StatusSkip, "not run (--checks runs it)")
	default:
		green, tail, err := d.Checks(ctx, *p, head)
		if ctx.Err() != nil {
			return Report{}, ctx.Err()
		}
		switch {
		case err != nil:
			add("main", StatusFail, err.Error())
		case !green:
			add("main", StatusFail, lastRunes(tail, mainTail))
		default:
			add("main", StatusOK, "the checks pass at "+textx.Prefix(head, 12))
		}
	}
	return r, nil
}

// lastRunes is Python's s[-n:].
func lastRunes(s string, n int) string {
	count := utf8.RuneCountInString(s)
	if count <= n {
		return s
	}
	skip := count - n
	for i := range s {
		if skip == 0 {
			return s[i:]
		}
		skip--
	}
	return ""
}
