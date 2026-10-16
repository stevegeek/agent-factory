package fizz

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Importer copies an account's boards from a Fizzy server (app.fizzy.do) into the store,
// keeping ids, card numbers, timestamps, columns, and closed / not now / golden.
// Running it again refreshes what it copied; a local card with the same number but
// another id stops it, rather than be overwritten.
type Importer struct {
	Base    string   // https://app.fizzy.do
	Token   string   // a Fizzy API token
	Account string   // the account number, as in .fizzy.yaml
	Boards  []string // only these board ids; none: every board
	Owner   string   // the owner's name when the account is new here
	Client  *http.Client
	Log     func(format string, args ...any)
	Sleep   func(time.Duration)
}

type Imported struct{ Boards, Cards, Comments, Users, Tags int }

type rUser struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email_address"`
	Role      string `json:"role"`
	Active    *bool  `json:"active"`
	CreatedAt string `json:"created_at"`
}

type rRef struct {
	ID string `json:"id"`
}

type rBoard struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	AllAccess    bool   `json:"all_access"`
	AutoPostpone int    `json:"auto_postpone_period_in_days"`
	CreatedAt    string `json:"created_at"`
	Creator      *rUser `json:"creator"`
}

type rColumn struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Color     json.RawMessage `json:"color"`
	CreatedAt string          `json:"created_at"`
}

type rCard struct {
	ID              string          `json:"id"`
	Number          int             `json:"number"`
	Title           string          `json:"title"`
	Description     string          `json:"description"`
	DescriptionHTML string          `json:"description_html"`
	Status          string          `json:"status"`
	Closed          bool            `json:"closed"`
	Postponed       bool            `json:"postponed"`
	Golden          bool            `json:"golden"`
	CreatedAt       string          `json:"created_at"`
	LastActiveAt    string          `json:"last_active_at"`
	Board           *rRef           `json:"board"`
	Column          *rRef           `json:"column"`
	Creator         *rUser          `json:"creator"`
	Assignees       []rUser         `json:"assignees"`
	Tags            json.RawMessage `json:"tags"`
	Steps           []Step          `json:"steps"`
}

