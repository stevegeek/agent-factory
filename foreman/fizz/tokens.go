package fizz

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
)

// A token is shown once and kept only as its sha256. Each acts as one user in one account.
type Token struct {
	Name      string `json:"name"`
	Hash      string `json:"sha256"`
	Account   string `json:"account"`
	UserID    string `json:"user_id"`
	CreatedAt string `json:"created_at"`
}

var tokenName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (s *Store) tokensPath() string { return filepath.Join(s.Dir, "tokens.json") }

func (s *Store) Tokens() ([]Token, error) {
	var ts []Token
	err := readJSON(s.tokensPath(), &ts)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return ts, err
}

// AddToken makes a token named name acting as the user; it returns the secret, never stored.
func (s *Store) AddToken(name, acct, userID string) (string, error) {
	if !tokenName.MatchString(name) {
		return "", fmt.Errorf("token name: letters, digits, '.', '_' and '-' only: %q", name)
	}
	ts, err := s.Tokens()
	if err != nil {
		return "", err
	}
	for _, t := range ts {
		if t.Name == name {
			return "", fmt.Errorf("a token named %q exists; revoke it first", name)
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	secret := "foreman_" + hex.EncodeToString(raw)
	ts = append(ts, Token{Name: name, Hash: hashToken(secret), Account: acct, UserID: userID, CreatedAt: s.Stamp()})
	return secret, writeFile(s.tokensPath(), ts)
}

func (s *Store) RevokeToken(name string) error {
	ts, err := s.Tokens()
	if err != nil {
		return err
	}
	for i, t := range ts {
		if t.Name == name {
			return writeFile(s.tokensPath(), append(ts[:i:i], ts[i+1:]...))
		}
	}
	return fmt.Errorf("no token named %q", name)
}

// CheckToken finds the token for a presented secret. Every stored hash is compared, in
// constant time, so the time taken says nothing about which (if any) matched.
func (s *Store) CheckToken(secret string) (*Token, error) {
	ts, err := s.Tokens()
	if err != nil || secret == "" {
		return nil, err
	}
	want, _ := hex.DecodeString(hashToken(secret))
	var found *Token
	for i := range ts {
		got, err := hex.DecodeString(ts[i].Hash)
		if err != nil || len(got) != len(want) {
			continue
		}
		if subtle.ConstantTimeCompare(got, want) == 1 {
			found = &ts[i]
		}
	}
	return found, nil
}

func hashToken(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}
