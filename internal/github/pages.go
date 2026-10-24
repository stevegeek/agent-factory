package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/stevegeek/agent-factory/internal/jsonx"
)

// Pages GETs path and follows GitHub's Link rel="next" pages, gathering
// list items until there are no more pages or limit items. Nothing calls it
// before cutover: the port keeps Python's single page of 50 issues and 100
// reviews or comments (§13.2 quirk 8); it is here for the fix after.
//
// A next link is followed only when it is under BaseURL, so the token is
// never sent to another host.
func (c *Client) Pages(ctx context.Context, path string, limit int) ([]jsonx.Value, error) {
	if limit <= 0 {
		return nil, errors.New("github: Pages needs a positive limit")
	}
	base := c.base()
	var out []jsonx.Value
	for page := 0; page < limit && path != ""; page++ {
		resp, err := c.Do(ctx, "GET", path, jsonx.Null())
		if err != nil {
			return out, err
		}
		items, ok := resp.Value.Array()
		if !ok {
			return out, fmt.Errorf("github: GET %s: expected a list, got %s", path, resp.Value.Kind())
		}
		if len(items) == 0 {
			break
		}
		out = append(out, items...)
		if len(out) >= limit {
			return out[:limit], nil
		}
		next := nextLink(resp.Header.Get("Link"))
		if next == "" {
			break
		}
		rest, ok := strings.CutPrefix(next, base)
		if !ok || !strings.HasPrefix(rest, "/") {
			return out, fmt.Errorf("github: GET %s: next page %q is not under %s", path, next, base)
		}
		path = rest
	}
	return out, nil
}

// nextLink is the rel="next" target of an RFC 8288 Link header, or "".
func nextLink(header string) string {
	for part := range strings.SplitSeq(header, ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(part), ";")
		target = strings.TrimSpace(target)
		if !ok || !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			continue
		}
		for p := range strings.SplitSeq(params, ";") {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			if !strings.EqualFold(strings.TrimSpace(k), "rel") {
				continue
			}
			for rel := range strings.FieldsSeq(strings.Trim(strings.TrimSpace(v), `"`)) {
				if strings.EqualFold(rel, "next") {
					return target[1 : len(target)-1]
				}
			}
		}
	}
	return ""
}
