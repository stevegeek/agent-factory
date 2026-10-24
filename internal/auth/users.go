package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/fsx"
)

// Other people: each has their own login secret and sees only their
// projects' tickets. The maintainer (the main secret, or a trusted
// Tailscale login) sees everything. Users live in foreman-users.json beside
// the secret (0600); `foreman users add|list|remove` manages it, and
// removing a user ends their sessions.

var userName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`) // re: go

// User is another person's login.
type User struct {
	Name     string   `json:"name"`
	Secret   string   `json:"secret_sha256"`
	Projects []string `json:"projects"`
	Added    string   `json:"added"`
}

// Sees reports whether this user may see a ticket of the project; a nil
// user is the maintainer, who sees everything.
func (u *User) Sees(project string) bool {
	if u == nil {
		return true
	}
	for _, p := range u.Projects {
		if p == project {
			return true
		}
	}
	return false
}

// Users is the users file, re-read when it changes (a new inode, mtime or
// size: a save is a rename, so a change within one mtime tick still shows). It is safe for
// concurrent use.
type Users struct {
	path  string
	clock clock.Clock
	mu    sync.Mutex
	seen  os.FileInfo // the file as last read: re-read when it is another file, or its mtime or size changed
	list  []User
}

// NewUsers is the store at path (foreman-users.json beside the secret).
// clk stamps new users; nil is the system clock.
func NewUsers(path string, clk clock.Clock) *Users {
	if clk == nil {
		clk = clock.System{}
	}
	return &Users{path: path, clock: clk}
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
		var f struct{ Users []User }
		data, _ := os.ReadFile(s.path)
		if json.Unmarshal(data, &f) == nil {
			s.list = f.Users
		}
		s.seen = info
	}
	return append([]User(nil), s.list...)
}

// List is every user, by name.
func (s *Users) List() []User {
	list := s.load()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list
}

// Get is the user of that name, or nil.
func (s *Users) Get(name string) *User {
	for _, u := range s.load() {
		if u.Name == name {
			u := u
			return &u
		}
	}
	return nil
}

// Sees reports whether the user of that name still exists and may see the
// project (the push package's Audience).
func (s *Users) Sees(name, project string) bool {
	u := s.Get(name)
	return u != nil && u.Sees(project)
}

// Match is the user whose secret this is, checking every user in constant
// time each, or "".
func (s *Users) Match(given string) string {
	g := sha256.Sum256([]byte(strings.TrimSpace(given)))
	found := ""
	for _, u := range s.load() {
		want, err := hex.DecodeString(u.Secret)
		if err == nil && len(want) == len(g) && subtle.ConstantTimeCompare(g[:], want) == 1 {
			found = u.Name
		}
	}
	return found
}

func (s *Users) save(list []User) error {
	data, err := json.MarshalIndent(struct {
		Users []User `json:"users"`
	}{list}, "", "  ")
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomic(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("save users: %w", err)
	}
	return nil
}

// Add makes a user and returns their secret: shown once, only its hash is
// kept.
func (s *Users) Add(name string, projects []string) (string, error) {
	if !userName.MatchString(name) || name == Maintainer {
		return "", errors.New("a user name is lowercase letters, digits and dashes, and not \"maintainer\"")
	}
	if len(projects) == 0 {
		return "", errors.New("name at least one project they may see")
	}
	if s.Get(name) != nil {
		return "", fmt.Errorf("%s already exists; remove them first to give a new secret", name)
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(secret))
	list := append(s.load(), User{Name: name, Secret: hex.EncodeToString(sum[:]), Projects: projects,
		Added: s.clock.Now().UTC().Format(time.RFC3339)})
	return secret, s.save(list)
}

// Remove deletes a user; their sessions end at once.
func (s *Users) Remove(name string) error {
	all := s.load()
	var keep []User
	for _, u := range all {
		if u.Name != name {
			keep = append(keep, u)
		}
	}
	if len(keep) == len(all) {
		return fmt.Errorf("no user %s", name)
	}
	return s.save(keep)
}

// UsersCommand is `foreman users add NAME --project P [--project Q]`,
// `foreman users list` and `foreman users remove NAME`, on the users file
// beside secretFile.
func UsersCommand(secretFile string, args []string, out io.Writer, clk clock.Clock) error {
	u := NewUsers(UsersFile(secretFile), clk)
	if len(args) == 0 {
		return errors.New("usage: foreman users add NAME --project P... | list | remove NAME")
	}
	switch args[0] {
	case "list":
		for _, x := range u.List() {
			fmt.Fprintf(out, "%-16s %s (added %s)\n", x.Name, strings.Join(x.Projects, ", "), x.Added)
		}
		return nil
	case "remove":
		if len(args) != 2 {
			return errors.New("usage: foreman users remove NAME")
		}
		if err := u.Remove(args[1]); err != nil {
			return err
		}
		fmt.Fprintf(out, "removed %s; their sessions end now\n", args[1])
		return nil
	case "add":
		if len(args) < 2 {
			return errors.New("usage: foreman users add NAME --project P...")
		}
		var projects []string
		for i := 2; i < len(args); i++ {
			if args[i] == "--project" && i+1 < len(args) {
				projects = append(projects, args[i+1])
				i++
			} else {
				return fmt.Errorf("unexpected %q", args[i])
			}
		}
		secret, err := u.Add(args[1], projects)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s can log in with this secret (shown once):\n%s\n", args[1], secret)
		return nil
	}
	return fmt.Errorf("unknown users command %q", args[0])
}
