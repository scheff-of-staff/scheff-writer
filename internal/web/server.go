// Package web is scheff-writer's HTTP surface: the projects index, the
// editor page, and the small API the editor calls (save, preview,
// Transform, chat, sources, attachments, revisions).
//
// Every request is authenticated (token cookie or bearer) unless the
// operator explicitly opts out, and every state-changing request must
// come from the writer's own pages (net/http CrossOriginProtection).
// All file access goes through internal/store, which owns path scoping.
package web

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/scheff-of-staff/scheff-writer/internal/llm"
	"github.com/scheff-of-staff/scheff-writer/internal/store"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

// Config wires the server.
type Config struct {
	Store *store.Store
	// LLM is the model client for Transform and chat. Nil (or
	// unconfigured) makes those features report a clear error.
	LLM *llm.Client

	// Token is the shared secret for sign-in. Empty is only allowed
	// with AllowNoAuth, for a writer bound to localhost on one machine.
	Token       string
	AllowNoAuth bool
	// InsecureCookie drops the Secure flag on the session cookie, for
	// plain-http local development. Leave false behind Tailscale Serve
	// or any TLS proxy.
	InsecureCookie bool

	// HomeURL, when set, adds a link back to the host application
	// (for example the Scheff of Staff dashboard) in the header.
	HomeURL string
	// ProjectURL, when set, is a prefix the slug is appended to for a
	// "Project page" link, e.g. "https://host:8444/project?slug=".
	ProjectURL string

	// Fetcher fetches URL sources (forager). Nil leaves "add by URL"
	// off; pasted text and PDF uploads still work.
	Fetcher SourceFetcher

	// APIToken turns on the JSON API under /api/v1/ (see api.go). It
	// must differ from Token and be at least 16 characters. Empty
	// leaves the API off.
	APIToken string
	// APIModels lists the model aliases an API request may pick.
	// The configured LLM model is always allowed; empty allows only it.
	APIModels []string
	// APIMaxConcurrent caps API calls in flight; extra calls get 429.
	// Zero means 4.
	APIMaxConcurrent int
}

type server struct {
	cfg  Config
	tpl  *template.Template
	auth *auth

	apiModels map[string]bool
	apiSem    chan struct{}
	sourceSem chan struct{}
}

// New builds the HTTP handler.
func New(cfg Config) (http.Handler, error) {
	if cfg.Store == nil {
		return nil, errors.New("web: Store is required")
	}
	if cfg.Token == "" && !cfg.AllowNoAuth {
		return nil, errors.New("web: a sign-in token is required (or explicitly allow no auth)")
	}
	if cfg.APIToken != "" {
		if len(cfg.APIToken) < 16 {
			return nil, errors.New("web: the API token must be at least 16 characters")
		}
		if cfg.APIToken == cfg.Token {
			return nil, errors.New("web: the API token must differ from the sign-in token")
		}
	}
	if cfg.LLM == nil {
		cfg.LLM = llm.New("", "", "")
	}
	if cfg.APIMaxConcurrent <= 0 {
		cfg.APIMaxConcurrent = defaultAPIMaxConcurrent
	}
	s := &server{
		cfg:       cfg,
		apiModels: map[string]bool{cfg.LLM.Model: true},
		apiSem:    make(chan struct{}, cfg.APIMaxConcurrent),
		sourceSem: make(chan struct{}, maxSourcesAtOnce),
	}
	for _, m := range cfg.APIModels {
		if m = strings.TrimSpace(m); m != "" {
			s.apiModels[m] = true
		}
	}
	funcs := template.FuncMap{
		"homeURL": func() string { return cfg.HomeURL },
		"model": func() string {
			if cfg.LLM.Model == "" {
				return "the model"
			}
			return cfg.LLM.Model
		},
		"projectURL": func(slug string) string {
			if cfg.ProjectURL == "" {
				return ""
			}
			return cfg.ProjectURL + slug
		},
	}
	tpl, err := template.New("").Funcs(funcs).ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}
	s.tpl = tpl
	s.auth = newAuth(cfg.Token, !cfg.InsecureCookie)
	s.auth.apiToken = cfg.APIToken

	mux := http.NewServeMux()
	staticSub, _ := fs.Sub(staticFiles, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, staticFiles, "static/favicon.png")
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /article", s.handleArticle)
	mux.HandleFunc("POST /api/create-project", s.handleCreateProject)
	mux.HandleFunc("POST /api/update-project", s.handleUpdateProject)
	mux.HandleFunc("POST /api/preview", s.handlePreview)
	mux.HandleFunc("POST /api/transform-article", s.handleTransform)
	mux.HandleFunc("POST /api/article-chat", s.handleChat)
	mux.HandleFunc("POST /api/article-chat-reset", s.handleChatReset)
	mux.HandleFunc("POST /api/add-source", s.handleAddSource)
	mux.HandleFunc("POST /api/remove-source", s.handleRemoveSource)
	mux.HandleFunc("POST /api/upload-attachment", s.handleUploadAttachment)
	mux.HandleFunc("GET /api/attachment", s.handleAttachment)
	mux.HandleFunc("GET /api/list-revisions", s.handleListRevisions)
	mux.HandleFunc("GET /api/load-revision", s.handleLoadRevision)
	if cfg.APIToken != "" {
		mux.HandleFunc("POST /api/v1/projects/{slug}/chat", s.handleAPIChat)
	}

	var h http.Handler = mux
	h = s.auth.middleware(h)
	h = http.NewCrossOriginProtection().Handler(h)
	h = securityHeaders(h)
	return h, nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// --- helpers ---------------------------------------------------------

func (s *server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

// fragmentOK writes the small result fragment the editor's htmx forms
// expect. reloadSec > 0 reloads the page after that many seconds.
func fragmentOK(w http.ResponseWriter, html string, reloadSec int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if reloadSec <= 0 {
		_, _ = fmt.Fprintf(w, `<div class="form-result ok">%s</div>`, html)
		return
	}
	_, _ = fmt.Fprintf(w, `<div class="form-result ok">%s</div>
<script>setTimeout(function(){ window.location.reload(); }, %d000);</script>`, html, reloadSec)
}

// fragmentErr reports a failure inline. Status stays 200 so htmx swaps
// it in; the class carries the error state.
func fragmentErr(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<div class="form-result err">%s</div>`, template.HTMLEscapeString(msg))
}

func formSlug(r *http.Request) (string, bool) {
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	return slug, store.SlugRe.MatchString(slug)
}

// errClause is the first clause of an error, safe to show the author
// without leaking paths or upstream bodies.
func errClause(err error) string {
	s := err.Error()
	if i := strings.Index(s, ": "); i > 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugify derives a project slug from a title: lowercase ASCII words
// joined by hyphens, at most 40 characters. "" if nothing usable.
func slugify(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		if r < unicode.MaxASCII {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	s := strings.Trim(nonSlug.ReplaceAllString(b.String(), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if !store.SlugRe.MatchString(s) {
		return ""
	}
	return s
}

func kb(n int) string {
	switch {
	case n <= 0:
		return ""
	case n < 1024:
		return "<1KB"
	}
	return fmt.Sprintf("%dKB", n/1024)
}
