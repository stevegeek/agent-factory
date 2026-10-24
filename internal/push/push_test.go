package push

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stevegeek/agent-factory/internal/boardread"
	"github.com/stevegeek/agent-factory/internal/model"
	"github.com/stevegeek/agent-factory/internal/paths"
	"github.com/stevegeek/agent-factory/internal/testkit"
	"github.com/stevegeek/agent-factory/internal/ticket"
)

func TestMain(m *testing.M) {
	time.Local = time.UTC
	os.Exit(m.Run())
}

const legacyBoard = "../ticket/testdata/legacy/board"

var testNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func fixtures(t *testing.T) []*ticket.Ticket {
	t.Helper()
	names, _ := filepath.Glob(filepath.Join(legacyBoard, "T*.json"))
	var out []*ticket.Ticket
	for _, n := range names {
		data, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		tk, _, err := ticket.Decode(data)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.Less(out[j].ID) })
	if len(out) != 12 {
		t.Fatalf("loaded %d fixture tickets", len(out))
	}
	return out
}

func find(ts []*ticket.Ticket, id string) *ticket.Ticket {
	for _, t := range ts {
		if string(t.ID) == id {
			return t
		}
	}
	return nil
}

// cp is a ticket copy a test may change.
func cp(t *ticket.Ticket) *ticket.Ticket { return ticket.Clone(t) }

func newPusher(t *testing.T, dir string, aud Audience) *Pusher {
	t.Helper()
	p, err := New(dir, "https://foreman.example", Options{Audience: aud, Clock: testkit.Fixed{T: testNow}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// a device: its key pair and auth secret, as a browser would make them
func device(t *testing.T) (Subscription, *ecdh.PrivateKey, []byte) {
	t.Helper()
	priv, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	var s Subscription
	s.Endpoint = "https://web.push.apple.com/QGd1Zm9vYmFy"
	s.Keys.P256dh = b64().EncodeToString(priv.PublicKey().Bytes())
	s.Keys.Auth = b64().EncodeToString(auth)
	return s, priv, auth
}

func TestEncryptionDecryptsOnTheDevice(t *testing.T) {
	s, priv, auth := device(t)
	msg := []byte(`{"title":"Review: Review and merge PR #23?"}`)
	body, err := encrypt(s, msg)
	if err != nil {
		t.Fatal(err)
	}
	// the receiving side of RFC 8291
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	asPub := body[21 : 21+idlen]
	if rs != 4096 || idlen != 65 {
		t.Fatalf("header rs=%d idlen=%d", rs, idlen)
	}
	pub, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, _ := priv.ECDH(pub)
	cek, nonce, err := deriveKeys(shared, auth, salt, priv.PublicKey().Bytes(), asPub)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		t.Fatalf("the device cannot decrypt: %v", err)
	}
	if string(plain[:len(plain)-1]) != string(msg) || plain[len(plain)-1] != 0x02 {
		t.Errorf("plaintext %q", plain)
	}
}

func TestVAPIDSignsForThePushServiceOrigin(t *testing.T) {
	dir := t.TempDir()
	p := newPusher(t, dir, nil)
	jwt, err := p.vapid("https://web.push.apple.com/abc")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	claims, _ := b64().DecodeString(parts[1])
	var c map[string]any
	json.Unmarshal(claims, &c)
	if c["aud"] != "https://web.push.apple.com" || c["sub"] != "https://foreman.example" {
		t.Errorf("claims %v", c)
	}
	if exp, _ := c["exp"].(float64); int64(exp) != testNow.Add(12*time.Hour).Unix() {
		t.Errorf("exp %v", c["exp"])
	}
	sig, _ := b64().DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&p.key.PublicKey, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Errorf("signature does not verify")
	}
	again := newPusher(t, dir, nil)
	if again.PublicKey() != p.PublicKey() {
		t.Errorf("the key must persist")
	}
	if info, _ := os.Stat(filepath.Join(dir, "foreman-vapid.pem")); info.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", info.Mode())
	}
}

