package auth

import (
	"bufio"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/fsx"
)

// The people who log in to Foreman, each with an email and a password. They
// may all do the same things; what differs is their desk, which shows the
// asks of their desk projects (none named: every project) and anything put
// to them by name. Users live in foreman-users.json beside the secret
// (0600); Foreman's Settings and `foreman users` manage it. While nobody is
// in it the main secret logs in, to add the first; after that only users do.

var (
	userName  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)                     // re: go
	userEmail = regexp.MustCompile(`^[^@\s]{1,64}@[^@\s]{1,189}\.[^@\s]{1,63}$`) // re: go
)

const (
	// MinPassword and MaxPassword bound a password's length in characters.
	MinPassword = 10
	MaxPassword = 256
	// PasswordCost is the PBKDF2-SHA256 rounds a new password is hashed with.
	PasswordCost = 600_000
	maxDismissed = 500
)

// User is one person's login. Name is theirs on the board: what their
// answers and notes record as by, what questions are put to and tickets
// assigned to, and what @name mentions; it belongs in [board] people.
type User struct {
	Name     string   `json:"name"`
	Email    string   `json:"email"`
	Password string   `json:"password"` // "pbkdf2-sha256$<rounds>$<salt>$<key>", base64url
	Desk     []string `json:"desk"`     // the projects on their desk; none: every project
	// Since is the unix time their sessions must be issued at or after:
	// logging out, or a new password, ends the ones before.
	Since int64 `json:"sessions_since,omitempty"`
	// Dismissed is, by ticket, the time of the last mention of them they
	// put away: an older mention stays off their desk.
	Dismissed map[string]string `json:"dismissed,omitempty"`
	Added     string            `json:"added"`
}

// Sees reports whether the project's asks are on this user's desk; a nil
// user (the main secret, before there are users) sees every project.
func (u *User) Sees(project string) bool {
	return u == nil || len(u.Desk) == 0 || slices.Contains(u.Desk, project)
}

// Users is the users file, re-read when it changes (a new inode, mtime or
// size: a save is a rename, so a change within one mtime tick still shows). It is safe for
// concurrent use.
type Users struct {
	path  string
	clock clock.Clock
	cost  int
	mu    sync.Mutex
	seen  os.FileInfo // the file as last read: re-read when it is another file, or its mtime or size changed
	list  []User
	// retired is the unix time of the last user added: the main secret's
	// sessions from before it never come back, even when the last user is
	// removed
	retired int64
	write   sync.Mutex // one change at a time: read, change, save
}

// usersFile is the file's shape.
type usersFile struct {
	Users   []User `json:"users"`
	Retired int64  `json:"secret_sessions_before,omitempty"`
}

// reserved are names the board gives a meaning of its own (the ticket
// API's tokens.ReservedNames): no person has one.
func reserved(name string) bool {
	return slices.Contains([]string{"factory", "maintainer", "human", "system", "me"}, name)
}

// NewUsers is the store at path (foreman-users.json beside the secret).
// clk stamps new users; nil is the system clock.
func NewUsers(path string, clk clock.Clock) *Users {
	if clk == nil {
		clk = clock.System{}
	}
	return &Users{path: path, clock: clk, cost: PasswordCost}
}

// WithCost hashes new passwords with that many rounds (tests: a few).
func (s *Users) WithCost(rounds int) *Users {
	s.cost = rounds
	return s
}

// UsersFile is the users file beside the secret.
func UsersFile(secretFile string) string {
	return filepath.Join(filepath.Dir(secretFile), "foreman-users.json")
}

func (s *Users) load() []User {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Stat(s.path)
	if err != nil {
		s.list, s.seen = nil, nil
		return nil
	}
	if s.seen == nil || !os.SameFile(s.seen, info) || !info.ModTime().Equal(s.seen.ModTime()) || info.Size() != s.seen.Size() {
		var f usersFile
		data, _ := os.ReadFile(s.path)
		if json.Unmarshal(data, &f) == nil {
			// an entry with no email or password (an older file's
			// secret-only users) is nobody: it can never log in, so it must
			// not shut the main secret out either
			s.list = slices.DeleteFunc(f.Users, func(u User) bool { return u.Email == "" || u.Password == "" })
			s.retired = f.Retired
		}
		s.seen = info
	}
	out := make([]User, len(s.list))
	for i, u := range s.list {
		u.Desk = slices.Clone(u.Desk)
		if u.Dismissed != nil {
			d := make(map[string]string, len(u.Dismissed))
			for k, v := range u.Dismissed {
				d[k] = v
			}
			u.Dismissed = d
		}
		out[i] = u
	}
	return out
}

