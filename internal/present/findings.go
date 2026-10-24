package present

import (
	"fmt"
	"strings"

	"github.com/stevegeek/agent-factory/internal/ticket"
)

// verifiedLine is the verifier's step in a hunt's story: how many of its
// findings it confirmed, and why the others were not.
func verifiedLine(t *ticket.Ticket) string {
	confirmed, dup, gone := 0, 0, 0
	for _, f := range t.Findings {
		switch f.Verdict {
		case ticket.VerdictConfirmed:
			confirmed++
		case ticket.VerdictDuplicate:
			dup++
		default:
			gone++
		}
	}
	line := fmt.Sprintf("Confirmed %d of %s", confirmed, Plural(len(t.Findings), "finding"))
	var why []string
	if dup > 0 {
		why = append(why, Plural(dup, "duplicate"))
	}
	if gone > 0 {
		why = append(why, fmt.Sprintf("%d not reproduced", gone))
	}
	if len(why) > 0 {
		line += " (" + strings.Join(why, ", ") + ")"
	}
	return line + "."
}

// reportedCount is the number of findings in a findings_reported move's
// words ("task done with 3 findings: …"), or "its".
func reportedCount(detail string) string {
	var n int
	if _, err := fmt.Sscanf(detail, "task done with %d findings", &n); err != nil {
		return "its"
	}
	return fmt.Sprint(n)
}