type rComment struct {
	ID   string `json:"id"`
	Body struct {
		HTML      string `json:"html"`
		PlainText string `json:"plain_text"`
	} `json:"body"`
	Creator   *rUser `json:"creator"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (im *Importer) logf(f string, args ...any) {
	if im.Log != nil {
		im.Log(f, args...)
	}
}

// get fetches one URL (a path under the account, or a full URL on the same host).
func (im *Importer) get(target string) ([]byte, string, error) {
	base, err := url.Parse(im.Base)
	if err != nil {
		return nil, "", err
	}
	u, err := base.Parse(target)
	if err != nil {
		return nil, "", err
	}
	// the token goes only to the server it is for
	if u.Scheme != base.Scheme || u.Host != base.Host {
		return nil, "", fmt.Errorf("refusing to follow %s: not on %s", u, base.Host)
	}
	for try := 0; ; try++ {
		req, _ := http.NewRequest("GET", u.String(), nil)
		req.Header.Set("Authorization", "Bearer "+im.Token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "foreman-import")
		resp, err := im.Client.Do(req)
		if err != nil {
			return nil, "", err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode == http.StatusTooManyRequests && try < 5 {
			wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			im.Sleep(time.Duration(min(max(wait, 1), 60)) * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("GET %s: %s", u.Path, resp.Status)
		}
		return body, linkNext(resp.Header.Get("Link")), nil
	}
}

func linkNext(h string) string {
	for _, part := range strings.Split(h, ",") {
		if strings.Contains(part, `rel="next"`) {
			if i, j := strings.Index(part, "<"), strings.Index(part, ">"); i >= 0 && j > i {
				return part[i+1 : j]
			}
		}
	}
	return ""
}

func (im *Importer) one(path string, v any) error {
	body, _, err := im.get(im.path(path))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	return nil
}

// all fetches every page of a list, following Link: rel="next".
func (im *Importer) all(path string, each func(json.RawMessage) error) error {
	next := im.path(path)
	for pages := 0; next != ""; pages++ {
		if pages > 10000 {
			return fmt.Errorf("%s: too many pages", path)
		}
		body, link, err := im.get(next)
		if err != nil {
			return err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		for _, it := range items {
			if err := each(it); err != nil {
				return err
			}
		}
		next = link
	}
	return nil
}

func (im *Importer) path(p string) string {
	if strings.HasPrefix(p, "/my/") {
		return strings.TrimSuffix(im.Base, "/") + p
	}
	return strings.TrimSuffix(im.Base, "/") + "/" + im.Account + p
}

func decode[T any](out *[]T) func(json.RawMessage) error {
	return func(raw json.RawMessage) error {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		*out = append(*out, v)
		return nil
	}
}

// Run reads everything first, then writes it under the store's lock.
func (im *Importer) Run(st *Store) (Imported, error) {
	var n Imported
	if !ValidAccount(im.Account) {
		return n, fmt.Errorf("account must be digits: %q", im.Account)
	}
	base, err := url.Parse(im.Base)
	if err != nil || base.Host == "" {
		return n, fmt.Errorf("bad Fizzy URL %q", im.Base)
	}
	// the token crosses the network only encrypted (a local test server excepted)
	if base.Scheme != "https" && !(base.Scheme == "http" && loopback(base.Hostname())) {
		return n, fmt.Errorf("the Fizzy URL must be https: %s", im.Base)
	}
	client := http.Client{Timeout: 60 * time.Second}
	if im.Client != nil {
		client = *im.Client
	}
	// a redirect could carry the token elsewhere: take the answer as it is
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	im.Client = &client
	if im.Sleep == nil {
		im.Sleep = time.Sleep
	}
	if im.Owner == "" {
		im.Owner = "Stephen"
	}
	name := "Fizzy " + im.Account
	var ident struct {
		Accounts []struct {
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"accounts"`
	}
	if err := im.one("/my/identity.json", &ident); err != nil {
		return n, fmt.Errorf("identity (is the token right?): %w", err)
	}
	for _, a := range ident.Accounts {
		if strings.Trim(a.Slug, "/") == im.Account && a.Name != "" {
			name = a.Name
		}
	}

	var users []rUser
	if err := im.all("/users.json", decode(&users)); err != nil {
		return n, err
	}
	var tags []Tag
	if err := im.all("/tags.json", decode(&tags)); err != nil {
		return n, err
	}
	var boards []rBoard
	if err := im.all("/boards.json", decode(&boards)); err != nil {
		return n, err
	}
	if len(im.Boards) > 0 {
		want := map[string]bool{}
		for _, id := range im.Boards {
			want[id] = true
		}
		var keep []rBoard
		for _, b := range boards {
			if want[b.ID] {
				keep = append(keep, b)
				delete(want, b.ID)
			}
		}
		for id := range want {
			return n, fmt.Errorf("no board %s in account %s", id, im.Account)
		}
		boards = keep
	}

	type full struct {
		card     rCard
		comments []rComment
	}
	columns := map[string][]rColumn{}
	cards := map[int]*full{}
	for _, b := range boards {
		if !ValidID(b.ID) {
			return n, fmt.Errorf("board id %q is not one this store can keep", b.ID)
		}
		var cols []rColumn
		if err := im.all("/boards/"+b.ID+"/columns.json", decode(&cols)); err != nil {
			return n, err
		}
		columns[b.ID] = cols
		for _, index := range []string{"all", "closed", "not_now"} {
			var list []rCard
			if err := im.all("/cards.json?board_ids[]="+url.QueryEscape(b.ID)+"&indexed_by="+index, decode(&list)); err != nil {
				return n, err
			}
			for _, c := range list {
				if c.Number < 1 || c.Number > MaxNumber {
					return n, fmt.Errorf("card number %d is not one this store can keep", c.Number)
				}
				if c.Board == nil || c.Board.ID == b.ID {
					cards[c.Number] = &full{card: c}
				}
			}
		}
		im.logf("board %s (%s): %d columns", b.Name, b.ID, len(cols))
	}
	if len(cards) > maxCards {
		return n, fmt.Errorf("%d cards: more than the %d an account may hold", len(cards), maxCards)
	}
	numbers := make([]int, 0, len(cards))
	for num := range cards {
		numbers = append(numbers, num)
	}
	sort.Ints(numbers)
	for _, num := range numbers {
		f := cards[num]
		var shown rCard
		if err := im.one("/cards/"+strconv.Itoa(num)+".json", &shown); err != nil {
			return n, err
		}
		if shown.Board == nil {
			shown.Board = f.card.Board
		}
		f.card = shown
		if err := im.all("/cards/"+strconv.Itoa(num)+"/comments.json", decode(&f.comments)); err != nil {
			return n, err
		}
		if len(f.comments) > maxComments || len(shown.Steps) > maxSteps {
			return n, fmt.Errorf("card #%d has more than %d comments or %d steps", num, maxComments, maxSteps)
		}
	}

	// write
	if err := st.Lock(); err != nil {
		return n, err
	}
	defer st.Unlock()
	a, err := st.Account(im.Account)
	if errors.Is(err, ErrNotFound) {
		a, err = st.Init(im.Account, name, im.Owner, "")
	}
	if err != nil {
		return n, err
	}
	// a remote user maps onto a local one by id only; one not here yet is added with its id.
	// Never by name: a name is no proof two accounts are one person.
	ids := map[string]string{}
	var mapUser func(u *rUser) string
	mapUser = func(u *rUser) string {
		if u == nil || u.ID == "" {
			return ""
		}
		if id, ok := ids[u.ID]; ok {
			return id
		}
		local := a.User(u.ID)
		if local == nil {
			if !ValidID(u.ID) {
				return ""
			}
			local = &User{ID: u.ID, Name: u.Name, Role: orDefault(u.Role, "member"), Active: u.Active == nil || *u.Active, CreatedAt: u.CreatedAt}
			a.Users = append(a.Users, local)
			n.Users++
		}
		if local.Email == "" {
			local.Email = u.Email
		}
		ids[u.ID] = local.ID
		return local.ID
	}
	for i := range users {
		mapUser(&users[i])
	}
	for _, t := range tags {
		if a.Tag(t.ID) == nil && a.TagTitled(t.Title) == nil && ValidID(t.ID) {
			a.Tags = append(a.Tags, &Tag{ID: t.ID, Title: t.Title, CreatedAt: t.CreatedAt})
			n.Tags++
		}
	}
	tagID := func(title string) string {
		if t := a.TagTitled(title); t != nil {
			return t.ID
		}
		t := &Tag{ID: NewID(), Title: title, CreatedAt: st.Stamp()}
		a.Tags = append(a.Tags, t)
		n.Tags++
		return t.ID
	}

	var out []*Card
	for _, num := range numbers {
		rc, rcs := cards[num].card, cards[num].comments
		if !ValidID(rc.ID) || num < 1 || num > MaxNumber {
			return n, fmt.Errorf("card %d: id %q is not one this store can keep", num, rc.ID)
		}
		if old, err := st.Card(a.ID, num); err == nil && old.ID != rc.ID {
			return n, fmt.Errorf("card #%d here is another card (%s, %q); not overwriting it", num, old.ID, old.Title)
		}
		c := &Card{ID: rc.ID, Number: num, Title: rc.Title, Status: orDefault(rc.Status, "published"),
			Closed: rc.Closed, Postponed: rc.Postponed, Golden: rc.Golden, CreatedAt: rc.CreatedAt,
			LastActiveAt: orDefault(rc.LastActiveAt, rc.CreatedAt), CreatorID: mapUser(rc.Creator),
			Description: rc.DescriptionHTML, Assignees: []string{}, Tags: []string{}, Steps: []*Step{}, Comments: []*Comment{}}
		if c.Description == "" {
			c.Description = RichText(rc.Description)
		}
		if rc.Board != nil {
			c.BoardID = rc.Board.ID
		}
		if rc.Column != nil {
			for _, col := range columns[c.BoardID] {
				if col.ID == rc.Column.ID {
					c.ColumnID = col.ID
				}
			}
		}
		for i := range rc.Assignees {
			if id := mapUser(&rc.Assignees[i]); id != "" {
				c.Assignees = append(c.Assignees, id)
			}
		}
		for _, title := range tagTitles(rc.Tags) {
			c.Tags = append(c.Tags, tagID(title))
		}
		for _, s := range rc.Steps {
			if ValidID(s.ID) {
				c.Steps = append(c.Steps, &Step{ID: s.ID, Content: s.Content, Completed: s.Completed})
			}
		}
		for i := range rcs {
			rcm := &rcs[i]
			if !ValidID(rcm.ID) {
				continue
			}
			body := rcm.Body.HTML
			if body == "" {
				body = RichText(rcm.Body.PlainText)
			}
			c.Comments = append(c.Comments, &Comment{ID: rcm.ID, Body: body, CreatorID: mapUser(rcm.Creator),
				CreatedAt: rcm.CreatedAt, UpdatedAt: orDefault(rcm.UpdatedAt, rcm.CreatedAt)})
			n.Comments++
		}
		a.LastNumber = max(a.LastNumber, num)
		out = append(out, c)
	}
	for _, rb := range boards {
		b := &Board{ID: rb.ID, Name: rb.Name, AllAccess: rb.AllAccess, AutoPostpone: rb.AutoPostpone,
			CreatedAt: rb.CreatedAt, CreatorID: mapUser(rb.Creator), Columns: []*Column{}}
		for _, rc := range columns[rb.ID] {
			if !ValidID(rc.ID) {
				continue
			}
			name, value := colorOf(rc.Color)
			b.Columns = append(b.Columns, &Column{ID: rc.ID, Name: rc.Name, ColorName: name, ColorValue: value, CreatedAt: rc.CreatedAt})
		}
		if err := st.SaveBoard(a.ID, b); err != nil {
			return n, err
		}
		n.Boards++
	}
	for _, c := range out {
		if err := st.SaveCard(a.ID, c); err != nil {
			return n, err
		}
		n.Cards++
	}
	return n, st.SaveAccount(a)
}

// tagTitles reads a card's tags: names, or objects with a title.
func tagTitles(raw json.RawMessage) []string {
	var names []string
	if json.Unmarshal(raw, &names) == nil {
		return names
	}
	var objs []struct {
		Title string `json:"title"`
	}
	json.Unmarshal(raw, &objs)
	for _, o := range objs {
		if o.Title != "" {
			names = append(names, o.Title)
		}
	}
	return names
}

func colorOf(raw json.RawMessage) (string, string) {
	var c struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &c) == nil && (c.Name != "" || c.Value != "") {
		return c.Name, c.Value
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return orDefault(colors[s], "Custom"), s
	}
	return "Blue", "var(--color-card-default)"
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