// Any reports whether there is a user: then the main secret no longer logs in.
func (s *Users) Any() bool { return len(s.load()) > 0 }

// Retired is the unix time before which the main secret's sessions are
// over for good: when the last user was added.
func (s *Users) Retired() int64 {
	if s == nil {
		return 0
	}
	s.load()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retired
}

// List is every user, by name.
func (s *Users) List() []User {
	list := s.load()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// Names is every user's name, sorted.
func (s *Users) Names() []string {
	var out []string
	for _, u := range s.List() {
		out = append(out, u.Name)
	}
	return out
}

// Get is the user of that name, or nil.
func (s *Users) Get(name string) *User {
	for _, u := range s.load() {
		if u.Name == name {
			return &u
		}
	}
	return nil
}

// ByEmail is the user with that email (any case), or nil.
func (s *Users) ByEmail(email string) *User {
	email = normEmail(email)
	for _, u := range s.load() {
		if u.Email == email {
			return &u
		}
	}
	return nil
}

// Sees reports whether the user of that name still exists and has the
// project on their desk (the push package's Audience).
func (s *Users) Sees(name, project string) bool {
	u := s.Get(name)
	return u != nil && u.Sees(project)
}

// Login is the name of the user with this email and password, or "". The
// password is hashed even for an unknown email, so the time taken does not
// say which emails exist.
func (s *Users) Login(email, password string) string {
	u := s.ByEmail(email)
	stored := ""
	if u != nil {
		stored = u.Password
	}
	if checkPassword(stored, password, s.cost) && u != nil {
		return u.Name
	}
	return ""
}

func normEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

func hashPassword(password string, rounds int) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, rounds, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString
	return "pbkdf2-sha256$" + strconv.Itoa(rounds) + "$" + enc(salt) + "$" + enc(key), nil
}

