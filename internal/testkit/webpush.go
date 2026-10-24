package testkit

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

// PushDevice is a browser's side of a Web Push subscription: its key pair
// and auth secret. P256dh and Auth are the subscription's keys as the
// browser gives them (base64url, unpadded).
type PushDevice struct {
	Priv         *ecdh.PrivateKey
	Secret       []byte
	P256dh, Auth string
}

// NewPushDevice makes a device's keys.
func NewPushDevice(t testing.TB) PushDevice {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	rand.Read(secret)
	enc := base64.RawURLEncoding
	return PushDevice{Priv: priv, Secret: secret, P256dh: enc.EncodeToString(priv.PublicKey().Bytes()), Auth: enc.EncodeToString(secret)}
}

// Open decrypts one aes128gcm push body (RFC 8291's receiving side) and
// returns the payload without its padding delimiter.
func (d PushDevice) Open(t testing.TB, body []byte) []byte {
	t.Helper()
	if len(body) < 21 || len(body) < 21+int(body[20]) {
		t.Fatalf("a push body of %d bytes", len(body))
	}
	salt, idlen := body[:16], int(body[20])
	asPub := body[21 : 21+idlen]
	pub, err := ecdh.P256().NewPublicKey(asPub)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := d.Priv.ECDH(pub)
	if err != nil {
		t.Fatal(err)
	}
	prkKey, _ := hkdf.Extract(sha256.New, shared, d.Secret)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, "WebPush: info\x00"+string(d.Priv.PublicKey().Bytes())+string(asPub), 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil || len(plain) == 0 {
		t.Fatalf("the device cannot decrypt: %v", err)
	}
	return plain[:len(plain)-1]
}
