package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/config"
	"github.com/stevegeek/agent-factory/internal/execx"
)

// Healthy is the deploy's check that commit12 runs: within
// HealthWait, Foreman's /healthz answers ok and names version commit12
// (the old Foreman answers ok until its restart, so ok alone proves
// nothing), and, when since is set, a start no earlier than since (to the
// second: a deploy of the running commit cannot tell the new Foreman by
// its version); and then bin/factory version starts with commit12. The
// address is [foreman] listen in the config.toml on disk now, so the check
// after a restore asks where the restored Foreman listens.
func (s Service) Healthy(ctx context.Context, commit12 string, since time.Time) error {
	if commit12 == "" {
		return errors.New("no commit to check: bin/BUILD named none")
	}
	cfg, err := config.Load(s.Paths.Config)
	if err != nil {
		return fmt.Errorf("health: %w", err)
	}
	url := "http://" + loopback(cfg.Foreman.Listen) + "/healthz"
	wait, every := s.HealthWait, s.HealthEvery
	if wait == 0 {
		wait = HealthWait
	}
	if every == 0 {
		every = HealthEvery
	}
	if err := s.awaitHealthz(ctx, url, cfg.Foreman.Listen, commit12, since, wait, every); err != nil {
		return err
	}
	// the version check has its own time: a Foreman up at the end of the
	// wait does not leave it an expired context
	res, err := s.exec().Run(ctx, execx.Cmd{Argv: []string{s.Paths.FactoryBin(), "version"}, Env: s.environ(), Timeout: VersionTimeout})
	if err == nil {
		err = execx.CheckErr(res)
	}
	if err != nil {
		return fmt.Errorf("bin/factory version: %w", err)
	}
	if !strings.HasPrefix(string(res.Stdout), commit12+" ") {
		return fmt.Errorf("bin/factory version is %q, not %s", strings.TrimSpace(string(res.Stdout)), commit12)
	}
	return nil
}

// awaitHealthz polls url every every until it names commit12 (started
// since, when set), for at most wait.
func (s Service) awaitHealthz(ctx context.Context, url, listen, commit12 string, since time.Time, wait, every time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	want := "version " + commit12
	if !since.IsZero() {
		want += " started after " + since.UTC().Format(time.RFC3339)
	}
	last := "no answer"
	for {
		body, err := s.get(ctx, url)
		if err == nil && healthOK(body, commit12, since) {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = "it answered " + strings.Join(strings.Fields(body), " ")
		}
		t := time.NewTimer(every)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("Foreman's /healthz on %s did not name %s within %v: %s", listen, want, wait, last)
		case <-t.C:
		}
	}
}

// healthOK is a /healthz body whose first line is ok, which names the
// version and, when since is set, a start no earlier than since's second.
func healthOK(body, commit12 string, since time.Time) bool {
	lines := strings.Split(body, "\n")
	if strings.TrimSpace(lines[0]) != "ok" {
		return false
	}
	version, started := false, since.IsZero()
	for _, l := range lines[1:] {
		l = strings.TrimSpace(l)
		if l == "version "+commit12 {
			version = true
		}
		if at, ok := strings.CutPrefix(l, "started "); ok && !started {
			t, err := time.Parse(time.RFC3339, at)
			started = err == nil && !t.Before(since.Truncate(time.Second))
		}
	}
	return version && started
}

func (s Service) get(ctx context.Context, url string) (string, error) {
	c := s.HTTP
	if c == nil {
		c = &http.Client{Timeout: 3 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return string(b), nil
}

// loopback is the address to reach a listen address on from this host,
// as deploy.sh's foreman_health maps it: a wildcard becomes loopback.
func loopback(listen string) string {
	switch {
	case strings.HasPrefix(listen, "0.0.0.0:"):
		return "127.0.0.1:" + strings.TrimPrefix(listen, "0.0.0.0:")
	case strings.HasPrefix(listen, "[::]:"):
		return "[::1]:" + strings.TrimPrefix(listen, "[::]:")
	}
	return listen
}
