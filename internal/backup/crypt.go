package backup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// The encrypted archive (§15.2): a header, then the archive in GCM-sealed
// chunks, so neither side holds more than one chunk in memory.
//
//	header: magic "FBACKUP" | version 1 | chunk size (uint32, big-endian) | nonce prefix (7 bytes)
//	chunk:  AES-256-GCM(plaintext of chunk size bytes, or fewer for the last)
//
// Chunk i's nonce is the prefix, i as a big-endian uint32 and a byte that
// is 1 on the last chunk only (the STREAM construction), and its additional
// data is the header: a reordered, dropped, truncated or extended stream,
// or a changed header, fails to open. The last chunk may be empty.
const (
	magic        = "FBACKUP"
	version      = 1
	headerLen    = len(magic) + 1 + 4 + prefixLen
	prefixLen    = 7
	keyLen       = 32
	DefaultChunk = 1 << 20 // 1 MiB of plaintext per chunk
	maxChunk     = 16 << 20
)

// ErrTampered is a stream that does not open with the key: a wrong key, or
// a changed, reordered or cut archive.
var ErrTampered = errors.New("backup: the archive does not open with this key (wrong key, or a changed or cut file)")

// ReadKey reads a 32-byte key written as 64 hex digits. The file must be
// readable by its owner only.
func ReadKey(path string) ([]byte, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("backup key: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("backup key: %s is mode %#o; it must be 0600", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("backup key: %w", err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) != keyLen {
		return nil, fmt.Errorf("backup key: %s does not hold 64 hex digits (32 bytes)", path)
	}
	return key, nil
}

// Keygen makes a new key in path (0600; it refuses to replace one) and
// returns it as hex, for the maintainer to keep.
func Keygen(path string, random io.Reader) (string, error) {
	key := make([]byte, keyLen)
	if _, err := io.ReadFull(random, key); err != nil {
		return "", fmt.Errorf("backup keygen: %w", err)
	}
	h := hex.EncodeToString(key)
	// a rebuilt machine may have no ~/.config/factory yet
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("backup keygen: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("backup keygen: %s exists; a new key would leave every backup made with it unreadable", path)
		}
		return "", fmt.Errorf("backup keygen: %w", err)
	}
	if _, err := f.WriteString(h + "\n"); err != nil {
		f.Close()
		return "", fmt.Errorf("backup keygen: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("backup keygen: %w", err)
	}
	return h, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("backup: the key is %d bytes, not %d", len(key), keyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	return cipher.NewGCM(block)
}

func nonce(prefix []byte, i uint32, last bool) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint32(n[prefixLen:], i)
	if last {
		n[11] = 1
	}
	return n
}

// sealer is the encrypting writer: Close seals the last chunk.
type sealer struct {
	w      io.Writer
	aead   cipher.AEAD
	header []byte
	prefix []byte
	buf    []byte
	size   int
	n      uint32
	closed bool
}

// Encrypt returns a writer that encrypts what it is given into w; Close
// writes the last chunk (and does not close w).
func Encrypt(w io.Writer, key []byte, chunk int, random io.Reader) (io.WriteCloser, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if chunk <= 0 || chunk > maxChunk {
		return nil, fmt.Errorf("backup: chunk size %d out of range", chunk)
	}
	prefix := make([]byte, prefixLen)
	if _, err := io.ReadFull(random, prefix); err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	header := make([]byte, 0, headerLen)
	header = append(header, magic...)
	header = append(header, version)
	header = binary.BigEndian.AppendUint32(header, uint32(chunk))
	header = append(header, prefix...)
	if _, err := w.Write(header); err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	return &sealer{w: w, aead: aead, header: header, prefix: prefix, buf: make([]byte, 0, chunk), size: chunk}, nil
}

func (s *sealer) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("backup: write after close")
	}
	written := 0
	for len(p) > 0 {
		room := s.size - len(s.buf)
		take := min(room, len(p))
		s.buf = append(s.buf, p[:take]...)
		p, written = p[take:], written+take
		// a full chunk is sealed only once more data follows: the last
		// chunk must carry the last flag
		if len(s.buf) == s.size && len(p) > 0 {
			if err := s.seal(false); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (s *sealer) seal(last bool) error {
	if s.n == ^uint32(0) {
		return errors.New("backup: the archive is too large for one stream")
	}
	out := s.aead.Seal(nil, nonce(s.prefix, s.n, last), s.buf, s.header)
	s.n++
	s.buf = s.buf[:0]
	if _, err := s.w.Write(out); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

func (s *sealer) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.seal(true)
}

// opener is the decrypting reader.
type opener struct {
	r      io.Reader
	aead   cipher.AEAD
	header []byte
	prefix []byte
	size   int
	n      uint32
	out    []byte
	next   []byte // the chunk read ahead, to know which one is last
	done   bool
	err    error
}

// Decrypt returns a reader of the plaintext of the stream r. It fails with
// ErrTampered on any chunk that does not open, and on a stream that ends
// before its last chunk.
func Decrypt(r io.Reader, key []byte) (io.Reader, error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	header := make([]byte, headerLen)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, ErrTampered
	}
	if !bytes.Equal(header[:len(magic)], []byte(magic)) || header[len(magic)] != version {
		return nil, fmt.Errorf("backup: not a factory backup (or a newer format)")
	}
	size := int(binary.BigEndian.Uint32(header[len(magic)+1:]))
	if size <= 0 || size > maxChunk {
		return nil, ErrTampered
	}
	o := &opener{r: r, aead: aead, header: header, prefix: header[len(magic)+5:], size: size}
	o.next, o.err = o.readChunk()
	return o, nil
}

func (o *opener) readChunk() ([]byte, error) {
	b := make([]byte, o.size+o.aead.Overhead())
	n, err := io.ReadFull(o.r, b)
	if err == io.EOF {
		return nil, nil // no more chunks
	}
	if err != nil && err != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("backup: %w", err)
	}
	return b[:n], nil
}

func (o *opener) Read(p []byte) (int, error) {
	for len(o.out) == 0 {
		if o.err != nil {
			return 0, o.err
		}
		if o.done {
			return 0, io.EOF
		}
		cur := o.next
		if cur == nil {
			o.err = ErrTampered // the stream ended before its last chunk
			return 0, o.err
		}
		var err error
		if len(cur) < o.size+o.aead.Overhead() {
			// a short chunk is the last: nothing may follow it
			o.next = nil
			if n, _ := o.r.Read(make([]byte, 1)); n > 0 {
				o.err = ErrTampered
				return 0, o.err
			}
		} else if o.next, err = o.readChunk(); err != nil {
			o.err = err
			return 0, err
		}
		last := o.next == nil
		plain, err := o.aead.Open(nil, nonce(o.prefix, o.n, last), cur, o.header)
		if err != nil {
			o.err = ErrTampered
			return 0, o.err
		}
		o.n++
		o.out = plain
		if last {
			o.done = true
		}
	}
	n := copy(p, o.out)
	o.out = o.out[n:]
	return n, nil
}

// cryptoRand is the default randomness.
func cryptoRand(r io.Reader) io.Reader {
	if r == nil {
		return rand.Reader
	}
	return r
}
