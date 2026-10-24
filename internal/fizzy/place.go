package fizzy

import (
	"strings"

	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/textx"
)

// NotNow is the wait of a card Fizzy postponed ("Not Now").
const NotNow = "not now (postponed in Fizzy)"

// Place is where a card goes on the board project: a column, why it
// waits (in waiting), and a label the column adds ("review").
type Place struct {
	Column model.BoardColumn
	Wait   string
	Label  string
}

// PlaceOf is the board column for a card: closed or in "Done" is done;
// postponed or in "Not Now" waits, not now; then by its column's name, in
// any case: none or "Maybe?" is inbox, "In progress" and "Doing" are
// doing, "Review" is doing with the label review, a name starting
// "Waiting" or "Blocked" waits with that name as the reason, and "Next"
// and every other column is next.
func PlaceOf(column string, closed, postponed bool) Place {
	name := strings.TrimSpace(textx.Lower(column))
	switch {
	case closed || name == "done":
		return Place{Column: model.BoardDone}
	case postponed || name == "not now":
		return Place{Column: model.BoardWaiting, Wait: NotNow}
	case name == "" || strings.TrimRight(name, "?") == "maybe":
		return Place{Column: model.BoardInbox}
	case name == "in progress" || name == "doing":
		return Place{Column: model.BoardDoing}
	case name == "review":
		return Place{Column: model.BoardDoing, Label: "review"}
	case strings.HasPrefix(name, "waiting") || strings.HasPrefix(name, "blocked"):
		return Place{Column: model.BoardWaiting, Wait: cutRunes(line(column), 500)}
	}
	return Place{Column: model.BoardNext}
}
