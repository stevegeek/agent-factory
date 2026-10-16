// Package fizz keeps project boards (boards, columns, cards, steps, comments) as JSON files and
// serves them over a Fizzy-compatible API, so the fizzy CLI can point at Foreman.
package fizz

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Layout under the store directory (<factory root>/boards):
//
//	tokens.json                 API tokens, hashed
//	<account>/account.json      the account, its users and tags
//	<account>/boards/<id>.json  a board and its columns
//	<account>/cards/<n>.json    a card with its steps and comments
//
// Every write is a temp file renamed over the old one. Callers hold Mu around a whole
// request, so a read-modify-write never interleaves with another in this process.

const TimeFormat = "2006-01-02T15:04:05.000Z"

var (
	accountRe = regexp.MustCompile(`^[0-9]{1,16}$`)
	idRe      = regexp.MustCompile(`^[0-9a-z]{1,64}$`)
	numberRe  = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)
)

func ValidAccount(s string) bool { return accountRe.MatchString(s) }
func ValidID(s string) bool      { return idRe.MatchString(s) }

// CardNumber parses a card number from a path segment; 0 when it is not one.
func CardNumber(s string) int {
	if !numberRe.MatchString(s) {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

var ErrNotFound = errors.New("not found")

const MaxNumber = 999_999_999 // nine digits, as numberRe allows

type Account struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	CreatedAt  string  `json:"created_at"`
	LastNumber int     `json:"last_number"` // numbers are never reused, even after a delete
	Users      []*User `json:"users"`
	Tags       []*Tag  `json:"tags"`
}

type User struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email_address"`
	Role      string `json:"role"` // owner, admin, member, system
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at"`
}

type Tag struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
}

type Board struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	AllAccess    bool      `json:"all_access"`
	AutoPostpone int       `json:"auto_postpone_period_in_days"`
	CreatedAt    string    `json:"created_at"`
	CreatorID    string    `json:"creator_id"`
	Columns      []*Column `json:"columns"`
}

type Column struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ColorName  string `json:"color_name"`
	ColorValue string `json:"color_value"`
	CreatedAt  string `json:"created_at"`
}

// Card: ColumnID "" is Maybe? (triage). Closed and Postponed are the Done and Not Now lanes.
type Card struct {
	ID           string     `json:"id"`
	Number       int        `json:"number"`
	BoardID      string     `json:"board_id"`
	ColumnID     string     `json:"column_id"`
	Title        string     `json:"title"`
	Description  string     `json:"description_html"`
	Status       string     `json:"status"`
	Closed       bool       `json:"closed"`
	ClosedAt     string     `json:"closed_at,omitempty"`
	CloserID     string     `json:"closer_id,omitempty"`
	Postponed    bool       `json:"postponed"`
	Golden       bool       `json:"golden"`
	CreatedAt    string     `json:"created_at"`
	LastActiveAt string     `json:"last_active_at"`
	CreatorID    string     `json:"creator_id"`
	Assignees    []string   `json:"assignee_ids"`
	Tags         []string   `json:"tag_ids"`
	Steps        []*Step    `json:"steps"`
	Comments     []*Comment `json:"comments"`
}

type Step struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Completed bool   `json:"completed"`
}

