package web

import (
	"crypto/hmac"
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/stevegeek/agent-factory/internal/auth"
	"github.com/stevegeek/agent-factory/internal/present"
)

// Flash is one message carried across a redirect.
type Flash struct {
	Kind, Text string // Kind: "ok" or "error"
}

const flashCookie = "foreman_flash"

// flashRedirect sets the message, signed and clipped to 600 characters,
// for 60 seconds, and redirects.
func (s *Server) flashRedirect(w http.ResponseWriter, r *http.Request, to, kind, text string) {
	v := base64.RawURLEncoding.EncodeToString([]byte(kind + "\x00" + present.Clip(text, 600)))
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: v + "." + s.auth.MAC("flash", v), Path: "/", MaxAge: 60,
		HttpOnly: true, Secure: auth.Secure(r), SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// takeFlash reads the message once and clears it; a bad signature is
// ignored.
func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) *Flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: auth.Secure(r), SameSite: http.SameSiteStrictMode})
	v, sig, ok := strings.Cut(c.Value, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.auth.MAC("flash", v))) {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return nil
	}
	kind, text, _ := strings.Cut(string(raw), "\x00")
	return &Flash{Kind: kind, Text: text}
}
