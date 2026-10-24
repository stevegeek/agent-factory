package helper

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/fsx"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/webpush"
)

// manualClock is a clock a test moves by hand.
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// foremanFake is the master's /healthz: what it answers can change
// between runs. onGet runs as a request arrives; with stall set, the
// answer starts and then never ends.
type foremanFake struct {
	mu         sync.Mutex
	code       int
	body       string
	stall      bool
	onGet      func()
	hosts, sni []string
}

func (f *foremanFake) set(code int, body string) {
	f.mu.Lock()
	f.code, f.body = code, body
	f.mu.Unlock()
}

func (f *foremanFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	code, body, stall, onGet := f.code, f.body, f.stall, f.onGet
	f.hosts = append(f.hosts, r.Host)
	if r.TLS != nil {
		f.sni = append(f.sni, r.TLS.ServerName)
	}
	f.mu.Unlock()
	if onGet != nil {
		onGet()
	}
	if stall {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	}
	if r.URL.Path != "/healthz" {
		code, body = 404, "not found\n"
	}
	if code == http.StatusFound {
		w.Header().Set("Location", "/healthz?again")
	}
	w.WriteHeader(code)
	io.WriteString(w, body)
}

// pushFake is a push service: it records each request and answers code.
type pushFake struct {
	mu   sync.Mutex
	code int
	got  []pushed
}

type pushed struct {
	path, auth string
	body       []byte
}

func (p *pushFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	p.got = append(p.got, pushed{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: b})
	code := p.code
	p.mu.Unlock()
	w.WriteHeader(code)
}

func (p *pushFake) all() []pushed {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.got)
}

const (
	watchURL  = "https://foreman.example.ts.net/healthz"
	watchName = "foreman.example.ts.net"
	healthy   = "ok\nversion abc123def456\nstarted 2026-10-18T11:00:00Z\ntick-age 60\n"
)

// watchRig is a helper with a watchdog configured, the master's /healthz
// behind a certificate for watchName, a push service and the maintainer's
// device. Every dial is recorded; one to port 443 reaches the fake master.
type watchRig struct {
	*rig
	clock   *manualClock
	foreman *foremanFake
	push    *pushFake
	devices map[string]testkit.PushDevice // by endpoint path
	pushURL string
	dialled []string
	mu      sync.Mutex
}