// checkPassword reports whether password is the one stored; a stored hash
// that cannot be read ("" for none) still costs the rounds of a new one.
func checkPassword(stored, password string, rounds int) bool {
	salt, want := make([]byte, 16), []byte(nil)
	ok := false
	if p := strings.Split(stored, "$"); len(p) == 4 && p[0] == "pbkdf2-sha256" {
		n, err1 := strconv.Atoi(p[1])
		s, err2 := base64.RawURLEncoding.DecodeString(p[2])
		k, err3 := base64.RawURLEncoding.DecodeString(p[3])
		if err1 == nil && err2 == nil && err3 == nil && n > 0 && n <= 10*PasswordCost && len(k) == 32 {
			salt, want, rounds, ok = s, k, n, true
		}
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, max(rounds, 1), 32)
	return ok && err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func checkNewPassword(p string) error {
	if n := utf8.RuneCountInString(p); n < MinPassword || n > MaxPassword || !utf8.ValidString(p) {
		return fmt.Errorf("a password is %d to %d characters", MinPassword, MaxPassword)
	}
	return nil
}

func (s *Users) save(list []User) error {
	return s.saveFile(usersFile{Users: list, Retired: s.Retired()})
}

func (s *Users) saveFile(f usersFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomic(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("save users: %w", err)
	}
	return nil
}

// change applies f to the user of that name and saves.
func (s *Users) change(name string, f func(u *User) error) error {
	if s == nil {
		return fmt.Errorf("no user %s", name)
	}
	s.write.Lock()
	defer s.write.Unlock()
	list := s.load()
	for i := range list {
		if list[i].Name == name {
			if err := f(&list[i]); err != nil {
				return err
			}
			return s.save(list)
		}
	}
	return fmt.Errorf("no user %s", name)
}

// Add makes a user. Their name is theirs on the board; desk names the
// projects on their desk (none: every project).
func (s *Users) Add(name, email, password string, desk []string) error {
	email = normEmail(email)
	if !userName.MatchString(name) || reserved(name) {
		return errors.New(`a user name is lowercase letters, digits and dashes, and not factory, maintainer, human, system or me`)
	}
	if !userEmail.MatchString(email) {
		return errors.New("that is not an email address")
	}
	if err := checkNewPassword(password); err != nil {
		return err
	}
	hash, err := hashPassword(password, s.cost)
	if err != nil {
		return err
	}
	s.write.Lock()
	defer s.write.Unlock()
	list := s.load()
	for _, u := range list {
		if u.Name == name {
			return fmt.Errorf("%s already exists", name)
		}
		if u.Email == email {
			return fmt.Errorf("%s already has that email", u.Name)
		}
	}
	// Since: a user removed and added again starts with no sessions; the
	// old cookies carry only the name
	now := s.clock.Now()
	list = append(list, User{Name: name, Email: email, Password: hash, Desk: cleanDesk(desk), Since: now.Unix(),
		Added: now.UTC().Format(time.RFC3339)})
	return s.saveFile(usersFile{Users: list, Retired: now.Unix()})
}

func cleanDesk(desk []string) []string {
	out := []string{}
	for _, p := range desk {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// SetPassword gives a user a new password and ends their sessions.
func (s *Users) SetPassword(name, password string) error {
	if err := checkNewPassword(password); err != nil {
		return err
	}
	hash, err := hashPassword(password, s.cost)
	if err != nil {
		return err
	}
	now := s.clock.Now().Unix()
	return s.change(name, func(u *User) error {
		u.Password, u.Since = hash, now
		return nil
	})
}

// SetDesk names the projects on a user's desk (none: every project).
func (s *Users) SetDesk(name string, desk []string) error {
	return s.change(name, func(u *User) error {
		u.Desk = cleanDesk(desk)
		return nil
	})
}

// Revoke ends a user's sessions issued before at.
func (s *Users) Revoke(name string, at time.Time) error {
	return s.change(name, func(u *User) error {
		u.Since = at.Unix()
		return nil
	})
}

// Dismiss puts away a user's mentions on a ticket up to at (an RFC 3339
// time). The oldest dismissals go past maxDismissed.
func (s *Users) Dismiss(name, ticket, at string) error {
	return s.change(name, func(u *User) error {
		if u.Dismissed == nil {
			u.Dismissed = map[string]string{}
		}
		if at > u.Dismissed[ticket] {
			u.Dismissed[ticket] = at
		}
		for len(u.Dismissed) > maxDismissed {
			oldest := ""
			for k, v := range u.Dismissed {
				if oldest == "" || v < u.Dismissed[oldest] {
					oldest = k
				}
			}
			delete(u.Dismissed, oldest)
		}
		return nil
	})
}

// Remove deletes a user; their sessions end at once.
func (s *Users) Remove(name string) error {
	s.write.Lock()
	defer s.write.Unlock()
	all := s.load()
	keep := slices.DeleteFunc(slices.Clone(all), func(u User) bool { return u.Name == name })
	if len(keep) == len(all) {
		return fmt.Errorf("no user %s", name)
	}
	return s.save(keep)
}

// UsersCommand is `foreman users add NAME EMAIL [--desk P]...`,
// `foreman users passwd NAME`, `foreman users desk NAME [P]...`,
// `foreman users list` and `foreman users remove NAME`, on the users file
// beside secretFile. A password is read from the first line of in.
func UsersCommand(secretFile string, args []string, in io.Reader, out io.Writer, clk clock.Clock) error {
	u := NewUsers(UsersFile(secretFile), clk)
	usage := errors.New("usage: foreman users add NAME EMAIL [--desk PROJECT]... | passwd NAME | desk NAME [PROJECT]... | list | remove NAME (a password is read from stdin)")
	password := func() (string, error) {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("give the password on stdin")
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "list":
		for _, x := range u.List() {
			fmt.Fprintf(out, "%-16s %-32s desk: %s (added %s)\n", x.Name, x.Email, orEvery(x.Desk), x.Added)
		}
		return nil
	case "remove":
		if len(args) != 2 {
			return usage
		}
		if err := u.Remove(args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "removed %s; their sessions end now\n", args[1])
		return nil
	case "passwd":
		if len(args) != 2 {
			return usage
		}
		p, err := password()
		if err != nil {
			return err
		}
		if err := u.SetPassword(args[1], p); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s has a new password; their sessions end now\n", args[1])
		return nil
	case "desk":
		if len(args) < 2 {
			return usage
		}
		if err := u.SetDesk(args[1], args[2:]); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s's desk: %s\n", args[1], orEvery(args[2:]))
		return nil
	case "add":
		if len(args) < 3 {
			return usage
		}
		var desk []string
		for i := 3; i < len(args); i++ {
			if args[i] == "--desk" && i+1 < len(args) {
				desk = append(desk, args[i+1])
				i++
			} else {
				return fmt.Errorf("unexpected %q", args[i])
			}
		}
		p, err := password()
		if err != nil {
			return err
		}
		if err := u.Add(args[1], args[2], p, desk); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s can log in as %s; desk: %s\n", args[1], normEmail(args[2]), orEvery(desk))
		return nil
	}
	return fmt.Errorf("unknown users command %q", args[0])
}

func orEvery(desk []string) string {
	if len(desk) == 0 {
		return "every project"
	}
	return strings.Join(cleanDesk(desk), ", ")
}
