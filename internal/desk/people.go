package desk

import (
	"github.com/stevegeek/agent-factory/internal/present"

	"slices"
	"time"
)

// PeopleView is Settings' people section: the viewer's own desk, and
// everyone's, which anyone may change.
type PeopleView struct {
	Me       string     // the viewer's name; "" for the main secret
	MyDesk   []DeskPick // the viewer's desk projects
	Users    []UserRow
	Projects []string // every configured project, for the desks
	Missing  []string // users who are not in [board] people: nothing can be put to them by name
}

// DeskPick is one project's box on a desk.
type DeskPick struct {
	Project string
	On      bool
}

// UserRow is one person's line in Settings.
type UserRow struct {
	Name, Email, Added string
	Desk               []DeskPick
	Every              bool // no desk projects: every project
	You                bool
}

// Person is a user as Settings lists them.
type Person struct {
	Name, Email, Added string
	Desk               []string
}

// People is Settings' people section for the viewer me (with desk), over
// the configured projects; people is [board] people.
func People(me string, desk []string, users []Person, projects, people []string, now time.Time) *PeopleView {
	picks := func(desk []string) []DeskPick {
		out := make([]DeskPick, 0, len(projects))
		for _, p := range projects {
			out = append(out, DeskPick{Project: p, On: slices.Contains(desk, p)})
		}
		return out
	}
	v := &PeopleView{Me: me, Projects: projects}
	if me != "" {
		v.MyDesk = picks(desk)
	}
	for _, u := range users {
		v.Users = append(v.Users, UserRow{Name: u.Name, Email: u.Email, Added: present.AgeSince(u.Added, now),
			Desk: picks(u.Desk), Every: len(u.Desk) == 0, You: u.Name == me})
		if len(people) > 0 && !slices.Contains(people, u.Name) {
			v.Missing = append(v.Missing, u.Name)
		}
	}
	return v
}
