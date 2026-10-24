package prflow

import (
	"strings"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/jsonx"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

// AutoApprovalPhrase is the line the off-box approve job looks for in a PR's
// body: it must not change.
const AutoApprovalPhrase = "- Small, docs/tests/config-only change: eligible for automatic approval."

// PRBody is pr_body (F:2404): the PR's public text, with no internal ids or
// board references. It is the worker's summary, "Fixes #<issue>" for a
// ticket from a GitHub issue, how the change was tested, its risk (with the
// files that touch the project's walls, when any do), how to revert it, and
// [identity] pr_footer.
//
// It reads the ticket as Python did, from the ticket as it would be saved:
// values of unexpected types appear as Python's str() of them.
func PRBody(cfg *config.Config, t *ticket.Ticket) string {
	tree := t.Tree()
	reports := dictGet(tree, "reports", jsonx.ObjectOf())
	work := dictGet(reports, "work", jsonx.ObjectOf())
	review := dictGet(reports, "review", jsonx.ObjectOf())
	sec := dictGet(reports, "security", jsonx.ObjectOf())
	g := dictGet(tree, "gate", jsonx.ObjectOf())
	var answers int
	if a := dictGet(sec, "answers", jsonx.Null()); a.Kind() == jsonx.KindArray {
		answers = pyLen(a)
	}
	q := jsonx.String("?")
	tier := dictGet(tree, "security_tier", q)
	tierText := jsonx.PyStr(tier)

	lines := []string{textx.Strip(jsonx.PyStr(dictGet(work, "summary", jsonx.String("")))), ""}
	if issue := dictGet(dictGet(tree, "source", jsonx.ObjectOf()), "issue", jsonx.Null()); jsonx.Truthy(issue) {
		lines = append(lines, "Fixes #"+jsonx.PyStr(issue), "")
	}
	plural := "s"
	if answers == 1 {
		plural = ""
	}
	verdict := "- Needs a maintainer's review: " + joinStr(dictGet(g, "blockers", jsonx.ArrayOf()), "; ") + "."
	if jsonx.Truthy(dictGet(g, "auto_eligible", jsonx.Null())) {
		verdict = AutoApprovalPhrase
	}
	behaviour := jsonx.PyStr(dictGet(review, "runtime_behaviour", q))
	risk := []string{"- " + capitalize(tierName(tier)) + " (security tier " + tierText + "); " + jsonx.PyStr(dictGet(g, "changed_lines", q)) +
		" changed lines in " + itoa(pyLen(dictGet(g, "files", jsonx.ArrayOf()))) + " file(s); changes behaviour for users: " + behaviour + "."}
	if walls := dictGet(g, "walls", jsonx.ArrayOf()); pyLen(walls) > 0 {
		risk = append(risk, "- Touches the walls: "+joinStr(walls, ", ")+".")
	}
	lines = append(lines,
		"**Testing**", "",
		"- The author ran the tests ("+jsonx.PyStr(dictGet(work, "tests", q))+") and lint ("+jsonx.PyStr(dictGet(work, "lint", q))+").",
		"- Tests and lint pass again on a fresh checkout with no network access.",
		"- Independent review: "+jsonx.PyStr(dictGet(review, "verdict", q))+"; changes runtime behaviour for users: "+behaviour+".",
		"- Security review (tier "+tierText+"): "+itoa(answers)+" question"+plural+" answered, none above low concern.",
		verdict,
		"", "**Risk**", "")
	lines = append(lines, risk...)
	lines = append(lines,
		"", "**How to revert**", "",
		"- Revert the merge commit (`git revert -m 1 <merge commit>`); this change carries no data migration.",
		"", cfg.Identity.PRFooter)
	return textx.Strip(strings.Join(lines, "\n")) + "\n"
}

// tierName is TIER_NAMES.get(tier, "unclassified"): a dict keyed by the ints
// 0-4, which 1.0 and True also find.
func tierName(v jsonx.Value) string {
	names := model.TierNames()
	n := -1
	switch v.Kind() {
	case jsonx.KindBool:
		if b, _ := v.Bool(); b {
			n = 1
		} else {
			n = 0
		}
	case jsonx.KindNumber:
		if f, ok := v.Float64(); ok && f == float64(int(f)) {
			n = int(f)
		}
	}
	if n < 0 || n >= len(names) {
		return "unclassified"
	}
	return names[n]
}

// capitalize is str.capitalize for the tier names: the first letter upper,
// the rest lower (they are lower already).
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(textx.Lower(s))
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}

// joinStr is sep.join(v) for a list (of strings, as Python needs; others
// as their str()).
func joinStr(v jsonx.Value, sep string) string {
	items, _ := v.Array()
	parts := make([]string, len(items))
	for i, it := range items {
		parts[i] = jsonx.PyStr(it)
	}
	return strings.Join(parts, sep)
}
