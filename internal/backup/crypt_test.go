package backup

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey() []byte { return bytes.Repeat([]byte{7}, 32) }

func seal(t *testing.T, plain []byte, chunk int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := Encrypt(&buf, testKey(), chunk, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// written in odd pieces, so chunk boundaries fall inside writes
	for len(plain) > 0 {
		n := min(len(plain), 5)
		if _, err := w.Write(plain[:n]); err != nil {
			t.Fatal(err)
		}
		plain = plain[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func open(sealed []byte, key []byte) ([]byte, error) {
	r, err := Decrypt(bytes.NewReader(sealed), key)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// an encrypt/decrypt round trip, at and around chunk boundaries.
func TestEncryptRoundTrip(t *testing.T) {
	const chunk = 16
	for _, n := range []int{0, 1, chunk - 1, chunk, chunk + 1, 3 * chunk, 3*chunk + 7} {
		plain := make([]byte, n)
		rand.Read(plain)
		sealed := seal(t, plain, chunk)
		got, err := open(sealed, testKey())
		if err != nil {
			t.Fatalf("%d bytes: %v", n, err)
		}
		if !bytes.Equal(got, plain) {
			t.Errorf("%d bytes: round trip changed the data", n)
		}
		if bytes.Contains(sealed, []byte("secret")) {
			t.Error("plaintext in the output")
		}
	}
}

// a tampered chunk fails, and so does every other change to the
// stream: a wrong key, a cut at a chunk boundary, a dropped or reordered
// chunk, an extension, a changed header.
func TestTamperedArchivesFail(t *testing.T) {
	const chunk = 16
	plain := bytes.Repeat([]byte("the board, the memory and the logs. "), 4) // several chunks
	sealed := seal(t, plain, chunk)
	ct := chunk + 16 // a sealed chunk
	body := sealed[headerLen:]
	chunks := [][]byte{}
	for len(body) > 0 {
		n := min(ct, len(body))
		chunks = append(chunks, body[:n])
		body = body[n:]
	}
	join := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	header := sealed[:headerLen]
	flipped := bytes.Clone(sealed)
	flipped[headerLen+ct+3] ^= 1
	hdr := bytes.Clone(sealed)
	hdr[headerLen-1] ^= 1 // the nonce prefix
	cases := map[string][]byte{
		"a flipped bit in chunk 2": flipped,
		"cut after a whole chunk":  join(header, chunks[0], chunks[1]),
		"cut inside a chunk":       sealed[:len(sealed)-5],
		"a chunk dropped":          join(append([][]byte{header, chunks[0]}, chunks[2:]...)...),
		"two chunks swapped":       join(append([][]byte{header, chunks[1], chunks[0]}, chunks[2:]...)...),
		"bytes appended":           join(sealed, []byte("more")),
		"a changed header":         hdr,
		"the header alone":         header,
		"the last chunk repeated":  join(sealed, chunks[len(chunks)-1]),
	}
	for name, b := range cases {
		if _, err := open(b, testKey()); !errors.Is(err, ErrTampered) {
			t.Errorf("%s: err = %v, want ErrTampered", name, err)
		}
	}
	other := bytes.Repeat([]byte{8}, 32)
	if _, err := open(sealed, other); !errors.Is(err, ErrTampered) {
		t.Errorf("wrong key: %v", err)
	}
	if _, err := open([]byte("PK\x03\x04 not ours at all, long enough"), testKey()); err == nil || errors.Is(err, ErrTampered) {
		t.Errorf("another format: %v", err)
	}
}

func TestKeygenAndReadKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "backup-key")
	h, err := Keygen(p, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 64 {
		t.Errorf("key %q", h)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	k, err := ReadKey(p)
	if err != nil || len(k) != 32 {
		t.Fatalf("ReadKey: %v %d", err, len(k))
	}
	if _, err := Keygen(p, rand.Reader); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Errorf("a second keygen must refuse: %v", err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadKey(p); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Errorf("a readable key: %v", err)
	}
	short := filepath.Join(dir, "short")
	os.WriteFile(short, []byte("abcd\n"), 0o600)
	if _, err := ReadKey(short); err == nil {
		t.Error("a short key read")
	}
}
