package present

import "github.com/stevegeek/agent-factory/internal/ticket"

// DraftWalls are the wall places a manager's drafts touch, as the tick's
// checker found them, each once.
func DraftWalls(t *ticket.Ticket) []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range t.Drafts {
		for _, w := range d.Walls {
			if !seen[w] {
				seen[w] = true
				out = append(out, w)
			}
		}
	}
	return out
}
