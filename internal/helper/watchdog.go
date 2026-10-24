package helper

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/stevegeek/agent-factory/internal/clock"
	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/webpush"
)

// What the watchdog counts as down, and how often it says so.
const (
	watchFails   = 2                // failures in a row before the first push: a deploy's restart is one
	watchGap     = 15 * time.Minute // a failure older than this is not "in a row": two timer runs and their jitter
	watchRepeat  = 6 * time.Hour    // between pushes while the master stays down
	watchStale   = 2 * time.Hour    // the oldest tick that is still fine: the reboot window drains for 90 minutes
	watchTimeout = 10 * time.Second // for the whole /healthz request
	watchMaxBody = 64 << 10         // of /healthz read
)

// watchState is <Root>/watchdog.json: the current outage, if any, and the
// last good check. Times are clock.Stamp. LastAlert is the last alert a
// device took, and Told says one did in this outage, so only then is
// there a "back" to say.
type watchState struct {
	Fails     int    `json:"fails"`
	DownSince string `json:"down_since,omitempty"`
	LastFail  string `json:"last_fail,omitempty"`
	LastAlert string `json:"last_alert,omitempty"`
	Told      bool   `json:"told,omitempty"`
	LastOK    string `json:"last_ok,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// watchMessage is a push as Foreman's service worker reads it. It carries
// no badge, so the desk count on the device stays as Foreman left it.
type watchMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
}

func (s *Server) watchKeys() string { return filepath.Join(s.Home, ".config", "factory", "watchdog") }
func (s *Server) watchFile() string { return filepath.Join(s.Root, "watchdog.json") }
func (s *Server) watchLock() string { return filepath.Join(s.Root, "watchdog.lock") }

func (s *Server) watchTimeout() time.Duration { return cmp.Or(s.WatchTimeout, watchTimeout) }

// watchdog is `factory-remote watchdog`, a helper's 5-minute timer: one
// check of the master's /healthz ([remote.<host>] watchdog, dialled at
// watchdog_ip when set). After watchFails failures in a row it pushes to
// the maintainer's devices, again every watchRepeat while the master stays
// down, and once more when it is back. Failures further apart than
// watchGap start the count again, and a check cut short (the timer
// stopped mid-request) counts for nothing. One run at a time: another
// finds the lock held and leaves it be. It only ever notifies: from here a
// dead master and a network split look the same, and taking over could
// make two masters. The push key and devices are a copy of Foreman's,
// read and never written. Whatever the master's state, a run that could
// check and tell exits 0.
func (s *Server) watchdog(ctx context.Context) error {
	cfg, err := s.config()
	if err != nil {
		return err
	}
	target, ip := cfg.Remote.Watchdog, cfg.Remote.WatchdogIP
	if target == "" {
		return refuse("no watchdog configured here")
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "/healthz" || u.RawQuery != "" || u.Fragment != "" {
		return refuse("watchdog = %q: it is an https:// URL whose path is /healthz", target)
	}
	if ip != "" {
		if _, err := netip.ParseAddr(ip); err != nil {
			return refuse("watchdog_ip = %q: it is an IP address", ip)
		}
	}
	snd, devices, err := s.watchSender(u)
	if err != nil {
		return err
	}

	lock, err := fsx.TryLock(s.watchLock())
	if errors.Is(err, fsx.ErrBusy) {
		_, err := fmt.Fprintln(s.Stdout, "watchdog: another run is checking; this one leaves it be")
		return err
	}
	if err != nil {
		return fmt.Errorf("watchdog: %w", err)
	}
	defer lock.Unlock()

	code, body := s.watchGet(ctx, target, ip)
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("watchdog: check cut short, nothing counted: %w", err)
	}
	now := s.Clock.Now()
	ok, reason := judge(code, body, watchStale)
	st := s.loadWatchState()
	var line string
	switch {
	case ok:
		line = "watchdog: master ok"
		if down, err := time.Parse(clock.Layout, st.DownSince); err == nil && st.Told {
			msg := watchMessage{Title: "Factory master is back (down " + roughly(now.Sub(down)) + ")", URL: "/settings", Tag: "watchdog"}
			line += fmt.Sprintf("; back after %s, told %d of %d devices", roughly(now.Sub(down)), s.watchPush(ctx, snd, devices, msg), len(devices))
		}
		st = watchState{LastOK: clock.Stamp(now)}
	default:
		if prev, err := time.Parse(clock.Layout, st.LastFail); st.Fails > 0 && (err != nil || now.Sub(prev) > watchGap) {
			// the last failure is too long ago to be in a row with this one.
			// An outage the maintainer was told of goes on; one nobody
			// heard of starts over.
			st.Fails = 0
			if !st.Told {
				st = watchState{LastOK: st.LastOK}
			}
		}
		st.Fails++
		if st.DownSince == "" {
			st.DownSince = clock.Stamp(now)
		}
		st.LastFail = clock.Stamp(now)
		st.Reason = reason
		line = fmt.Sprintf("watchdog: master down (%s), %d in a row", reason, st.Fails)
		last, lerr := time.Parse(clock.Layout, st.LastAlert)
		if st.Fails >= watchFails && (st.LastAlert == "" || lerr != nil || now.Sub(last) >= watchRepeat) {
			msg := watchMessage{Title: "Factory master down", URL: "/settings", Tag: "watchdog",
				Body: reason + " since " + downSince(st.DownSince, now) + " UTC. The watchdog does not take over: fail over by hand."}
			sent := s.watchPush(ctx, snd, devices, msg)
			line += fmt.Sprintf("; told %d of %d devices", sent, len(devices))
			// only a push a device took counts: with none, the next run
			// tries again
			if sent > 0 {
				st.LastAlert, st.Told = clock.Stamp(now), true
			}
		}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomic(s.watchFile(), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("watchdog: %w", err)
	}
	_, err = fmt.Fprintln(s.Stdout, line)
	return err
}

// watchSender loads the copied VAPID key and the maintainer's devices. The
// key's VAPID contact is the watched URL's origin, as Foreman's is its
// first host.
func (s *Server) watchSender(u *url.URL) (webpush.Sender, []webpush.Subscription, error) {
	missing := fmt.Errorf("watchdog: no push keys in ~/.config/factory/watchdog (copy them from the master)")
	key, err := webpush.LoadVAPID(filepath.Join(s.watchKeys(), "foreman-vapid.pem"))
	if errors.Is(err, fs.ErrNotExist) {
		return webpush.Sender{}, nil, missing
	}
	if err != nil {
		return webpush.Sender{}, nil, fmt.Errorf("watchdog: %w", err)
	}
	subs, err := webpush.ReadSubscriptions(filepath.Join(s.watchKeys(), "foreman-push.json"))
	if err != nil {
		return webpush.Sender{}, nil, fmt.Errorf("watchdog: %w", err)
	}
	var devices []webpush.Subscription
	for _, d := range subs {
		if webpush.ForMaintainer(d) {
			devices = append(devices, d)
		}
	}
	if len(devices) == 0 {
		return webpush.Sender{}, nil, missing
	}
	return webpush.Sender{Key: key, Subject: u.Scheme + "://" + u.Host, HTTP: s.HTTP, Clock: s.Clock, AllowHost: s.PushAllow}, devices, nil
}

// watchPush sends msg to each device, at most once, and returns how many
// took it. A failure is logged; a gone device is not forgotten here (the
// list is Foreman's, copied by hand).
func (s *Server) watchPush(ctx context.Context, snd webpush.Sender, devices []webpush.Subscription, msg watchMessage) int {
	payload, err := json.Marshal(msg)
	if err != nil {
		return 0
	}
	sent := 0
	for _, d := range devices {
		code, err := snd.Send(ctx, d, payload)
		switch {
		case err != nil:
			fmt.Fprintf(s.Stderr, "watchdog: push: %v\n", err)
		case code == http.StatusNotFound || code == http.StatusGone:
			fmt.Fprintf(s.Stderr, "watchdog: push: a device on %s is gone (%d): copy the devices from the master again\n", webpush.HostOf(d.Endpoint), code)
		case code >= 300:
			fmt.Fprintf(s.Stderr, "watchdog: push: %s answered %d\n", webpush.HostOf(d.Endpoint), code)
		default:
			sent++
		}
	}
	return sent
}

// downSince is when an outage began, for the push: the time alone on the
// day it began, else with the date.
func downSince(stamp string, now time.Time) string {
	t, err := time.Parse(clock.Layout, stamp)
	if err != nil {
		return stamp
	}
	t, now = t.UTC(), now.UTC()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// loadWatchState reads watchdog.json; a missing or unreadable one is a
// fresh start.
func (s *Server) loadWatchState() watchState {
	var st watchState
	data, err := os.ReadFile(s.watchFile())
	if err != nil {
		return watchState{}
	}
	if err := json.Unmarshal(data, &st); err != nil {
		fmt.Fprintf(s.Stderr, "watchdog: %s: %v; starting afresh\n", s.watchFile(), err)
		return watchState{}
	}
	return st
}

// watchGet is one GET of target: no redirect followed, watchTimeout for
// all of it, at most watchMaxBody read. With ip set, every dial goes to
// ip (at the URL's port) while TLS still checks the URL's host. A request
// that got no response is code 0.
func (s *Server) watchGet(ctx context.Context, target, ip string) (code int, body string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, ""
	}
	resp, err := s.watchClient(ip).Do(req)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, watchMaxBody))
	if err != nil {
		return 0, ""
	}
	return resp.StatusCode, string(b)
}

func (s *Server) watchClient(ip string) *http.Client {
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var base http.RoundTripper
	if s.HTTP != nil {
		base = s.HTTP.Transport
	}
	if base == nil {
		base = http.DefaultTransport
	}
	tr, ok := base.(*http.Transport)
	if !ok {
		return &http.Client{Transport: base, Timeout: s.watchTimeout(), CheckRedirect: noRedirect}
	}
	tr = tr.Clone()
	tr.Proxy = nil
	tr.DisableKeepAlives = true
	if ip != "" {
		dial := tr.DialContext
		if dial == nil {
			dial = (&net.Dialer{Timeout: s.watchTimeout()}).DialContext
		}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return dial(ctx, network, net.JoinHostPort(ip, port))
		}
	}
	return &http.Client{Transport: tr, Timeout: s.watchTimeout(), CheckRedirect: noRedirect}
}

// judge reads one /healthz answer (code 0: none): up only for a 200 whose
// first line is ok and whose tick-age is at most stale. Otherwise it says
// why, briefly, for the push.
func judge(code int, body string, stale time.Duration) (ok bool, reason string) {
	switch {
	case code == 0:
		return false, "no answer"
	case code != http.StatusOK:
		return false, fmt.Sprintf("HTTP %d", code)
	}
	sc := bufio.NewScanner(strings.NewReader(body))
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "ok" {
		return false, "not ok"
	}
	age := time.Duration(-1)
	for sc.Scan() {
		if v, found := strings.CutPrefix(sc.Text(), "tick-age "); found {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 && n < int64(1<<62)/int64(time.Second) {
				age = time.Duration(n) * time.Second
			}
			break
		}
	}
	switch {
	case age < 0:
		return false, "tick-age unknown"
	case age > stale:
		return false, "tick " + roughly(age) + " old"
	}
	return true, ""
}

// roughly is a duration in minutes: "45m", "3h", "1h10m".
func roughly(d time.Duration) string {
	m := int64(d / time.Minute)
	switch {
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m%60 == 0:
		return fmt.Sprintf("%dh", m/60)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}