// unixListener listens on a fresh unix socket; its directory is short,
// since a socket's path is at most about 100 bytes.
func unixListener(t *testing.T) net.Listener {
	t.Helper()
	dir, err := os.MkdirTemp("", "wd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// selfSigned is a certificate for name, and a pool that trusts it.
func selfSigned(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func newWatchRig(t *testing.T, certName string) *watchRig {
	t.Helper()
	w := &watchRig{rig: newRig(t), foreman: &foremanFake{code: 200, body: healthy}, push: &pushFake{code: 201},
		devices: map[string]testkit.PushDevice{}}
	w.clock = &manualClock{t: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
	w.Clock = w.clock
	w.Host = "box"
	// the certificate is trusted either way: only its name can fail
	cert, pool := selfSigned(t, certName)
	// both servers listen on unix sockets: a test's connections never meet
	// the host's network or firewall, and the kernel buffers them as TCP
	// would, so a refused handshake fails at once
	fs := httptest.NewUnstartedServer(w.foreman)
	fs.Listener.Close()
	fs.Listener = unixListener(t)
	fs.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	fs.Config.ErrorLog = log.New(io.Discard, "", 0) // the refused certificate's handshake
	fs.StartTLS()
	t.Cleanup(fs.Close)
	ps := httptest.NewUnstartedServer(w.push)
	ps.Listener.Close()
	ps.Listener = unixListener(t)
	ps.Start()
	t.Cleanup(ps.Close)
	w.pushURL = "http://push.example"
	w.HTTP = &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			w.mu.Lock()
			w.dialled = append(w.dialled, addr)
			w.mu.Unlock()
			var d net.Dialer
			switch {
			case addr == "push.example:80":
				return d.DialContext(ctx, "unix", ps.Listener.Addr().String())
			case strings.HasSuffix(addr, ":443"):
				return d.DialContext(ctx, "unix", fs.Listener.Addr().String())
			}
			return nil, errors.New("no route to " + addr)
		},
	}}
	t.Cleanup(w.HTTP.CloseIdleConnections) // before the servers close: they wait for open connections
	w.PushAllow = func(endpoint string) bool { return strings.HasPrefix(endpoint, w.pushURL+"/") }
	w.write(t, "config.toml", "[remote.box]\nwatchdog = \""+watchURL+"\"\nwatchdog_ip = \"100.64.0.1\"\n")
	// the copied key and the maintainer's device
	keys := filepath.Join(w.Home, ".config", "factory", "watchdog")
	os.MkdirAll(keys, 0o700)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalECPrivateKey(k)
	os.WriteFile(filepath.Join(keys, "foreman-vapid.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
	w.devicesFile(t, w.pushURL, map[string]string{"/mine": ""})
	return w
}

// devicesFile writes foreman-push.json: one device per path, for the user
// given ("" is the maintainer).
func (w *watchRig) devicesFile(t *testing.T, base string, users map[string]string) {
	t.Helper()
	var subs []webpush.Subscription
	var paths []string
	for p := range users {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	for _, p := range paths {
		d := testkit.NewPushDevice(t)
		w.devices[p] = d
		var s webpush.Subscription
		s.Endpoint, s.User = base+p, users[p]
		s.Keys.P256dh, s.Keys.Auth = d.P256dh, d.Auth
		subs = append(subs, s)
	}
	data, _ := json.Marshal(subs)
	if err := os.WriteFile(filepath.Join(w.Home, ".config", "factory", "watchdog", "foreman-push.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// run is one timer run: `factory-remote watchdog`, which exits 0 whatever
// the master's state.
func (w *watchRig) run(t *testing.T) {
	t.Helper()
	w.reset()
	w.push.mu.Lock()
	w.push.got = nil
	w.push.mu.Unlock()
	if code := w.argv(t, "watchdog"); code != 0 {
		t.Fatalf("watchdog: exit %d: %s", code, w.stderr)
	}
	if lines := strings.Count(w.stdout.String(), "\n"); lines != 1 {
		t.Errorf("want one log line, got %q", w.stdout)
	}
}

func (w *watchRig) state(t *testing.T) watchState {
	t.Helper()
	var st watchState
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(w.Root, "watchdog.json"))), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// message is what one push said, decrypted on its device.
func (w *watchRig) message(t *testing.T, p pushed) map[string]any {
	t.Helper()
	d, ok := w.devices[p.path]
	if !ok {
		t.Fatalf("a push to %s, no device of the test", p.path)
	}
	var m map[string]any
	if err := json.Unmarshal(d.Open(t, p.body), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestWatchdogHealthyStaysQuiet(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.run(t)
	w.clock.advance(5 * time.Minute)
	w.run(t)
	if got := w.push.all(); len(got) != 0 {
		t.Errorf("pushed %d times to a healthy master", len(got))
	}
	st := w.state(t)
	if st.Fails != 0 || st.LastOK != "2026-10-18T12:05:00+00:00" || st.DownSince != "" || st.LastAlert != "" {
		t.Errorf("state %+v", st)
	}
	if !strings.Contains(w.stdout.String(), "ok") {
		t.Errorf("log %q", w.stdout)
	}
}

func TestWatchdogAlertsAfterTwoFailures(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.foreman.set(502, "bad gateway\n")
	w.run(t)
	if got := w.push.all(); len(got) != 0 {
		t.Fatalf("pushed on the first failure")
	}
	if st := w.state(t); st.Fails != 1 || st.DownSince != "2026-10-18T12:00:00+00:00" || st.Reason != "HTTP 502" {
		t.Errorf("after one failure: %+v", st)
	}
	w.clock.advance(5 * time.Minute)
	w.run(t)
	got := w.push.all()
	if len(got) != 1 {
		t.Fatalf("pushed %d times after two failures", len(got))
	}
	m := w.message(t, got[0])
	if m["title"] != "Factory master down" || m["url"] != "/settings" || m["tag"] != "watchdog" ||
		!strings.HasPrefix(m["body"].(string), "HTTP 502 since 12:00 UTC.") || !strings.Contains(m["body"].(string), "does not take over") {
		t.Errorf("message %v", m)
	}
	if _, ok := m["badge"]; ok {
		t.Errorf("the watchdog touched the badge: %v", m)
	}
	// the VAPID contact is the watchdog URL's origin
	jwt := strings.TrimPrefix(strings.Split(got[0].auth, ",")[0], "vapid t=")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("authorization %q", got[0].auth)
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c map[string]any
	json.Unmarshal(claims, &c)
	if c["sub"] != "https://foreman.example.ts.net" {
		t.Errorf("claims %v", c)
	}
	if st := w.state(t); st.Fails != 2 || st.LastAlert != "2026-10-18T12:05:00+00:00" {
		t.Errorf("after the alert: %+v", st)
	}
	w.clock.advance(5 * time.Minute)
	w.run(t)
	if got := w.push.all(); len(got) != 0 {
		t.Errorf("pushed again 5 minutes later")
	}
}

func TestWatchdogRepeatsAfterSixHours(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.foreman.set(502, "")
	w.run(t)
	w.clock.advance(5 * time.Minute)
	w.run(t) // the first alert, at 12:05
	for range 71 {
		w.clock.advance(5 * time.Minute)
		w.run(t)
		if got := w.push.all(); len(got) != 0 {
			t.Fatalf("pushed again at %s", w.clock.Now().Format("15:04"))
		}
	}
	w.clock.advance(5 * time.Minute) // 18:05
	w.run(t)
	got := w.push.all()
	if len(got) != 1 {
		t.Fatalf("pushed %d times six hours on", len(got))
	}
	if m := w.message(t, got[0]); m["title"] != "Factory master down" || !strings.HasPrefix(m["body"].(string), "HTTP 502 since 12:00 UTC.") {
		t.Errorf("repeat %v", m)
	}
}

func TestWatchdogSaysWhenTheMasterIsBack(t *testing.T) {
	w := newWatchRig(t, watchName)
	// one failure and back: nothing was said, so nothing is
	w.foreman.set(502, "")
	w.run(t)
	w.foreman.set(200, healthy)
	w.clock.advance(5 * time.Minute)
	w.run(t)
	if got := w.push.all(); len(got) != 0 {
		t.Fatalf("a blip was pushed")
	}
	if st := w.state(t); st.Fails != 0 || st.DownSince != "" {
		t.Errorf("after a blip: %+v", st)
	}
	w.foreman.set(502, "")
	w.clock.advance(5 * time.Minute)
	w.run(t) // 12:10
	w.clock.advance(5 * time.Minute)
	w.run(t) // 12:15, the alert
	w.foreman.set(200, healthy)
	w.clock.advance(65 * time.Minute)
	w.run(t) // 13:20
	got := w.push.all()
	if len(got) != 1 {
		t.Fatalf("pushed %d times on recovery", len(got))
	}
	if m := w.message(t, got[0]); m["title"] != "Factory master is back (down 1h10m)" || m["tag"] != "watchdog" {
		t.Errorf("back %v", m)
	}
	if st := w.state(t); st != (watchState{LastOK: "2026-10-18T13:20:00+00:00"}) {
		t.Errorf("after recovery: %+v", st)
	}
	w.clock.advance(5 * time.Minute)
	w.run(t)
	if got := w.push.all(); len(got) != 0 {
		t.Errorf("said back twice")
	}
}

func TestWatchdogStaleTickIsDown(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.foreman.set(200, "ok\nversion abc\ntick-age 7200\n")
	w.run(t)
	if st := w.state(t); st.Fails != 0 {
		t.Errorf("a tick exactly 2h old is fine: %+v", st)
	}
	w.foreman.set(200, "ok\nversion abc\ntick-age 10800\n")
	w.run(t)
	w.clock.advance(5 * time.Minute)
	w.run(t)
	got := w.push.all()
	if len(got) != 1 {
		t.Fatalf("pushed %d times for a stale tick", len(got))
	}
	if m := w.message(t, got[0]); !strings.HasPrefix(m["body"].(string), "tick 3h old since 12:00 UTC.") {
		t.Errorf("stale %v", m)
	}
}

func TestWatchdogUnknownTickIsDown(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.foreman.set(200, "ok\nversion abc\ntick-age unknown\n")
	w.run(t)
	if st := w.state(t); st.Fails != 1 || st.Reason != "tick-age unknown" {
		t.Errorf("state %+v", st)
	}
	for _, c := range []struct {
		code       int
		body, want string
	}{
		{200, healthy, ""},
		{200, "ok\ntick-age 0\n", ""},
		{200, "ok\nversion abc\n", "tick-age unknown"},
		{200, "ok\ntick-age soon\n", "tick-age unknown"},
		{200, "ok\ntick-age -5\n", "tick-age unknown"},
		{200, "ok\ntick-age 7201\n", "tick 2h old"},
		{200, "ok\ntick-age 11000\n", "tick 3h03m old"},
		{200, "starting\ntick-age 1\n", "not ok"},
		{200, "", "not ok"},
		{502, "ok\ntick-age 1\n", "HTTP 502"},
		{302, "", "HTTP 302"},
		{0, "", "no answer"},
		// a body cut off at the 64 KiB read still has its first lines
		{200, "ok\ntick-age 60\n" + strings.Repeat("x", watchMaxBody), ""},
		{200, "ok\n" + strings.Repeat("x", watchMaxBody) + "\ntick-age 60\n", "tick-age unknown"},
	} {
		ok, reason := judge(c.code, c.body, watchStale)
		if ok != (c.want == "") || reason != c.want {
			t.Errorf("judge(%d, %q) = %v %q, want %q", c.code, c.body, ok, reason, c.want)
		}
	}
}

// The check dials watchdog_ip, not what the name resolves to, while TLS
// still checks the URL's host; without watchdog_ip it dials the name. A
// certificate for another name is no answer. A redirect is not followed.
func TestWatchdogDialsTheConfiguredAddress(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.run(t)
	if st := w.state(t); st.LastOK == "" {
		t.Fatalf("not ok: %+v (%s)", st, w.stdout)
	}
	w.mu.Lock()
	dialled := slices.Clone(w.dialled)
	w.mu.Unlock()
	if !slices.Equal(dialled, []string{"100.64.0.1:443"}) {
		t.Errorf("dialled %q", dialled)
	}
	w.foreman.mu.Lock()
	hosts, sni := slices.Clone(w.foreman.hosts), slices.Clone(w.foreman.sni)
	w.foreman.mu.Unlock()
	if !slices.Equal(hosts, []string{watchName}) || !slices.Equal(sni, []string{watchName}) {
		t.Errorf("host %q, server name %q", hosts, sni)
	}

	w.write(t, "config.toml", "[remote.box]\nwatchdog = \""+watchURL+"\"\n")
	w.mu.Lock()
	w.dialled = nil
	w.mu.Unlock()
	w.run(t)
	w.mu.Lock()
	dialled = slices.Clone(w.dialled)
	w.mu.Unlock()
	if !slices.Equal(dialled, []string{watchName + ":443"}) {
		t.Errorf("without watchdog_ip dialled %q", dialled)
	}

	w.foreman.set(http.StatusFound, "")
	w.run(t)
	if st := w.state(t); st.Reason != "HTTP 302" {
		t.Errorf("a redirect: %+v", st)
	}

	other := newWatchRig(t, "other.example.ts.net")
	other.run(t)
	if st := other.state(t); st.Fails != 1 || st.Reason != "no answer" {
		t.Errorf("a certificate for another name: %+v", st)
	}
}

func TestWatchdogSendsOnlyToTheMaintainer(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.devicesFile(t, w.pushURL, map[string]string{"/mine": "", "/also-mine": webpush.Maintainer, "/robins": "robin"})
	w.push.code = 500 // a failing device is logged, and the run still ends well
	w.foreman.set(502, "")
	w.run(t)
	w.clock.advance(5 * time.Minute)
	w.run(t)
	var paths []string
	for _, p := range w.push.all() {
		paths = append(paths, p.path)
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"/also-mine", "/mine"}) {
		t.Errorf("pushed to %q", paths)
	}
	if !strings.Contains(w.stderr.String(), "answered 500") {
		t.Errorf("the failed push was not logged: %q", w.stderr)
	}
}

// The watchdog is no ssh verb: over the dispatcher's key it is refused
// like any other unknown verb, and nothing is checked or written.
func TestWatchdogRefusedOverSSH(t *testing.T) {
	w := newWatchRig(t, watchName)
	if code := w.ssh(t, "watchdog"); code != 2 {
		t.Errorf("over ssh: exit %d", code)
	}
	w.Environ = append(w.Environ, "SSH_ORIGINAL_COMMAND=list")
	if code := w.argv(t, "watchdog"); code != 2 {
		t.Errorf("argv watchdog over ssh: exit %d", code)
	}
	if onDisk(filepath.Join(w.Root, "watchdog.json")) || len(w.dialled) != 0 {
		t.Errorf("an ssh call checked the master")
	}
}

func TestWatchdogWithoutKeysFails(t *testing.T) {
	w := newWatchRig(t, watchName)
	keys := filepath.Join(w.Home, ".config", "factory", "watchdog")
	os.RemoveAll(keys)
	if code := w.argv(t, "watchdog"); code != 1 ||
		!strings.Contains(w.stderr.String(), "watchdog: no push keys in ~/.config/factory/watchdog (copy them from the master)") {
		t.Errorf("no keys: exit %d: %s", code, w.stderr)
	}
	if onDisk(keys) {
		t.Errorf("the watchdog made the key directory")
	}
	// a key but no devices
	w = newWatchRig(t, watchName)
	os.Remove(filepath.Join(w.Home, ".config", "factory", "watchdog", "foreman-push.json"))
	w.reset()
	if code := w.argv(t, "watchdog"); code != 1 || !strings.Contains(w.stderr.String(), "no push keys") {
		t.Errorf("no devices: exit %d: %s", code, w.stderr)
	}
	// devices but no key: none is made
	w = newWatchRig(t, watchName)
	key := filepath.Join(w.Home, ".config", "factory", "watchdog", "foreman-vapid.pem")
	os.Remove(key)
	w.reset()
	if code := w.argv(t, "watchdog"); code != 1 || !strings.Contains(w.stderr.String(), "no push keys") {
		t.Errorf("no key: exit %d: %s", code, w.stderr)
	}
	if onDisk(key) {
		t.Errorf("the watchdog made a key")
	}
	if onDisk(filepath.Join(w.Root, "watchdog.json")) || len(w.dialled) != 0 {
		t.Errorf("checked the master with no way to tell anyone")
	}
	// no watchdog in config
	w.write(t, "config.toml", "[remote]\nmemory = \"1g\"\n")
	w.reset()
	if code := w.argv(t, "watchdog"); code != 2 || !strings.Contains(w.stderr.String(), "no watchdog configured here") {
		t.Errorf("no watchdog: exit %d: %s", code, w.stderr)
	}
	// a URL that is not the master's /healthz over https
	for _, bad := range []string{"http://foreman.example.ts.net/healthz", "https://foreman.example.ts.net/", "https://foreman.example.ts.net/healthz?x"} {
		w.write(t, "config.toml", "[remote.box]\nwatchdog = \""+bad+"\"\n")
		w.reset()
		if code := w.argv(t, "watchdog"); code != 2 || !strings.Contains(w.stderr.String(), "whose path is /healthz") {
			t.Errorf("%s: exit %d: %s", bad, code, w.stderr)
		}
	}
	w.write(t, "config.toml", "[remote.box]\nwatchdog = \""+watchURL+"\"\nwatchdog_ip = \"foreman\"\n")
	w.reset()
	if code := w.argv(t, "watchdog"); code != 2 || !strings.Contains(w.stderr.String(), "it is an IP address") {
		t.Errorf("a name for watchdog_ip: exit %d: %s", code, w.stderr)
	}
	if onDisk(filepath.Join(w.Root, "watchdog.json")) || len(w.dialled) != 0 {
		t.Errorf("checked the master with a bad config")
	}
}

// The copied key and devices are only ever read.
func TestWatchdogNeverWritesTheKeys(t *testing.T) {
	w := newWatchRig(t, watchName)
	dir := filepath.Join(w.Home, ".config", "factory", "watchdog")
	before := map[string]string{}
	for _, f := range []string{"foreman-vapid.pem", "foreman-push.json"} {
		before[f] = readFile(t, filepath.Join(dir, f))
	}
	w.push.code = 410 // a gone device is not forgotten here
	w.foreman.set(502, "")
	w.run(t)
	w.clock.advance(5 * time.Minute)
	w.run(t)
	for f, b := range before {
		if readFile(t, filepath.Join(dir, f)) != b {
			t.Errorf("%s changed", f)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("files in the key directory: %v", entries)
	}
}

// An alert no device took was not an alert: the next run tries again, and
// a master back before anyone heard gets no "back".
func TestWatchdogAlertCountsOnlyWhenADeviceTookIt(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.push.code = 500
	w.foreman.set(502, "")
	w.run(t)
	w.clock.advance(5 * time.Minute)
	w.run(t) // the alert, refused
	if got := w.push.all(); len(got) != 1 {
		t.Fatalf("pushed %d times", len(got))
	}
	if st := w.state(t); st.LastAlert != "" || st.Told || st.Fails != 2 {
		t.Errorf("after a refused alert: %+v", st)
	}
	w.push.code = 201
	w.clock.advance(5 * time.Minute)
	w.run(t) // 12:10: tried again, and taken
	got := w.push.all()
	if len(got) != 1 {
		t.Fatalf("no retry: pushed %d times", len(got))
	}
	if m := w.message(t, got[0]); m["title"] != "Factory master down" || !strings.HasPrefix(m["body"].(string), "HTTP 502 since 12:00 UTC.") {
		t.Errorf("retry %v", m)
	}
	if st := w.state(t); st.LastAlert != "2026-10-18T12:10:00+00:00" || !st.Told {
		t.Errorf("after the alert: %+v", st)
	}
	w.foreman.set(200, healthy)
	w.clock.advance(5 * time.Minute)
	w.run(t)
	if got := w.push.all(); len(got) != 1 || w.message(t, got[0])["title"] != "Factory master is back (down 15m)" {
		t.Errorf("back: %d pushes", len(got))
	}

	// the helper's own network was down: every alert failed, so the master
	// being fine says nothing
	w = newWatchRig(t, watchName)
	w.push.code = 500
	w.foreman.set(502, "")
	for range 3 {
		w.run(t)
		w.clock.advance(5 * time.Minute)
	}
	w.push.code = 201
	w.foreman.set(200, healthy)
	w.run(t)
	if got := w.push.all(); len(got) != 0 {
		t.Errorf("said back for an outage nobody heard of: %d pushes", len(got))
	}
	if st := w.state(t); st != (watchState{LastOK: "2026-10-18T12:15:00+00:00"}) {
		t.Errorf("state %+v", st)
	}
}

// Failures further apart than watchGap are not in a row: the helper was
// off, or the timer stopped, in between.
func TestWatchdogFailuresFarApartAreNotInARow(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.foreman.set(502, "")
	w.run(t) // 12:00
	w.clock.advance(20 * time.Minute)
	w.run(t) // 12:20
	if got := w.push.all(); len(got) != 0 {
		t.Fatalf("pushed for two failures 20 minutes apart")
	}
	if st := w.state(t); st.Fails != 1 || st.DownSince != "2026-10-18T12:20:00+00:00" || st.LastFail != st.DownSince {
		t.Errorf("after the gap: %+v", st)
	}
	w.clock.advance(watchGap)
	w.run(t) // 12:35: in a row, just
	if got := w.push.all(); len(got) != 1 {
		t.Fatalf("pushed %d times for failures %s apart", len(got), watchGap)
	}
	if m := w.message(t, w.push.all()[0]); !strings.HasPrefix(m["body"].(string), "HTTP 502 since 12:20 UTC.") {
		t.Errorf("message %v", m)
	}

	// an outage the maintainer was told of goes on over a gap: the count
	// starts again, but its start and the "back" stay
	w.clock.advance(time.Hour)
	w.run(t) // 13:35
	if got := w.push.all(); len(got) != 0 {
		t.Errorf("pushed after the gap")
	}
	if st := w.state(t); st.Fails != 1 || st.DownSince != "2026-10-18T12:20:00+00:00" || !st.Told || st.LastAlert != "2026-10-18T12:35:00+00:00" {
		t.Errorf("a told outage after a gap: %+v", st)
	}
	w.foreman.set(200, healthy)
	w.clock.advance(5 * time.Minute)
	w.run(t)
	if got := w.push.all(); len(got) != 1 || w.message(t, got[0])["title"] != "Factory master is back (down 1h20m)" {
		t.Errorf("back: %d pushes", len(got))
	}
}

// A check cut short by the run's own end (the timer stopping it) is no
// failure: nothing is counted or written.
func TestWatchdogCutShortCountsNothing(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.foreman.set(502, "")
	w.run(t)
	before := readFile(t, filepath.Join(w.Root, "watchdog.json"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w.foreman.mu.Lock()
	w.foreman.stall, w.foreman.onGet = true, cancel
	w.foreman.mu.Unlock()
	w.clock.advance(5 * time.Minute)
	w.reset()
	if code := w.Serve(ctx, []string{w.Self, "watchdog"}); code == 0 || !strings.Contains(w.stderr.String(), "cut short") {
		t.Errorf("cut short: exit %d: %s", code, w.stderr)
	}
	if got := w.push.all(); len(got) != 0 {
		t.Errorf("pushed %d times", len(got))
	}
	if after := readFile(t, filepath.Join(w.Root, "watchdog.json")); after != before {
		t.Errorf("state changed: %s", after)
	}
}

// An answer that starts and stalls is no answer, within the timeout.
func TestWatchdogStalledAnswerIsNoAnswer(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.WatchTimeout = 300 * time.Millisecond
	w.foreman.mu.Lock()
	w.foreman.stall = true
	w.foreman.mu.Unlock()
	start := time.Now()
	w.run(t)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s", d)
	}
	if st := w.state(t); st.Fails != 1 || st.Reason != "no answer" {
		t.Errorf("state %+v", st)
	}
}

// An answer over 64 KiB is read in part, not refused.
func TestWatchdogLongAnswerIsReadInPart(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.foreman.set(200, healthy+strings.Repeat("padding\n", 4*watchMaxBody/8))
	w.run(t)
	if st := w.state(t); st.LastOK == "" || st.Fails != 0 {
		t.Errorf("state %+v (%s)", st, w.stdout)
	}
}

// A watchdog.json that does not parse is a fresh start, and says so.
func TestWatchdogCorruptStateStartsAfresh(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.write(t, "watchdog.json", `{"fails": 7, "down_since":`)
	w.foreman.set(502, "")
	w.run(t)
	if !strings.Contains(w.stderr.String(), "starting afresh") {
		t.Errorf("not logged: %q", w.stderr)
	}
	if got := w.push.all(); len(got) != 0 {
		t.Errorf("pushed %d times", len(got))
	}
	if st := w.state(t); st.Fails != 1 || st.DownSince != "2026-10-18T12:00:00+00:00" {
		t.Errorf("state %+v", st)
	}
}

// While another run holds the lock, a run checks nothing and exits 0.
func TestWatchdogOneRunAtATime(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.write(t, "watchdog.json", `{"fails": 1, "down_since": "2026-10-18T11:55:00+00:00", "last_fail": "2026-10-18T11:55:00+00:00"}`)
	if err := os.MkdirAll(w.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	held, err := fsx.TryLock(filepath.Join(w.Root, "watchdog.lock"))
	if err != nil {
		t.Fatal(err)
	}
	w.foreman.set(502, "")
	w.run(t)
	if !strings.Contains(w.stdout.String(), "another run is checking") || len(w.push.all()) != 0 || len(w.dialled) != 0 {
		t.Errorf("ran beside another: %q", w.stdout)
	}
	if st := w.state(t); st.Fails != 1 {
		t.Errorf("state %+v", st)
	}
	held.Unlock()
	w.run(t)
	if st := w.state(t); st.Fails != 2 || len(w.push.all()) != 1 {
		t.Errorf("after the lock was let go: %+v", st)
	}
}

// An outage that began on another day says which.
func TestWatchdogSinceGivesTheDayWhenNotToday(t *testing.T) {
	w := newWatchRig(t, watchName)
	w.write(t, "watchdog.json", `{"fails": 300, "down_since": "2026-10-17T12:00:00+00:00", "last_fail": "2026-10-18T11:55:00+00:00",
 "last_alert": "2026-10-18T06:00:00+00:00", "told": true}`)
	w.foreman.set(502, "")
	w.run(t)
	got := w.push.all()
	if len(got) != 1 {
		t.Fatalf("pushed %d times", len(got))
	}
	if m := w.message(t, got[0]); !strings.HasPrefix(m["body"].(string), "HTTP 502 since Oct 17 12:00 UTC.") {
		t.Errorf("message %v", m)
	}
	for _, c := range []struct{ stamp, want string }{
		{"2026-10-18T00:00:00+00:00", "00:00"},
		{"2026-10-17T23:59:00+00:00", "Oct 17 23:59"},
		{"2025-10-18T12:00:00+00:00", "Oct 18 12:00"},
		{"garbled", "garbled"},
	} {
		if got := downSince(c.stamp, w.clock.Now()); got != c.want {
			t.Errorf("downSince(%s) = %q, want %q", c.stamp, got, c.want)
		}
	}
}
