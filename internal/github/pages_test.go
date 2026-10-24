package github

import (
	"context"
	"testing"
)

// Pagination exists but no caller uses it before cutover (the
// port keeps Python's single page of 50 issues and 100 reviews).
func TestPagesFollowsNextLinksOnTheSameServer(t *testing.T) {
	f := newFake(t)
	next := func(page string) map[string]string {
		return map[string]string{"Link": `<` + f.srv.URL + `/repos/o/r/pulls/1/reviews?per_page=2&page=` + page + `>; rel="next", <` + f.srv.URL + `/x?page=9>; rel="last"`}
	}
	f.onReply("GET", "/repos/o/r/pulls/1/reviews?per_page=2", reply{Code: 200, Body: `[1, 2]`, Header: next("2")})
	f.onReply("GET", "/repos/o/r/pulls/1/reviews?per_page=2&page=2", reply{Code: 200, Body: `[3, 4]`, Header: next("3")})
	f.on("GET", "/repos/o/r/pulls/1/reviews?per_page=2&page=3", 200, `[5]`)
	items, err := f.client().Pages(context.Background(), "/repos/o/r/pulls/1/reviews?per_page=2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 5 {
		t.Fatalf("items = %v", items)
	}
	for i, it := range items {
		if n, _ := it.Int64(); n != int64(i+1) {
			t.Errorf("item %d = %v", i, it)
		}
	}
	for _, r := range f.requests() {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("%s sent without the token", r.URI)
		}
	}
}

func TestPagesStopsAtItsLimit(t *testing.T) {
	f := newFake(t)
	f.onReply("GET", "/a", reply{Code: 200, Body: `[1]`, Header: map[string]string{"Link": `<` + f.srv.URL + `/a>; rel="next"`}})
	items, err := f.client().Pages(context.Background(), "/a", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || len(f.requests()) != 3 {
		t.Errorf("%d items from %d requests, want 3 from 3", len(items), len(f.requests()))
	}
}

func TestPagesNeverSendsTheTokenElsewhere(t *testing.T) {
	f := newFake(t)
	other := newFake(t)
	other.on("GET", "/steal", 200, `[2]`)
	f.onReply("GET", "/a", reply{Code: 200, Body: `[1]`, Header: map[string]string{"Link": `<` + other.srv.URL + `/steal>; rel="next"`}})
	if _, err := f.client().Pages(context.Background(), "/a", 10); err == nil {
		t.Error("a next link to another server was followed")
	}
	if n := len(other.requests()); n != 0 {
		t.Errorf("the other server got %d requests", n)
	}
}

func TestPagesNeedsALimitAndLists(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/obj", 200, `{}`)
	if _, err := f.client().Pages(context.Background(), "/obj", 10); err == nil {
		t.Error("an object page was accepted")
	}
	if _, err := f.client().Pages(context.Background(), "/obj", 0); err == nil {
		t.Error("a zero limit was accepted")
	}
}

func TestNextLinkParsing(t *testing.T) {
	for in, want := range map[string]string{
		``:                                 "",
		`<https://x/a?page=2>; rel="next"`: "https://x/a?page=2",
		`<https://x/l>; rel="last", <https://x/n>; rel="next"`: "https://x/n",
		`<https://x/n>; rel="prev next"`:                       "https://x/n",
		`<https://x/n>; rel=next`:                              "https://x/n",
		`<https://x/p>; rel="prev"`:                            "",
		`garbage`:                                              "",
	} {
		if got := nextLink(in); got != want {
			t.Errorf("nextLink(%q) = %q, want %q", in, got, want)
		}
	}
}
