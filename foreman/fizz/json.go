package fizz

import (
	"strconv"
	"strings"
)

// The JSON shapes the Fizzy API returns, built from the store. base is the API root as the
// client reached it (scheme, host and mount path), so urls work for the caller.

type view struct {
	base string
	a    *Account
}

func (v view) acctURL() string { return v.base + "/" + v.a.ID }

func (v view) user(id string) map[string]any {
	u := v.a.User(id)
	if u == nil {
		return map[string]any{"id": id, "name": "Unknown", "role": "member", "active": false,
			"email_address": "", "created_at": "", "url": v.acctURL() + "/users/" + id, "avatar_url": ""}
	}
	return v.userJSON(u)
}

func (v view) userJSON(u *User) map[string]any {
	url := v.acctURL() + "/users/" + u.ID
	return map[string]any{"id": u.ID, "name": u.Name, "role": u.Role, "active": u.Active,
		"email_address": u.Email, "created_at": u.CreatedAt, "url": url, "avatar_url": url + "/avatar"}
}

func (v view) board(b *Board) map[string]any {
	return map[string]any{"id": b.ID, "name": b.Name, "all_access": b.AllAccess,
		"auto_postpone_period_in_days": b.AutoPostpone, "created_at": b.CreatedAt,
		"url": v.acctURL() + "/boards/" + b.ID, "creator": v.user(b.CreatorID)}
}

func (v view) column(b *Board, c *Column) map[string]any {
	return map[string]any{"id": c.ID, "name": c.Name, "created_at": c.CreatedAt,
		"color":     map[string]any{"name": c.ColorName, "value": c.ColorValue},
		"cards_url": v.acctURL() + "/boards/" + b.ID + "/columns/" + c.ID + "/cards"}
}

func (v view) tag(t *Tag) map[string]any {
	return map[string]any{"id": t.ID, "title": t.Title, "created_at": t.CreatedAt,
		"url": v.acctURL() + "/cards?tag_ids[]=" + t.ID}
}

func (v view) cardURL(n int) string { return v.acctURL() + "/cards/" + strconv.Itoa(n) }

// card: steps only on show, as the real API does.
func (v view) card(c *Card, b *Board, withSteps bool) map[string]any {
	tags := []string{}
	for _, id := range c.Tags {
		if t := v.a.Tag(id); t != nil {
			tags = append(tags, t.Title)
		}
	}
	assignees := []any{}
	for _, id := range c.Assignees {
		assignees = append(assignees, v.user(id))
	}
	m := map[string]any{
		"id": c.ID, "number": c.Number, "title": c.Title, "status": orDefault(c.Status, "published"),
		"description": PlainText(c.Description, false), "description_html": c.Description,
		"image_url": nil, "has_attachments": strings.Contains(c.Description, "<action-text-attachment"),
		"tags": tags, "closed": c.Closed, "postponed": c.Postponed, "golden": c.Golden,
		"last_active_at": c.LastActiveAt, "created_at": c.CreatedAt,
		"url": v.cardURL(c.Number), "comments_url": v.cardURL(c.Number) + "/comments",
		"reactions_url": v.cardURL(c.Number) + "/reactions",
		"creator":       v.user(c.CreatorID), "assignees": assignees, "has_more_assignees": false,
	}
	if b != nil {
		m["board"] = v.board(b)
		if col := b.Column(c.ColumnID); col != nil && !c.Postponed {
			m["column"] = v.column(b, col)
		}
	}
	if withSteps {
		steps := []any{}
		for _, s := range c.Steps {
			steps = append(steps, v.step(c, s))
		}
		m["steps"] = steps
	}
	return m
}

func (v view) step(c *Card, s *Step) map[string]any {
	return map[string]any{"id": s.ID, "content": s.Content, "completed": s.Completed,
		"url": v.cardURL(c.Number) + "/steps/" + s.ID}
}

func (v view) comment(c *Card, cm *Comment) map[string]any {
	url := v.cardURL(c.Number) + "/comments/" + cm.ID
	return map[string]any{"id": cm.ID, "created_at": cm.CreatedAt, "updated_at": cm.UpdatedAt,
		"body":    map[string]any{"plain_text": PlainText(cm.Body, false), "html": cm.Body},
		"creator": v.user(cm.CreatorID), "card": map[string]any{"id": c.ID, "url": v.cardURL(c.Number)},
		"reactions_url": url + "/reactions", "url": url}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