type Comment struct {
	ID        string `json:"id"`
	Body      string `json:"body_html"`
	CreatorID string `json:"creator_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type Store struct {
	Dir      string
	Now      func() time.Time
	MaxBytes int64 // writes are refused once the store is this big

	mu     sync.Mutex
	lockf  *os.File
	sizeMu sync.Mutex
	size   int64
	sizeAt time.Time
}

func Open(dir string) *Store { return &Store{Dir: dir, Now: time.Now, MaxBytes: 200 << 20} }

// Lock holds the store against this process (a mutex) and every other one (flock on
// <dir>/.lock): the server, the web views, an import and the token commands never interleave.
func (s *Store) Lock() error {
	s.mu.Lock()
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		s.mu.Unlock()
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, ".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err == nil {
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			f.Close()
		}
	}
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("lock %s: %w", s.Dir, err)
	}
	s.lockf = f
	return nil
}

func (s *Store) Unlock() {
	if s.lockf != nil {
		syscall.Flock(int(s.lockf.Fd()), syscall.LOCK_UN)
		s.lockf.Close()
		s.lockf = nil
	}
	s.mu.Unlock()
}

// Full: the store has reached MaxBytes. The walk is redone at most every 10 s.
func (s *Store) Full() bool {
	s.sizeMu.Lock()
	defer s.sizeMu.Unlock()
	if s.MaxBytes <= 0 {
		return false
	}
	if now := time.Now(); s.sizeAt.IsZero() || now.Sub(s.sizeAt) > 10*time.Second {
		var total int64
		filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if info, err := d.Info(); err == nil {
					total += info.Size()
				}
			}
			return nil
		})
		s.size, s.sizeAt = total, now
	}
	return s.size >= s.MaxBytes
}

func (s *Store) Stamp() string { return s.Now().UTC().Format(TimeFormat) }

// NewID: 25 random base36 characters, the shape Fizzy uses.
func NewID() string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 25)
	max := big.NewInt(int64(len(digits)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(err)
		}
		b[i] = digits[n.Int64()]
	}
	return string(b)
}

func (s *Store) accountDir(acct string) (string, error) {
	if !ValidAccount(acct) {
		return "", ErrNotFound
	}
	return filepath.Join(s.Dir, acct), nil
}

// Accounts lists the account ids in the store.
func (s *Store) Accounts() []string {
	es, _ := os.ReadDir(s.Dir)
	var out []string
	for _, e := range es {
		if e.IsDir() && ValidAccount(e.Name()) {
			if _, err := os.Stat(filepath.Join(s.Dir, e.Name(), "account.json")); err == nil {
				out = append(out, e.Name())
			}
		}
	}
	sort.Strings(out)
	return out
}

func (s *Store) Account(acct string) (*Account, error) {
	dir, err := s.accountDir(acct)
	if err != nil {
		return nil, err
	}
	var a Account
	if err := readJSON(filepath.Join(dir, "account.json"), &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *Store) SaveAccount(a *Account) error {
	dir, err := s.accountDir(a.ID)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, "account.json"), a)
}

func (s *Store) Boards(acct string) ([]*Board, error) {
	var out []*Board
	err := s.each(acct, "boards", func(data []byte) error {
		var b Board
		if err := json.Unmarshal(data, &b); err != nil {
			return err
		}
		out = append(out, &b)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, err
}

func (s *Store) Board(acct, id string) (*Board, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	dir, err := s.accountDir(acct)
	if err != nil {
		return nil, err
	}
	var b Board
	if err := readJSON(filepath.Join(dir, "boards", id+".json"), &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) SaveBoard(acct string, b *Board) error {
	if !ValidID(b.ID) {
		return fmt.Errorf("bad board id %q", b.ID)
	}
	dir, err := s.accountDir(acct)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, "boards", b.ID+".json"), b)
}

func (s *Store) Cards(acct string) ([]*Card, error) {
	var out []*Card
	err := s.each(acct, "cards", func(data []byte) error {
		var c Card
		if err := json.Unmarshal(data, &c); err != nil {
			return err
		}
		out = append(out, &c)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, err
}

func (s *Store) Card(acct string, n int) (*Card, error) {
	dir, err := s.accountDir(acct)
	if err != nil || n <= 0 {
		return nil, ErrNotFound
	}
	var c Card
	if err := readJSON(filepath.Join(dir, "cards", strconv.Itoa(n)+".json"), &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) SaveCard(acct string, c *Card) error {
	dir, err := s.accountDir(acct)
	if err != nil {
		return err
	}
	if c.Number <= 0 || !ValidID(c.ID) {
		return fmt.Errorf("bad card %d %q", c.Number, c.ID)
	}
	return writeFile(filepath.Join(dir, "cards", strconv.Itoa(c.Number)+".json"), c)
}

// DeleteCard moves a card to cards/.trash/<n>-<unixnano>.json: gone from the API, kept for RestoreCard.
func (s *Store) DeleteCard(acct string, n int) error {
	dir, err := s.accountDir(acct)
	if err != nil || n <= 0 {
		return ErrNotFound
	}
	from := filepath.Join(dir, "cards", strconv.Itoa(n)+".json")
	if _, err := os.Stat(from); errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	trash := filepath.Join(dir, "cards", ".trash")
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return err
	}
	return os.Rename(from, filepath.Join(trash, fmt.Sprintf("%d-%d.json", n, s.Now().UnixNano())))
}

// RestoreCard brings back the card most recently deleted with number n.
func (s *Store) RestoreCard(acct string, n int) error {
	dir, err := s.accountDir(acct)
	if err != nil || n <= 0 {
		return ErrNotFound
	}
	to := filepath.Join(dir, "cards", strconv.Itoa(n)+".json")
	if _, err := os.Stat(to); err == nil {
		return fmt.Errorf("card #%d exists; not restoring over it", n)
	}
	es, _ := os.ReadDir(filepath.Join(dir, "cards", ".trash"))
	latest, at := "", int64(-1)
	for _, e := range es {
		num, stamp, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".json"), "-")
		t, err := strconv.ParseInt(stamp, 10, 64)
		if ok && err == nil && num == strconv.Itoa(n) && t > at {
			latest, at = e.Name(), t
		}
	}
	if latest == "" {
		return ErrNotFound
	}
	return os.Rename(filepath.Join(dir, "cards", ".trash", latest), to)
}

// CountCards counts an account's live cards from their file names alone.
func (s *Store) CountCards(acct string) (int, error) {
	dir, err := s.accountDir(acct)
	if err != nil {
		return 0, err
	}
	es, err := os.ReadDir(filepath.Join(dir, "cards"))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	n := 0
	for _, e := range es {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			n++
		}
	}
	return n, err
}

// NextNumber reserves the account's next card number: one past the last given, skipping
// any number a card file already holds.
func (s *Store) NextNumber(a *Account) (int, error) {
	dir, err := s.accountDir(a.ID)
	if err != nil {
		return 0, err
	}
	n := a.LastNumber + 1
	for ; n <= MaxNumber; n++ {
		if _, err := os.Stat(filepath.Join(dir, "cards", strconv.Itoa(n)+".json")); errors.Is(err, os.ErrNotExist) {
			break
		}
	}
	if n > MaxNumber {
		return 0, errors.New("card numbers are used up")
	}
	a.LastNumber = n
	return n, s.SaveAccount(a)
}

func (s *Store) each(acct, kind string, f func([]byte) error) error {
	dir, err := s.accountDir(acct)
	if err != nil {
		return err
	}
	es, err := os.ReadDir(filepath.Join(dir, kind))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range es {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, kind, e.Name()))
		if err != nil {
			return err
		}
		if err := f(data); err != nil {
			return fmt.Errorf("%s/%s: %w", kind, e.Name(), err)
		}
	}
	return nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// writeFile replaces path atomically: a reader sees the old file or the new one, never half.
func writeFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// ---------------------------------------------------------------- lookups

func (a *Account) User(id string) *User {
	for _, u := range a.Users {
		if u.ID == id {
			return u
		}
	}
	return nil
}

func (a *Account) UserNamed(name string) *User {
	for _, u := range a.Users {
		if strings.EqualFold(u.Name, name) {
			return u
		}
	}
	return nil
}

// System: the user that signs automatic comments (moves); made on first use.
func (a *Account) System(now string) *User {
	for _, u := range a.Users {
		if u.Role == "system" {
			return u
		}
	}
	u := &User{ID: NewID(), Name: "System", Role: "system", Active: true, CreatedAt: now}
	a.Users = append(a.Users, u)
	return u
}

func (a *Account) Tag(id string) *Tag {
	for _, t := range a.Tags {
		if t.ID == id {
			return t
		}
	}
	return nil
}

func (a *Account) TagTitled(title string) *Tag {
	for _, t := range a.Tags {
		if strings.EqualFold(t.Title, title) {
			return t
		}
	}
	return nil
}

func (b *Board) Column(id string) *Column {
	for _, c := range b.Columns {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// Init creates an account with its owner, or returns the one already there.
func (s *Store) Init(acct, name, owner, email string) (*Account, error) {
	if !ValidAccount(acct) {
		return nil, fmt.Errorf("account id must be digits, like 6182510: %q", acct)
	}
	if a, err := s.Account(acct); err == nil {
		return a, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	now := s.Stamp()
	a := &Account{ID: acct, Name: name, CreatedAt: now,
		Users: []*User{{ID: NewID(), Name: owner, Email: email, Role: "owner", Active: true, CreatedAt: now}}}
	a.System(now)
	return a, s.SaveAccount(a)
}