func TestOnlyPushServicesAreAccepted(t *testing.T) {
	for u, ok := range map[string]bool{
		"https://web.push.apple.com/abc":            true,
		"https://fcm.googleapis.com/fcm/send/x":     true,
		"https://updates.push.services.mozilla.com": true,
		"https://wns2-par02p.notify.windows.com/w":  true,
		"http://web.push.apple.com/abc":             false,
		"https://evil.example/web.push.apple.com":   false,
		"https://push.apple.com.evil.example/x":     false,
		"https://web.push.apple.com:8443/x":         false,
		"https://web.push.apple.com:443/x":          true,
		"https://user@web.push.apple.com/x":         false,
		"https://127.0.0.1/x":                       false,
		"https://fcm.googleapis.com.evil/x":         false,
		"::not a url":                               false,
	} {
		if HostOK(u) != ok {
			t.Errorf("%s: want %v", u, ok)
		}
	}
	p := newPusher(t, t.TempDir(), nil)
	s, _, _ := device(t)
	s.Endpoint = "https://169.254.169.254/latest"
	if p.Add(s) == nil {
		t.Errorf("a non-push endpoint was stored")
	}
	s, _, _ = device(t)
	s.Keys.Auth = "short"
	if p.Add(s) == nil {
		t.Errorf("a bad auth secret was stored")
	}
	s, _, _ = device(t)
	s.Keys.P256dh = b64().EncodeToString([]byte("x"))
	if p.Add(s) == nil {
		t.Errorf("a bad key was stored")
	}
}

type fakePush struct {
	code int
	got  []*http.Request
}

