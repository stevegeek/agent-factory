package fizz

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Mount is where Foreman serves the API; a .fizzy.yaml's api_url is https://<host>/fizzy.
// The CLI joins api_url + "/<account>/…" (and "/my/identity.json"), so any prefix works.
const Mount = "/fizzy"

const maxBody = 1 << 20

// API is the Fizzy-compatible HTTP API over a Store. It never uses Foreman's login session:
// every request carries a bearer token from `foreman tokens add`.
type API struct {
	Store   *Store
	PerPage int
	mux     *http.ServeMux
}

func NewAPI(st *Store) *API {
	a := &API{Store: st, PerPage: 50, mux: http.NewServeMux()}
	routes := []struct {
		pattern string
		h       func(*call)
	}{
		{"GET /my/identity.json", a.identity},
		{"GET /{account}/boards.json", a.boards},
		{"POST /{account}/boards.json", a.createBoard},
		{"GET /{account}/boards/{board}", a.showBoard},
		{"GET /{account}/boards/{board}/columns.json", a.columns},
		{"POST /{account}/boards/{board}/columns.json", a.createColumn},
		{"GET /{account}/boards/{board}/columns/{column}", a.showColumn},
		{"GET /{account}/cards.json", a.cards},
		{"POST /{account}/cards.json", a.createCard},
		{"GET /{account}/cards/{card}", a.showCard},
		{"PATCH /{account}/cards/{card}", a.updateCard},
		{"PUT /{account}/cards/{card}", a.updateCard},
		{"DELETE /{account}/cards/{card}", a.deleteCard},
		{"POST /{account}/cards/{card}/closure.json", a.close},
		{"DELETE /{account}/cards/{card}/closure.json", a.reopen},
		{"POST /{account}/cards/{card}/triage.json", a.triage},
		{"DELETE /{account}/cards/{card}/triage.json", a.untriage},
		{"POST /{account}/cards/{card}/not_now.json", a.postpone},
		{"POST /{account}/cards/{card}/taggings.json", a.tagging},
		{"POST /{account}/cards/{card}/assignments.json", a.assign},
		{"POST /{account}/cards/{card}/self_assignment.json", a.selfAssign},
		{"POST /{account}/cards/{card}/goldness.json", a.gild(true)},
		{"DELETE /{account}/cards/{card}/goldness.json", a.gild(false)},
		{"GET /{account}/cards/{card}/comments.json", a.comments},
		{"POST /{account}/cards/{card}/comments.json", a.createComment},
		{"GET /{account}/cards/{card}/comments/{comment}", a.showComment},
		{"PATCH /{account}/cards/{card}/comments/{comment}", a.updateComment},
		{"PUT /{account}/cards/{card}/comments/{comment}", a.updateComment},
		{"DELETE /{account}/cards/{card}/comments/{comment}", a.deleteComment},
		{"GET /{account}/cards/{card}/steps.json", a.steps},
		{"POST /{account}/cards/{card}/steps.json", a.createStep},
		{"GET /{account}/cards/{card}/steps/{step}", a.showStep},
		{"PATCH /{account}/cards/{card}/steps/{step}", a.updateStep},
		{"PUT /{account}/cards/{card}/steps/{step}", a.updateStep},
		{"DELETE /{account}/cards/{card}/steps/{step}", a.deleteStep},
		{"GET /{account}/search.json", a.search},
		{"GET /{account}/tags.json", a.tags},
		{"GET /{account}/users.json", a.users},
		{"GET /{account}/users/{user}", a.showUser},
	}
	for _, rt := range routes {
		method, path, _ := strings.Cut(rt.pattern, " ")
		h := rt.h
		a.mux.HandleFunc(method+" "+Mount+path, func(w http.ResponseWriter, r *http.Request) { a.serve(w, r, h) })
	}
	a.mux.HandleFunc(Mount+"/", func(w http.ResponseWriter, r *http.Request) { a.serve(w, r, nil) })
	return a
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

// call: one request, authenticated, with its account loaded. Handlers set code and out;
// serve writes them after the store is unlocked.
type call struct {
	hdr  http.Header
	r    *http.Request
	raw  []byte // the request body, read before the lock
	st   *Store
	tok  *Token
	acct *Account
	v    view
	per  int
	code int
	out  []byte
}

type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func fail(code int, msg string) { panic(&httpError{code, msg}) }

func notFound() { fail(http.StatusNotFound, "Not Found") }

func errorBody(msg string) []byte {
	b, _ := json.Marshal(map[string]any{"error": msg})
	return append(b, '\n')
}

func (a *API) serve(w http.ResponseWriter, r *http.Request, h func(*call)) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		unauthorized(w)
		return
	}
	// tokens.json is only ever replaced whole, so it is read without the lock
	tok, err := a.Store.CheckToken(strings.TrimSpace(secret))
	if err != nil {
		log.Printf("fizzy api: tokens: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Internal Server Error"})
		return
	}
	if tok == nil {
		log.Printf("fizzy api: refused a bad token from %s", r.RemoteAddr)
		unauthorized(w)
		return
	}
	// the body is read, and the answer written, outside the lock: a slow client holds up no one
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "Request body too large"})
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Bad request"})
		}
		return
	}
	c := &call{hdr: w.Header(), r: r, raw: raw, st: a.Store, tok: tok, per: a.PerPage}
	code, out := a.run(c, h)
	w.WriteHeader(code)
	w.Write(out)
}

