package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Other people: each has their own login secret and sees only their projects' tickets. The maintainer
// (the main secret, or a trusted Tailscale login) sees everything. Users live in foreman-users.json beside
// the secret (0600); `foreman users add|list|remove` manages it, and removing a user ends their sessions.

const maintainer = "maintainer"

var userName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

type User struct {
	Name     string   `json:"name"`
	Secret   string   `json:"secret_sha256"`
	Projects []string `json:"projects"`
	Added    string   `json:"added"`
}

// Sees: whether this user may see a ticket of the project.
func (u *User) Sees(project string) bool {
	if u == nil {
		return true // the maintainer
	}
	for _, p := range u.Projects {
		if p == project {
			return true
		}
	}
	return false
}

type Users struct {
	path string
	mu   sync.Mutex
	mod  time.Time
	list []User
}

func NewUsers(path string) *Users { return &Users{path: path} }

func (s *Users) load() []User {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := os.Stat(s.path)
	if err != nil {
		s.list, s.mod = nil, time.Time{}
		return nil
	}
	if !info.ModTime().Equal(s.mod) {
		var f struct{ Users []User }
		data, _ := os.ReadFile(s.path)
		if json.Unmarshal(data, &f) == nil {
			s.list = f.Users
		}
		s.mod = info.ModTime()
	}
	return s.list
}

func (s *Users) Get(name string) *User {
	for _, u := range s.load() {
		if u.Name == name {
			u := u
			return &u
		}
	}
	return nil
}

// Match: the user whose secret this is, checking every user (constant time each).
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
	data, _ := json.MarshalIndent(struct {
		Users []User `json:"users"`
	}{list}, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Add makes a user and returns their secret (shown once; only its hash is kept).
func (s *Users) Add(name string, projects []string) (string, error) {
	if !userName.MatchString(name) || name == maintainer {
		return "", errors.New("a user name is lowercase letters, digits and dashes, and not \"maintainer\"")
	}
	if len(projects) == 0 {
		return "", errors.New("name at least one project they may see")
	}
	if s.Get(name) != nil {
		return "", fmt.Errorf("%s already exists; remove them first to give a new secret", name)
	}
	raw := make([]byte, 24)
	rand.Read(raw)
	secret := base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(secret))
	list := append(s.load(), User{Name: name, Secret: hex.EncodeToString(sum[:]), Projects: projects,
		Added: time.Now().UTC().Format(time.RFC3339)})
	return secret, s.save(list)
}

func (s *Users) Remove(name string) error {
	var keep []User
	for _, u := range s.load() {
		if u.Name != name {
			keep = append(keep, u)
		}
	}
	if len(keep) == len(s.load()) {
		return fmt.Errorf("no user %s", name)
	}
	return s.save(keep)
}

// usersCommand: `foreman users add NAME --project P [--project Q]`, `foreman users list`, `foreman users remove NAME`.
func usersCommand(secretFile string, args []string) error {
	u := NewUsers(filepath.Join(filepath.Dir(secretFile), "foreman-users.json"))
	if len(args) == 0 {
		return errors.New("usage: foreman users add NAME --project P... | list | remove NAME")
	}
	switch args[0] {
	case "list":
		list := u.load()
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		for _, x := range list {
			fmt.Printf("%-16s %s (added %s)\n", x.Name, strings.Join(x.Projects, ", "), x.Added)
		}
		return nil
	case "remove":
		if len(args) != 2 {
			return errors.New("usage: foreman users remove NAME")
		}
		if err := u.Remove(args[1]); err != nil {
			return err
		}
		fmt.Printf("removed %s; their sessions end now\n", args[1])
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
		fmt.Printf("%s can log in with this secret (shown once):\n%s\n", args[1], secret)
		return nil
	}
	return fmt.Errorf("unknown users command %q", args[0])
}
