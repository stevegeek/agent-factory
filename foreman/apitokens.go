package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// APITokens reads the dispatcher's token store (`factory tokens`, api-tokens.json). Foreman only checks a
// bearer token here and learns its name; what the name may do is the CLI's to decide on every call.
type APITokens struct {
	path   string
	now    func() time.Time
	mu     sync.Mutex
	mod    time.Time
	size   int64
	recs   []apiToken
	warned string // the last refusal logged, so a bad store is logged once, not per request
}

type apiToken struct {
	Name    string  `json:"name"`
	SHA256  string  `json:"sha256"`
	Expires *string `json:"expires"`
}

func NewAPITokens(path string) *APITokens { return &APITokens{path: path, now: time.Now} }

// load re-reads the store when its mtime or size changes. A store others can read, or one in a directory
// others can write, holds no tokens: Foreman never writes the store, so it refuses it rather than tightening it.
func (s *APITokens) load() []apiToken {
	info, err := os.Stat(s.path)
	if err != nil {
		s.recs, s.mod, s.size = nil, time.Time{}, 0
		return nil
	}
	// checked on every read: a chmod changes neither the mtime nor the size
	if err := storePerms(s.path, info); err != nil {
		s.refuse(err.Error())
		return nil
	}
	if info.ModTime().Equal(s.mod) && info.Size() == s.size {
		return s.recs
	}
	s.recs, s.mod, s.size = nil, info.ModTime(), info.Size()
	var f struct {
		Version int        `json:"version"`
		Tokens  []apiToken `json:"tokens"`
	}
	data, err := os.ReadFile(s.path)
	if err == nil {
		err = json.Unmarshal(data, &f)
	}
	if err == nil && f.Version != 1 {
		err = fmt.Errorf("version %d", f.Version)
	}
	if err != nil {
		s.refuse(fmt.Sprintf("%s: not a token store Foreman reads (%v); no API token works", s.path, err))
		return nil
	}
	s.warned = ""
	s.recs = f.Tokens
	return s.recs
}

func (s *APITokens) refuse(msg string) {
	if msg != s.warned {
		log.Print(msg)
		s.warned = msg
	}
}

func storePerms(path string, info os.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s is readable by others (%v); no API token works until it is 0600 and its tokens are re-made",
			path, info.Mode().Perm())
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if dir.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by others; no API token works while the store is kept there", filepath.Dir(path))
	}
	return nil
}

// Name: whose token this is, or "" when it matches none, or only a record past its expiry.
func (s *APITokens) Name(token string) string {
	found := ""
	s.match(token, func(r apiToken) {
		if !s.expired(r) {
			found = r.Name
		}
	})
	return found
}

// Stale: the token is one this store issued whose record has expired.
func (s *APITokens) Stale(token string) bool {
	stale := false
	s.match(token, func(r apiToken) {
		if s.expired(r) {
			stale = true
		}
	})
	return stale
}

func (s *APITokens) match(token string, each func(apiToken)) {
	if token == "" {
		return
	}
	sum := sha256.Sum256([]byte(token))
	s.mu.Lock()
	recs := s.load()
	s.mu.Unlock()
	for _, r := range recs {
		want, err := hex.DecodeString(r.SHA256)
		if err == nil && len(want) == len(sum) && subtle.ConstantTimeCompare(sum[:], want) == 1 {
			each(r)
		}
	}
}

// expired as the CLI reads it: a time that does not parse has expired.
func (s *APITokens) expired(r apiToken) bool {
	if r.Expires == nil || *r.Expires == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339, *r.Expires)
	return err != nil || !at.After(s.now())
}
