// Package auth is Foreman's login (ARCHITECTURE.md §2.3, inventory-foreman
// §8): the shared secret, stateless signed sessions with a revocation file,
// the login throttle that slows guessing without ever locking the
// maintainer out, CSRF tokens derived from the session, the same-origin and
// Host checks, safe post-login redirects, the trusted Tailscale header and
// other people's logins (users).
//
// Login sessions never open the bearer-token APIs and the other way round:
// this package knows only the browser's cookie.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Logger is where auth reports what a maintainer should know (a tightened
// secret, a guessing alarm). It may be nil.
type Logger interface {
	Printf(format string, args ...any)
}

func logf(l Logger, format string, args ...any) {
	if l != nil {
		l.Printf(format, args...)
	}
}

// LoadSecret reads the shared login secret, creating one (32 random bytes
// as hex, mode 0600, its directory 0700) when there is none. A secret
// others can read is tightened to 0600 and logged; one in a directory
// others can write is refused; one shorter than 16 characters is refused.
func LoadSecret(path string, log Logger) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err = newSecret(path); err != nil {
			return nil, fmt.Errorf("make a login secret: %w", err)
		}
		logf(log, "wrote a new login secret to %s", path)
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read the login secret: %w", err)
	}
	if err := checkPerms(path, log); err != nil {
		return nil, err
	}
	s := []byte(strings.TrimSpace(string(data)))
	if len(s) < 16 {
		return nil, fmt.Errorf("%s: the secret is shorter than 16 characters", path)
	}
	return s, nil
}

// checkPerms: a secret others can read is tightened; one in a directory
// others can write is refused.
func checkPerms(path string, log Logger) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("check the login secret: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, 0o600); err != nil {
			return fmt.Errorf("%s is readable by others and could not be made 0600: %w", path, err)
		}
		logf(log, "%s was readable by others; it is now 0600. Rotate it if anyone else may have read it", path)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("check the login secret's directory: %w", err)
	}
	if dir.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by others; keep the secret in a directory only you can write", filepath.Dir(path))
	}
	return nil
}

func newSecret(path string) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(hex.EncodeToString(raw) + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// SinceFile is the revocation file beside the secret: it holds a unix time,
// and sessions issued before it are refused.
func SinceFile(secretFile string) string { return secretFile + ".since" }

// RevokeSessions ends every session issued until now, on every device
// (foreman --revoke-sessions).
func RevokeSessions(secretFile string, now time.Time) error {
	return writeSince(SinceFile(secretFile), now)
}

func writeSince(p string, now time.Time) error {
	if err := os.WriteFile(p, []byte(strconv.FormatInt(now.Unix()+1, 10)+"\n"), 0o600); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	return nil
}
