package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const sessionCookie = "writer_session"

// auth gates every non-public route behind the shared token. Browsers
// sign in once at /login and get an HttpOnly, SameSite=Strict cookie
// holding an HMAC of the token (never the token itself); scripts and
// other services send "Authorization: Bearer <token>".
type auth struct {
	token   string
	session string // expected cookie value
	secure  bool
	// apiToken, when set, is the only credential /api/v1/ accepts,
	// and it is accepted nowhere else.
	apiToken string
}

func newAuth(token string, secure bool) *auth {
	a := &auth{token: token, secure: secure}
	if token != "" {
		m := hmac.New(sha256.New, []byte(token))
		m.Write([]byte("scheff-writer-session-v1"))
		a.session = hex.EncodeToString(m.Sum(nil))
	}
	return a
}

func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (a *auth) ok(r *http.Request) bool {
	if a.token == "" {
		return true
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return equal(strings.TrimPrefix(h, "Bearer "), a.token)
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return equal(c.Value, a.session)
	}
	return false
}

func public(path string) bool {
	return path == "/healthz" || path == "/login" || path == "/favicon.ico" || strings.HasPrefix(path, "/static/")
}

func (a *auth) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.apiToken != "" && strings.HasPrefix(r.URL.Path, "/api/v1/") {
			h := r.Header.Get("Authorization")
			if strings.HasPrefix(h, "Bearer ") && equal(strings.TrimPrefix(h, "Bearer "), a.apiToken) {
				next.ServeHTTP(w, r)
				return
			}
			apiError(w, http.StatusUnauthorized, "API token required")
			return
		}
		if public(r.URL.Path) || a.ok(r) {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		http.Error(w, "sign in required", http.StatusUnauthorized)
	})
}

// safeNext keeps post-login redirects on this site: a single leading
// slash, no scheme, no "//" or "/\" host trick.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

func (s *server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.auth.token == "" || s.auth.ok(r) {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, "login.html", map[string]any{"Next": safeNext(r.URL.Query().Get("next"))})
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	next := safeNext(r.PostFormValue("next"))
	if s.auth.token == "" {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	if !equal(r.PostFormValue("token"), s.auth.token) {
		time.Sleep(400 * time.Millisecond) // blunt guessing; tailnet-only anyway
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		s.render(w, "login.html", map[string]any{"Next": next, "Failed": true})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.auth.session,
		Path:     "/",
		MaxAge:   30 * 24 * 3600,
		HttpOnly: true,
		Secure:   s.auth.secure,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.auth.secure, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