func (f *fakePush) RoundTrip(r *http.Request) (*http.Response, error) {
	f.got = append(f.got, r)
	return &http.Response{StatusCode: f.code, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func TestSendSignsEncryptsAndForgetsGoneDevices(t *testing.T) {
	p := newPusher(t, t.TempDir(), nil)
	s, _, _ := device(t)
	if err := p.Add(s); err != nil {
		t.Fatal(err)
	}
	fake := &fakePush{code: 201}
	p.client.Transport = fake
	if n := p.Send(context.Background(), Message{Title: "hi", URL: "/"}); n != 1 {
		t.Fatalf("sent %d", n)
	}
	r := fake.got[0]
	if r.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") ||
		!strings.Contains(r.Header.Get("Authorization"), ", k="+p.PublicKey()) || r.Header.Get("TTL") != "86400" ||
		r.Header.Get("Urgency") != "normal" || r.Method != "POST" || r.URL.String() != s.Endpoint {
		t.Errorf("request %s %s %v", r.Method, r.URL, r.Header)
	}
	fake.code = 500
	if n := p.Send(context.Background(), Message{Title: "x"}); n != 0 || p.Count() != 1 {
		t.Errorf("a failing service keeps the device: sent %d, %d devices", n, p.Count())
	}
	fake.code = 410
	p.Send(context.Background(), Message{Title: "again"})
	if p.Count() != 0 {
		t.Errorf("a gone device should be forgotten")
	}
	data, _ := os.ReadFile(filepath.Join(p.dir, "foreman-push.json"))
	if strings.Contains(string(data), "web.push.apple.com") {
		t.Errorf("the gone device is still saved: %s", data)
	}
}

func TestSubscriptionsPersistAndAreCapped(t *testing.T) {
	dir := t.TempDir()
	p := newPusher(t, dir, nil)
	s, _, _ := device(t)
	if err := p.Add(s); err != nil {
		t.Fatal(err)
	}
	s.User = "robin"
	if err := p.Add(s); err != nil || p.Count() != 1 {
		t.Fatalf("re-subscribing replaces: %v %d", err, p.Count())
	}
	for i := 1; i < maxDevices; i++ {
		d, _, _ := device(t)
		d.Endpoint += string(rune('a' + i))
		if err := p.Add(d); err != nil {
			t.Fatalf("device %d: %v", i, err)
		}
	}
	extra, _, _ := device(t)
	extra.Endpoint += "zz"
	if err := p.Add(extra); err == nil || !strings.Contains(err.Error(), "too many devices") {
		t.Errorf("past %d devices: %v", maxDevices, err)
	}
	info, _ := os.Stat(filepath.Join(dir, "foreman-push.json"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("subscriptions mode %v", info.Mode())
	}
	again := newPusher(t, dir, nil)
	if again.Count() != maxDevices || again.CountFor("robin") != 1 {
		t.Errorf("reloaded %d (robin %d)", again.Count(), again.CountFor("robin"))
	}
	// an unreadable list stops the start rather than being saved over
	os.WriteFile(filepath.Join(dir, "foreman-push.json"), []byte("{broken"), 0o600)
	if _, err := New(dir, "x", Options{}); err == nil {
		t.Errorf("a broken subscription list should stop the start")
	}
}

// Bug 7: the device count is the viewer's own, and a viewer removes only
// their own devices.
func TestDevicesAreCountedPerPerson(t *testing.T) {
	p := newPusher(t, t.TempDir(), nil)
	mine, _, _ := device(t)
	hers, _, _ := device(t)
	hers.Endpoint += "x"
	hers.User = "robin"
	p.Add(mine)
	p.Add(hers)
	if p.CountFor("") != 1 || p.CountFor(Maintainer) != 1 || p.CountFor("robin") != 1 || p.CountFor("other") != 0 {
		t.Errorf("counts: maintainer %d, robin %d", p.CountFor(""), p.CountFor("robin"))
	}
	if err := p.RemoveFor("robin", mine.Endpoint); err != nil || p.Count() != 2 {
		t.Errorf("robin removed the maintainer's device: %v %d", err, p.Count())
	}
	fake := &fakePush{code: 201}
	p.client.Transport = fake
	if n := p.SendTo(context.Background(), "robin", Message{Title: "test"}); n != 1 || len(fake.got) != 1 || fake.got[0].URL.String() != hers.Endpoint {
		t.Errorf("robin's test went to %d devices", len(fake.got))
	}
	if err := p.RemoveFor("robin", hers.Endpoint); err != nil || p.Count() != 1 {
		t.Errorf("robin could not remove her own device: %v %d", err, p.Count())
	}
}

type audience map[string][]string

func (a audience) Sees(user, project string) bool {
	ps, ok := a[user]
	if !ok {
		return false
	}
	for _, p := range ps {
		if p == project {
			return true
		}
	}
	return false
}

// Foreman's TestPushReachesOnlyTheDevicesThatMaySeeIt.
func TestPushReachesOnlyTheDevicesThatMaySeeIt(t *testing.T) {
	p := &Pusher{audience: audience{"robin": {"quotes"}}}
	mine, hers := Subscription{Endpoint: "https://a"}, Subscription{Endpoint: "https://b", User: "robin"}
	gone := Subscription{Endpoint: "https://c", User: "gone"}
	deliver := func(m Message) string {
		var to []string
		for _, s := range []Subscription{mine, hers, gone} {
			if p.allowed(s, m) {
				to = append(to, s.Endpoint)
			}
		}
		return strings.Join(to, ",")
	}
	if got := deliver(Message{Project: "widgets"}); got != "https://a" {
		t.Errorf("widgets news went to %v", got)
	}
	if got := deliver(Message{Project: "quotes"}); got != "https://a,https://b" {
		t.Errorf("quotes news went to %v", got)
	}
	if got := deliver(Message{Project: "quotes", forOthers: true}); got != "https://b" {
		t.Errorf("an item already in the maintainer's summary went to %v", got)
	}
	if got := deliver(Message{Title: "5 updates"}); got != "https://a" {
		t.Errorf("the summary went to %v", got)
	}
	// D19: what a person did themselves is news only to everyone else
	if got := deliver(Message{Project: "quotes", by: "robin"}); got != "https://a" {
		t.Errorf("robin's own doing went to %v", got)
	}
	if got := deliver(Message{Project: "quotes", by: "maintainer"}); got != "https://b" {
		t.Errorf("the maintainer's own doing went to %v", got)
	}
	p.maintainer = func() string { return "alice" }
	if got := deliver(Message{Project: "quotes", by: "alice"}); got != "https://b" {
		t.Errorf("the maintainer's own doing, by their board name, went to %v", got)
	}
	// with no audience, only the maintainer's devices hear anything
	if got := (&Pusher{}).allowed(hers, Message{Project: "quotes"}); got {
		t.Errorf("a user's device heard news with no audience to ask")
	}
}

func TestOtherPeoplesDevicesGetNoBadge(t *testing.T) {
	p := newPusher(t, t.TempDir(), audience{"robin": {"widgets"}})
	mine, mpriv, mauth := device(t)
	hers, hpriv, hauth := device(t)
	hers.Endpoint += "x"
	hers.User = "robin"
	p.Add(mine)
	p.Add(hers)
	fake := &fakePush{code: 201}
	p.client.Transport = fake
	if n := p.Send(context.Background(), Message{Title: "t", Badge: 7, Project: "widgets"}); n != 2 {
		t.Fatalf("sent %d", n)
	}
	badges := map[string]float64{}
	for i, r := range fake.got {
		body, _ := io.ReadAll(r.Body)
		priv, auth := mpriv, mauth
		if r.URL.String() == hers.Endpoint {
			priv, auth = hpriv, hauth
		}
		var m map[string]any
		json.Unmarshal(decrypt(t, body, priv, auth), &m)
		badges[r.URL.String()] = m["badge"].(float64)
		_ = i
	}
	if badges[mine.Endpoint] != 7 || badges[hers.Endpoint] != 0 {
		t.Errorf("badges %v", badges)
	}
}

func decrypt(t *testing.T, body []byte, priv *ecdh.PrivateKey, auth []byte) []byte {
	t.Helper()
	salt, idlen := body[:16], int(body[20])
	asPub := body[21 : 21+idlen]
	pub, _ := ecdh.P256().NewPublicKey(asPub)
	shared, _ := priv.ECDH(pub)
	cek, nonce, _ := deriveKeys(shared, auth, salt, priv.PublicKey().Bytes(), asPub)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		t.Fatal(err)
	}
	return plain[:len(plain)-1]
}

func snapsOf(ts ...*ticket.Ticket) map[string]Snap {
	m := map[string]Snap{}
	for _, t := range ts {
		m[string(t.ID)] = SnapOf(t)
	}
	return m
}

func move(at, detail string) ticket.Entry {
	return ticket.Entry{At: at, Event: "move", Detail: detail}
}

// Foreman's TestNewsIsWhatChangedWithoutYou.
func TestNewsIsWhatChangedWithoutYou(t *testing.T) {
	ts := fixtures(t)
	prev := snapsOf(ts...)
	if n := News(prev, ts, "maintainer"); len(n) != 0 {
		t.Fatalf("nothing changed, yet %v", n)
	}
	// a ticket arrives on the desk through the pipeline
	start := cp(find(ts, "T0002"))
	delete(prev, "T0002")
	out := News(prev, []*ticket.Ticket{start}, "maintainer")
	if len(out) != 1 || out[0].URL != "/t/T0002" || !strings.HasPrefix(out[0].Title, "Start: ") || out[0].Project == "" {
		t.Errorf("new ask: %+v", out)
	}
	// the same, but you moved it there yourself
	mine := cp(start)
	mine.History = append(mine.History, move("2026-10-09T10:00:00+00:00", "ready -> needs_human: by human"))
	if out := News(prev, []*ticket.Ticket{mine}, "maintainer"); len(out) != 1 || out[0].by != "maintainer" {
		t.Errorf("your own move is not news to you: %+v", out)
	}
	// D19: another person's move is still news to you, and not to them
	theirs := cp(start)
	moved := ticket.Moved{From: "ready", To: "needs_human", Reason: model.ReasonByHuman, By: "robin"}
	theirs.History = append(theirs.History, ticket.Entry{At: "2026-10-09T10:00:00+00:00", Event: "move",
		Detail: "ready -> needs_human: by human", Type: moved.EventType(), Data: moved})
	if out := News(prev, []*ticket.Ticket{theirs}, "maintainer"); len(out) != 1 || out[0].by != "robin" {
		t.Errorf("robin's move: %+v", out)
	}
	// a PR ticket closes on GitHub
	prev = snapsOf(find(ts, "T0001"))
	merged := cp(find(ts, "T0001"))
	merged.Column = "done"
	merged.History = append(merged.History, move("2026-10-09T10:00:00+00:00", "pr_open -> done: PR merged by someone"))
	if out := News(prev, []*ticket.Ticket{merged}, "maintainer"); len(out) != 1 || out[0].Title != "T0001 done: PR #23" {
		t.Errorf("merge: %+v", out)
	}
	// a merge you confirmed is yours
	yours := cp(merged)
	yours.History = append(yours.History, move("2026-10-09T10:01:00+00:00", "pr_open -> done: merged on your go-ahead"))
	if out := News(prev, []*ticket.Ticket{yours}, "maintainer"); len(out) != 1 || out[0].by != "maintainer" {
		t.Errorf("your go-ahead is not news to you: %+v", out)
	}
	// the answer to a question comes back: it points at the ticket asked about
	parent := cp(find(ts, "T0001"))
	parent.History = append(parent.History, ticket.Entry{At: "2026-10-09T10:00:00+00:00", Event: "asked", Detail: "T0099: Did Steep pass?"})
	q, _, _ := ticket.Decode([]byte(`{"id": "T0099", "kind": "task", "parent": "T0001", "column": "working", "project": "widgets"}`))
	prev = snapsOf(q, parent)
	a := cp(q)
	a.Column, a.Result = "needs_human", ticket.Some("Yes: Steep passed.")
	out = News(prev, []*ticket.Ticket{parent, a}, "maintainer")
	if len(out) != 1 || out[0].URL != "/t/T0001" || out[0].Body != "Yes: Steep passed." || out[0].Title != "Answer to your question on T0001" {
		t.Errorf("answer: %+v", out)
	}
	// an agent's question opens the conversation
	qt := cp(find(ts, "T0009"))
	if out := News(map[string]Snap{}, []*ticket.Ticket{qt}, "maintainer"); len(out) != 1 || out[0].URL != "/t/T0009/talk#reply" || !strings.HasPrefix(out[0].Title, "T0009 asks: ") {
		t.Errorf("question: %+v", out)
	}
}

func TestManyNewsItemsAreOneSummaryForTheMaintainer(t *testing.T) {
	ts := fixtures(t)
	out := News(map[string]Snap{}, ts, "maintainer")
	if len(out) < 5 || out[0].Tag != "batch" || !strings.HasSuffix(out[0].Title, "updates from the factory") {
		t.Fatalf("batch %+v", out)
	}
	for _, m := range out[1:] {
		if !m.forOthers || m.Badge != out[0].Badge {
			t.Errorf("an item after the summary: %+v", m)
		}
	}
}

// On a board ticket only an agent's new question to a person is news: not a
// stored ask with no open question behind it, not a move, not a question
// already seen.
func TestOnlyABoardQuestionIsPushed(t *testing.T) {
	b, _, err := ticket.Decode([]byte(`{"id": "T0100", "schema": 1, "mode": "board", "project": "sandbox", "title": "Date picker",
		"column": "waiting", "history": [], "ask": {"kind": "question", "question": "?", "since": "2026-10-09T10:00:00+00:00"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if out := News(map[string]Snap{}, []*ticket.Ticket{b}, "maintainer"); len(out) != 0 {
		t.Errorf("a board ticket with no open question was news: %+v", out)
	}
	moved := map[string]Snap{"T0100": {Column: "doing"}}
	if out := News(moved, []*ticket.Ticket{b}, "maintainer"); len(out) != 0 {
		t.Errorf("a board move was news: %+v", out)
	}
	b.Questions = []ticket.BoardQuestion{{N: 1, By: "example-agent", Recipient: "alice", ToPerson: true, Text: "Which locale?",
		At: "2026-10-09T10:00:00+00:00"}}
	out := News(moved, []*ticket.Ticket{b}, "maintainer")
	if len(out) != 1 || out[0].Title != "example-agent asks on sandbox: Which locale?" || out[0].Body != "T0100 · Date picker" ||
		out[0].URL != "/t/T0100/talk#reply" || out[0].Project != "sandbox" || out[0].Badge != 1 {
		t.Fatalf("a board question: %+v", out)
	}
	if out := News(map[string]Snap{"T0100": SnapOf(b)}, []*ticket.Ticket{b}, "maintainer"); len(out) != 0 {
		t.Errorf("a board question seen before was news again: %+v", out)
	}
	// a question to another agent is not the maintainer's
	b.Questions[0].ToPerson, b.Questions[0].Recipient = false, "other-agent"
	if out := News(moved, []*ticket.Ticket{b}, "maintainer"); len(out) != 0 {
		t.Errorf("an agent-to-agent question was news: %+v", out)
	}
}

// boardOf is a snapshot of a root with the fixture tickets.
func boardOf(t *testing.T, root string, ts []*ticket.Ticket) *boardread.Snapshot {
	t.Helper()
	p := paths.FromEnv(map[string]string{"FACTORY_ROOT": root, "HOME": root})
	os.MkdirAll(p.Tickets, 0o755)
	for _, tk := range ts {
		data := ticket.Encode(tk)
		os.WriteFile(p.TicketFile(string(tk.ID)), data, 0o644)
	}
	return boardread.NewSnapshot(&boardread.Reader{Paths: p})
}

// Foreman's TestWatchRemembersAcrossRestartsAndSkipsFailedReads.
func TestWatchRemembersAcrossRestartsAndSkipsFailedReads(t *testing.T) {
	dir := t.TempDir()
	p := newPusher(t, dir, nil)
	once, cancel := context.WithCancel(context.Background())
	cancel()
	board := boardOf(t, filepath.Join(dir, "root"), fixtures(t))
	p.Watch(once, board, time.Hour) // one pass: the baseline
	seen := filepath.Join(dir, "foreman-seen.json")
	data, err := os.ReadFile(seen)
	if err != nil || !strings.Contains(string(data), `"T0001"`) {
		t.Fatalf("seen file %q %v", data, err)
	}
	info, _ := os.Stat(seen)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("seen file mode %v", info.Mode())
	}
	// an unreadable board leaves what was seen alone
	p.Watch(once, boardOf(t, filepath.Join(dir, "nowhere"), nil), time.Hour)
	again, _ := os.ReadFile(seen)
	if string(again) != string(data) {
		t.Errorf("a failed read replaced what was seen")
	}
	// news made while Foreman was down is sent on the first pass
	s, _, _ := device(t)
	p.Add(s)
	fake := &fakePush{code: 201}
	p.client.Transport = fake
	ts := fixtures(t)
	changed := cp(find(ts, "T0010"))
	changed.Column = "needs_human"
	changed.Ask = ticket.Some(ticket.Ask{Kind: "notice", Question: "Read it?", Since: "2026-10-09T11:00:00+00:00"})
	ts[9] = changed
	p.Watch(once, boardOf(t, filepath.Join(dir, "root2"), ts), time.Hour)
	if len(fake.got) != 1 {
		t.Errorf("the change made while down was not announced: %d sends", len(fake.got))
	}
	// a key that exists but cannot be read must not be replaced
	os.Chmod(filepath.Join(dir, "foreman-vapid.pem"), 0)
	if os.Getuid() != 0 {
		if _, err := New(dir, "x", Options{}); err == nil {
			t.Errorf("an unreadable key should stop the start")
		}
	}
}

func TestACorruptKeyStopsTheStart(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "foreman-vapid.pem"), []byte("not pem"), 0o600)
	if _, err := New(dir, "x", Options{}); err == nil {
		t.Errorf("a corrupt key should stop the start")
	}
}
