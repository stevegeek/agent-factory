package gate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// The walls and public-repo hygiene. A wall is a file that holds the
// factory's own safety: a change to one never merges without the
// maintainer. Hygiene is for a public repo: what a PR publishes (its
// paths, added lines, commit messages, branch and the report texts the
// body prints) must carry no [hygiene] deny term. The hits name the term,
// so they go only to private places (gate-output.txt and the board); the
// blocker, which the PR body prints, only counts them.

// HygieneSnippet is how much text around a hit a hygiene line shows, in
// code points.
const HygieneSnippet = 80

// hygieneHeading starts the hits in gate-output.txt.
const hygieneHeading = "public-repo hygiene:"

// Walls are the files that match one of the project's walls patterns, in
// the order given.
func Walls(p config.Project, files []string) []string {
	var out []string
	for _, f := range files {
		if slices.ContainsFunc(p.Walls, func(w string) bool { return textx.Fnmatch(f, w) }) {
			out = append(out, f)
		}
	}
	return out
}

// HygieneIn is what a PR publishes. Texts is keyed by where the text goes
// ("the work summary").
type HygieneIn struct {
	Files, Added, Messages []string
	Branch                 string
	Texts                  map[string]string
}

// Hygiene is every deny term found in what the PR publishes, matched
// without regard to case: one line per term in each path, line, message
// and text, naming the place and the term, with HygieneSnippet code points
// around the hit. Blank terms are skipped.
func Hygiene(deny []string, in HygieneIn) []string {
	var terms []string
	for _, d := range deny {
		if d = textx.Strip(d); d != "" {
			terms = append(terms, d)
		}
	}
	if len(terms) == 0 {
		return nil
	}
	var out []string
	scan := func(place, text string) {
		lower := []rune(strings.ToLower(text)) // rune for rune, so its indexes are text's
		for _, term := range terms {
			if i := runeIndex(lower, []rune(strings.ToLower(term))); i >= 0 {
				out = append(out, fmt.Sprintf("%s: %q in %q", place, term, snippet([]rune(text), i, len([]rune(term)))))
			}
		}
	}
	for _, f := range in.Files {
		scan("a path", f)
	}
	for _, l := range in.Added {
		scan("an added line", l)
	}
	for _, m := range in.Messages {
		scan("a commit message", m)
	}
	scan("the branch name", in.Branch)
	keys := make([]string, 0, len(in.Texts))
	for k := range in.Texts {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		scan(k, in.Texts[k])
	}
	return out
}

// runeIndex is the code point index of sub in s, or -1.
func runeIndex(s, sub []rune) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if slices.Equal(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

// snippet is HygieneSnippet code points of text around the n at i, on one
// line.
func snippet(text []rune, i, n int) string {
	start := max(0, i-(HygieneSnippet-n)/2)
	end := min(len(text), start+HygieneSnippet)
	start = max(0, end-HygieneSnippet)
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, string(text[start:end]))
}

// PublicTexts are the report fields prflow.PRBody prints, keyed by where
// they go, as the body prints them (Python's str()), and its Updates; a
// missing one is left out.
func PublicTexts(t *ticket.Ticket) map[string]string {
	out := map[string]string{}
	tree, err := pyTicket(t)
	if err != nil {
		return out
	}
	reports, _ := tree.get("reports")
	for _, f := range []struct{ report, key string }{
		{"work", "summary"}, {"work", "tests"}, {"work", "lint"}, {"review", "verdict"}, {"review", "runtime_behaviour"},
	} {
		r, _ := reports.get(f.report)
		if v, ok := r.get(f.key); ok {
			out["the "+f.report+" "+f.key] = v.str()
		}
	}
	if u := ticket.UpdatesText(t); u != "" {
		out["the updates"] = u
	}
	return out
}

// hygieneOutput is the gate's output with the hits first.
func hygieneOutput(hits []string, output string) string {
	if len(hits) == 0 {
		return output
	}
	return hygieneHeading + "\n" + strings.Join(hits, "\n") + "\n\n" + output
}