// run handles one call under the store's lock and returns the status and body to send.
func (a *API) run(c *call, h func(*call)) (code int, out []byte) {
	if err := a.Store.Lock(); err != nil {
		log.Printf("fizzy api: %v", err)
		return http.StatusInternalServerError, errorBody("Internal Server Error")
	}
	defer a.Store.Unlock()
	defer func() {
		if p := recover(); p != nil {
			e, ok := p.(*httpError)
			if !ok {
				panic(p)
			}
			code, out = e.code, errorBody(e.msg)
		}
	}()
	r := c.r
	if h == nil {
		notFound()
	}
	if r.PathValue("account") != c.tok.Account && r.URL.Path != Mount+"/my/identity.json" {
		notFound()
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && a.Store.Full() {
		fail(http.StatusInsufficientStorage, "The board store is full")
	}
	acct, err := a.Store.Account(c.tok.Account)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			log.Printf("fizzy api: account %s: %v", c.tok.Account, err)
		}
		notFound()
	}
	c.acct = acct
	c.v = view{base: baseURL(r), a: acct, plain: map[string]string{}}
	c.code = http.StatusOK
	h(c)
	return c.code, c.out
}

// unauthorized: what the CLI reads as an auth error ({"error": …} with 401).
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="Foreman"`)
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "Unauthorized"})
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + Mount
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}

func (c *call) reply(code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		must(err)
	}
	c.code, c.out = code, append(b, '\n')
}

func (c *call) ok(v any)   { c.reply(http.StatusOK, v) }
func (c *call) noContent() { c.code, c.out = http.StatusNoContent, nil }
func (c *call) created(loc string, v any) {
	c.hdr.Set("Location", loc)
	c.reply(http.StatusCreated, v)
}

// body decodes the JSON request body into v. The real API also takes Rails-style
// {"card": {…}}; a single object under the wrapper key is unwrapped.
func (c *call) body(wrapper string, v any) {
	raw := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(c.raw))) > 0 {
		if err := json.Unmarshal(c.raw, &raw); err != nil {
			fail(http.StatusBadRequest, "Bad request: the body is not a JSON object")
		}
	}
	data, _ := json.Marshal(raw)
	if inner, ok := raw[wrapper]; ok && len(inner) > 0 && inner[0] == '{' {
		data = inner
	}
	if err := json.Unmarshal(data, v); err != nil {
		fail(http.StatusUnprocessableEntity, "Invalid parameters: "+err.Error())
	}
}

// Limits: what one account may hold, so no client can make the store, or an answer, huge.
const (
	maxTitle    = 1 << 10   // card titles, step content, board, column and tag names
	maxText     = 128 << 10 // a description or comment, as stored
	maxComments = 1000      // per card
	maxSteps    = 1000      // per card
	maxCards    = 10000     // per account
	maxBoards   = 1000      // per account
	maxColumns  = 100       // per board
	maxTags     = 1000      // per account
	maxTerms    = 10        // search words
	pageBudget  = 1 << 20   // a list page stops growing near this many bytes
)

// short checks a one-line field: present and at most maxTitle bytes.
func short(field, v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		fail(http.StatusUnprocessableEntity, field+" can't be blank")
	}
	if len(v) > maxTitle {
		fail(http.StatusUnprocessableEntity, field+" is too long (at most 1 KB)")
	}
	return v
}

// long stores rich text, at most maxText bytes once stored.
func long(field, v string) string {
	h := RichText(v)
	if len(h) > maxText {
		fail(http.StatusUnprocessableEntity, field+" is too long (at most 128 KB)")
	}
	return h
}

// seg: a path value with an optional ".json" suffix removed.
func (c *call) seg(name string) string { return strings.TrimSuffix(c.r.PathValue(name), ".json") }

func (c *call) save(card *Card) {
	card.LastActiveAt = c.st.Stamp()
	must(c.st.SaveCard(c.acct.ID, card))
}

func (c *call) saveAccount() { must(c.st.SaveAccount(c.acct)) }

func must(err error) {
	if err != nil {
		log.Printf("fizzy api: %v", err)
		fail(http.StatusInternalServerError, "Internal Server Error")
	}
}

func (c *call) user() *User {
	u := c.acct.User(c.tok.UserID)
	if u == nil {
		fail(http.StatusUnauthorized, "Unauthorized")
	}
	return u
}

func (c *call) loadBoard(id string) *Board {
	b, err := c.st.Board(c.acct.ID, id)
	if errors.Is(err, ErrNotFound) {
		notFound()
	}
	must(err)
	return b
}

func (c *call) loadCard() *Card {
	n := CardNumber(c.seg("card"))
	if n == 0 {
		notFound()
	}
	card, err := c.st.Card(c.acct.ID, n)
	if errors.Is(err, ErrNotFound) {
		notFound()
	}
	must(err)
	return card
}

func (c *call) boardOf(card *Card) *Board {
	b, err := c.st.Board(c.acct.ID, card.BoardID)
	if err != nil {
		return nil
	}
	return b
}

// page cuts one page from n items of about the same size.
func (c *call) page(n int) (from, to int) { return c.pageSized(n, nil) }

// pageSized cuts one page from n items: at most per items, and fewer once size (an estimate
// of each item's JSON) passes pageBudget. Pages are cut the same way on every request, so
// ?page=N stays meaningful. It sets the Link header when more follow.
func (c *call) pageSized(n int, size func(int) int) (from, to int) {
	p := 1
	if s := c.r.URL.Query().Get("page"); s != "" {
		var err error
		if p, err = strconv.Atoi(s); err != nil || p < 1 || p > 1_000_000 {
			fail(http.StatusBadRequest, "Bad page")
		}
	}
	if size == nil {
		from = min((p-1)*c.per, n)
		to = min(from+c.per, n)
	} else {
		start := 0
		for page := 1; ; page++ {
			end, bytes := start, 0
			for end < n && end-start < c.per {
				sz := size(end)
				if end > start && bytes+sz > pageBudget {
					break
				}
				bytes += sz
				end++
			}
			if page == p || end >= n {
				from, to = start, end
				if page != p {
					from = n
					to = n
				}
				break
			}
			start = end
		}
	}
	if to < n {
		q := c.r.URL.Query()
		q.Set("page", strconv.Itoa(p+1))
		u := *c.r.URL
		u.RawQuery = q.Encode()
		next := strings.TrimSuffix(c.v.base, Mount) + u.RequestURI()
		c.hdr.Set("Link", "<"+next+`>; rel="next"`)
	}
	return from, to
}

// stamp parses a client's created_at; "" keeps now.
func (c *call) stamp(s string) string {
	if s == "" {
		return c.st.Stamp()
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		if t, err = time.Parse("2006-01-02", s); err != nil {
			fail(http.StatusUnprocessableEntity, "created_at must be an ISO 8601 time")
		}
	}
	return t.UTC().Format(TimeFormat)
}

// note adds a system comment, as Fizzy records moves.
func (c *call) note(card *Card, text string) {
	n := len(c.acct.Users)
	sys := c.acct.System(c.st.Stamp())
	if len(c.acct.Users) != n {
		c.saveAccount()
	}
	if len(card.Comments) >= maxComments {
		return
	}
	now := c.st.Stamp()
	card.Comments = append(card.Comments, &Comment{ID: NewID(), Body: RichText(text), CreatorID: sys.ID, CreatedAt: now, UpdatedAt: now})
}

// ---------------------------------------------------------------- identity, users, tags

func (a *API) identity(c *call) {
	u := c.user()
	c.ok(map[string]any{"id": u.ID, "email_address": u.Email, "accounts": []any{map[string]any{
		"id": c.acct.ID, "name": c.acct.Name, "slug": "/" + c.acct.ID, "created_at": c.acct.CreatedAt, "url": c.v.acctURL(),
		"user": c.v.userJSON(u)}}})
}

func (a *API) users(c *call) {
	out := []any{}
	for _, u := range c.acct.Users {
		if u.Role != "system" {
			out = append(out, c.v.userJSON(u))
		}
	}
	from, to := c.page(len(out))
	c.ok(out[from:to])
}

func (a *API) showUser(c *call) {
	u := c.acct.User(c.seg("user"))
	if u == nil {
		notFound()
	}
	c.ok(c.v.userJSON(u))
}

func (a *API) tags(c *call) {
	out := []any{}
	for _, t := range c.acct.Tags {
		out = append(out, c.v.tag(t))
	}
	from, to := c.page(len(out))
	c.ok(out[from:to])
}

// ---------------------------------------------------------------- boards and columns

func (a *API) boards(c *call) {
	bs, err := c.st.Boards(c.acct.ID)
	must(err)
	out := []any{}
	for _, b := range bs {
		out = append(out, c.v.board(b))
	}
	from, to := c.page(len(out))
	c.ok(out[from:to])
}

func (a *API) showBoard(c *call) { c.ok(c.v.board(c.loadBoard(c.seg("board")))) }

func (a *API) createBoard(c *call) {
	var in struct {
		Name         string `json:"name"`
		AllAccess    *bool  `json:"all_access"`
		AutoPostpone int    `json:"auto_postpone_period_in_days"`
	}
	c.body("board", &in)
	name := short("Name", in.Name)
	bs, err := c.st.Boards(c.acct.ID)
	must(err)
	if len(bs) >= maxBoards {
		fail(http.StatusUnprocessableEntity, "This account has as many boards as it may")
	}
	b := &Board{ID: NewID(), Name: name, AllAccess: in.AllAccess == nil || *in.AllAccess,
		AutoPostpone: in.AutoPostpone, CreatedAt: c.st.Stamp(), CreatorID: c.user().ID, Columns: []*Column{}}
	if b.AutoPostpone == 0 {
		b.AutoPostpone = 30
	}
	must(c.st.SaveBoard(c.acct.ID, b))
	c.created(c.v.acctURL()+"/boards/"+b.ID+".json", c.v.board(b))
}

func (a *API) columns(c *call) {
	b := c.loadBoard(c.seg("board"))
	out := []any{}
	for _, col := range b.Columns {
		out = append(out, c.v.column(b, col))
	}
	c.ok(out)
}

// colors: Fizzy's column palette, value to name.
var colors = map[string]string{
	"var(--color-card-default)": "Blue", "var(--color-card-1)": "Gray", "var(--color-card-2)": "Tan",
	"var(--color-card-3)": "Yellow", "var(--color-card-4)": "Lime", "var(--color-card-5)": "Aqua",
	"var(--color-card-6)": "Violet", "var(--color-card-7)": "Purple", "var(--color-card-8)": "Pink",
}

func (a *API) createColumn(c *call) {
	b := c.loadBoard(c.seg("board"))
	var in struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	c.body("column", &in)
	name := short("Name", in.Name)
	if len(in.Color) > 100 {
		fail(http.StatusUnprocessableEntity, "Color is too long")
	}
	if len(b.Columns) >= maxColumns {
		fail(http.StatusUnprocessableEntity, "This board has as many columns as it may")
	}
	col := &Column{ID: NewID(), Name: name, ColorValue: orDefault(in.Color, "var(--color-card-default)"), CreatedAt: c.st.Stamp()}
	col.ColorName = orDefault(colors[col.ColorValue], "Custom")
	b.Columns = append(b.Columns, col)
	must(c.st.SaveBoard(c.acct.ID, b))
	c.created(c.v.acctURL()+"/boards/"+b.ID+"/columns/"+col.ID+".json", c.v.column(b, col))
}

// showColumn: a real column, or the board's closed / not_now / stream lane as a card list.
func (a *API) showColumn(c *call) {
	b := c.loadBoard(c.seg("board"))
	id := c.seg("column")
	lane := map[string]string{"closed": "closed", "not_now": "not_now", "stream": "maybe"}[id]
	if lane != "" {
		q := url.Values{"board_ids[]": {b.ID}, "indexed_by": {lane}}
		c.listCards(q)
		return
	}
	col := b.Column(id)
	if col == nil {
		notFound()
	}
	c.ok(c.v.column(b, col))
}

// ---------------------------------------------------------------- cards

func (a *API) cards(c *call) { c.listCards(c.r.URL.Query()) }

func (c *call) listCards(q url.Values) {
	cards, err := c.st.Cards(c.acct.ID)
	must(err)
	boards, err := c.st.Boards(c.acct.ID)
	must(err)
	byID := map[string]*Board{}
	for _, b := range boards {
		byID[b.ID] = b
	}
	f, msg := newFilter(q, c.acct, c.st.Now())
	if msg != "" {
		fail(http.StatusBadRequest, msg)
	}
	var hits []*Card
	for _, card := range cards {
		if byID[card.BoardID] != nil && f.match(card, c.v.descText) {
			hits = append(hits, card)
		}
	}
	f.sort(hits)
	from, to := c.pageSized(len(hits), func(i int) int { return cardSize(hits[i]) })
	out := []any{}
	for _, card := range hits[from:to] {
		out = append(out, c.v.card(card, byID[card.BoardID], false))
	}
	c.ok(out)
}

func (a *API) search(c *call) {
	q := strings.TrimSpace(c.r.URL.Query().Get("q"))
	if q == "" {
		c.ok([]any{})
		return
	}
	if n := CardNumber(q); n > 0 {
		if card, err := c.st.Card(c.acct.ID, n); err == nil {
			c.ok([]any{c.v.card(card, c.boardOf(card), false)})
			return
		}
	}
	vals := url.Values{"indexed_by": {"everything"}}
	for _, t := range strings.Fields(q) {
		vals.Add("terms[]", t)
	}
	c.listCards(vals)
}

func (a *API) showCard(c *call) {
	card := c.loadCard()
	c.ok(c.v.card(card, c.boardOf(card), true))
}

func (a *API) createCard(c *call) {
	var in struct {
		BoardID     string `json:"board_id"`
		Title       string `json:"title"`
		Description string `json:"description"`
		CreatedAt   string `json:"created_at"`
	}
	c.body("card", &in)
	if in.BoardID == "" {
		in.BoardID = c.r.URL.Query().Get("board_id")
	}
	b := c.loadBoard(in.BoardID)
	title, desc := short("Title", in.Title), long("Description", in.Description)
	at := c.stamp(in.CreatedAt)
	count, err := c.st.CountCards(c.acct.ID)
	must(err)
	if count >= maxCards {
		fail(http.StatusUnprocessableEntity, "This account has as many cards as it may")
	}
	n, err := c.st.NextNumber(c.acct)
	must(err)
	card := &Card{ID: NewID(), Number: n, BoardID: b.ID, Title: title,
		Description: desc, Status: "published", CreatedAt: at, CreatorID: c.user().ID,
		Assignees: []string{}, Tags: []string{}, Steps: []*Step{}, Comments: []*Comment{}}
	c.save(card)
	c.created(c.v.cardURL(n)+".json", c.v.card(card, b, true))
}

func (a *API) updateCard(c *call) {
	card := c.loadCard()
	var in struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		CreatedAt   string  `json:"created_at"`
	}
	c.body("card", &in)
	if in.Title != nil {
		card.Title = short("Title", *in.Title)
	}
	if in.Description != nil {
		card.Description = long("Description", *in.Description)
	}
	if in.CreatedAt != "" {
		card.CreatedAt = c.stamp(in.CreatedAt)
	}
	c.save(card)
	c.ok(c.v.card(card, c.boardOf(card), true))
}

func (a *API) deleteCard(c *call) {
	card := c.loadCard()
	must(c.st.DeleteCard(c.acct.ID, card.Number))
	c.noContent()
}

func (a *API) close(c *call) {
	card := c.loadCard()
	if !card.Closed {
		card.Closed, card.Postponed = true, false
		card.ClosedAt, card.CloserID = c.st.Stamp(), c.user().ID
		c.note(card, "Closed by "+c.user().Name+".")
	}
	c.save(card)
	c.noContent()
}

func (a *API) reopen(c *call) {
	card := c.loadCard()
	if card.Closed {
		card.Closed, card.ClosedAt, card.CloserID = false, "", ""
		c.note(card, "Reopened by "+c.user().Name+".")
	}
	c.save(card)
	c.noContent()
}

// triage puts a card in a column: on the board, open, not postponed.
func (a *API) triage(c *call) {
	card := c.loadCard()
	var in struct {
		ColumnID string `json:"column_id"`
	}
	c.body("card", &in)
	b := c.boardOf(card)
	if b == nil {
		notFound()
	}
	col := b.Column(in.ColumnID)
	if col == nil {
		fail(http.StatusNotFound, "Column not found on this card's board")
	}
	if card.ColumnID != col.ID || card.Closed || card.Postponed {
		card.ColumnID, card.Postponed, card.Closed, card.ClosedAt, card.CloserID = col.ID, false, false, "", ""
		c.note(card, "Moved to "+col.Name+" by "+c.user().Name+".")
	}
	c.save(card)
	c.noContent()
}

func (a *API) untriage(c *call) {
	card := c.loadCard()
	if card.ColumnID != "" || card.Closed || card.Postponed {
		card.ColumnID, card.Postponed, card.Closed, card.ClosedAt, card.CloserID = "", false, false, "", ""
		c.note(card, "Moved back to Maybe? by "+c.user().Name+".")
	}
	c.save(card)
	c.noContent()
}

func (a *API) postpone(c *call) {
	card := c.loadCard()
	if !card.Postponed || card.Closed {
		card.Postponed, card.ColumnID, card.Closed, card.ClosedAt, card.CloserID = true, "", false, "", ""
		c.note(card, "Moved to Not Now by "+c.user().Name+".")
	}
	c.save(card)
	c.noContent()
}

// tagging toggles a tag by title, making the tag when it is new.
func (a *API) tagging(c *call) {
	card := c.loadCard()
	var in struct {
		TagTitle string `json:"tag_title"`
	}
	c.body("tagging", &in)
	title := short("Tag title", strings.TrimPrefix(strings.TrimSpace(in.TagTitle), "#"))
	t := c.acct.TagTitled(title)
	if t == nil {
		if len(c.acct.Tags) >= maxTags {
			fail(http.StatusUnprocessableEntity, "This account has as many tags as it may")
		}
		t = &Tag{ID: NewID(), Title: title, CreatedAt: c.st.Stamp()}
		c.acct.Tags = append(c.acct.Tags, t)
		c.saveAccount()
	}
	card.Tags = toggle(card.Tags, t.ID)
	c.save(card)
	c.noContent()
}

func (a *API) assign(c *call) {
	card := c.loadCard()
	var in struct {
		AssigneeID string `json:"assignee_id"`
	}
	c.body("assignment", &in)
	u := c.acct.User(in.AssigneeID)
	if u == nil || u.Role == "system" {
		fail(http.StatusNotFound, "User not found")
	}
	card.Assignees = toggle(card.Assignees, u.ID)
	c.save(card)
	c.noContent()
}

func (a *API) selfAssign(c *call) {
	card := c.loadCard()
	card.Assignees = toggle(card.Assignees, c.user().ID)
	c.save(card)
	c.noContent()
}

func (a *API) gild(on bool) func(*call) {
	return func(c *call) {
		card := c.loadCard()
		card.Golden = on
		c.save(card)
		c.noContent()
	}
}

func toggle(ids []string, id string) []string {
	for i, x := range ids {
		if x == id {
			return append(ids[:i:i], ids[i+1:]...)
		}
	}
	return append(ids, id)
}

// ---------------------------------------------------------------- comments

func (a *API) comments(c *call) {
	card := c.loadCard()
	from, to := c.pageSized(len(card.Comments), func(i int) int { return 2*len(card.Comments[i].Body) + 1500 })
	out := []any{}
	for _, cm := range card.Comments[from:to] {
		out = append(out, c.v.comment(card, cm))
	}
	c.ok(out)
}

func findComment(card *Card, id string) (int, *Comment) {
	for i, cm := range card.Comments {
		if cm.ID == id {
			return i, cm
		}
	}
	notFound()
	return 0, nil
}

func (a *API) showComment(c *call) {
	card := c.loadCard()
	_, cm := findComment(card, c.seg("comment"))
	c.ok(c.v.comment(card, cm))
}

func (a *API) createComment(c *call) {
	card := c.loadCard()
	var in struct {
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
	}
	c.body("comment", &in)
	if strings.TrimSpace(in.Body) == "" {
		fail(http.StatusUnprocessableEntity, "Body can't be blank")
	}
	body := long("Body", in.Body)
	if len(card.Comments) >= maxComments {
		fail(http.StatusUnprocessableEntity, "This card has as many comments as it may")
	}
	at := c.stamp(in.CreatedAt)
	cm := &Comment{ID: NewID(), Body: body, CreatorID: c.user().ID, CreatedAt: at, UpdatedAt: at}
	card.Comments = append(card.Comments, cm)
	c.save(card)
	c.created(c.v.cardURL(card.Number)+"/comments/"+cm.ID+".json", c.v.comment(card, cm))
}

func (a *API) updateComment(c *call) {
	card := c.loadCard()
	_, cm := findComment(card, c.seg("comment"))
	c.mine(cm.CreatorID)
	var in struct {
		Body string `json:"body"`
	}
	c.body("comment", &in)
	if strings.TrimSpace(in.Body) == "" {
		fail(http.StatusUnprocessableEntity, "Body can't be blank")
	}
	cm.Body, cm.UpdatedAt = long("Body", in.Body), c.st.Stamp()
	c.save(card)
	c.ok(c.v.comment(card, cm))
}

func (a *API) deleteComment(c *call) {
	card := c.loadCard()
	i, cm := findComment(card, c.seg("comment"))
	c.mine(cm.CreatorID)
	card.Comments = append(card.Comments[:i:i], card.Comments[i+1:]...)
	c.save(card)
	c.noContent()
}

// mine: only a comment's author, or the account owner, may change it.
func (c *call) mine(creator string) {
	if u := c.user(); u.ID != creator && u.Role != "owner" && u.Role != "admin" {
		fail(http.StatusForbidden, "You can only change your own comments")
	}
}

// ---------------------------------------------------------------- steps

func (a *API) steps(c *call) {
	card := c.loadCard()
	out := []any{}
	for _, s := range card.Steps {
		out = append(out, c.v.step(card, s))
	}
	c.ok(out)
}

func findStep(card *Card, id string) (int, *Step) {
	for i, s := range card.Steps {
		if s.ID == id {
			return i, s
		}
	}
	notFound()
	return 0, nil
}

func (a *API) showStep(c *call) {
	card := c.loadCard()
	_, s := findStep(card, c.seg("step"))
	c.ok(c.v.step(card, s))
}

func (a *API) createStep(c *call) {
	card := c.loadCard()
	var in struct {
		Content   string `json:"content"`
		Completed bool   `json:"completed"`
	}
	c.body("step", &in)
	content := short("Content", in.Content)
	if len(card.Steps) >= maxSteps {
		fail(http.StatusUnprocessableEntity, "This card has as many steps as it may")
	}
	s := &Step{ID: NewID(), Content: content, Completed: in.Completed}
	card.Steps = append(card.Steps, s)
	c.save(card)
	c.created(c.v.cardURL(card.Number)+"/steps/"+s.ID+".json", c.v.step(card, s))
}

func (a *API) updateStep(c *call) {
	card := c.loadCard()
	_, s := findStep(card, c.seg("step"))
	var in struct {
		Content   *string `json:"content"`
		Completed *bool   `json:"completed"`
	}
	c.body("step", &in)
	if in.Content != nil {
		s.Content = short("Content", *in.Content)
	}
	if in.Completed != nil {
		s.Completed = *in.Completed
	}
	c.save(card)
	c.ok(c.v.step(card, s))
}

func (a *API) deleteStep(c *call) {
	card := c.loadCard()
	i, _ := findStep(card, c.seg("step"))
	card.Steps = append(card.Steps[:i:i], card.Steps[i+1:]...)
	c.save(card)
	c.noContent()
}

// cardSize estimates a listed card's JSON: its text twice (HTML and plain), escaped, and the
// nested board, creator and assignees.
func cardSize(c *Card) int {
	return 2*len(c.Title) + 3*len(c.Description) + 3000 + 600*len(c.Assignees) + 100*len(c.Tags)
}
