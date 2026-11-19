package web

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scheff-of-staff/scheff-writer/internal/llm"
	"github.com/scheff-of-staff/scheff-writer/internal/store"
)

const testToken = "test-token-0123456789abcdef"

// fakeModel is an OpenAI-compatible endpoint that returns a fixed
// reply and records the last request body.
type fakeModel struct {
	reply   string
	lastReq map[string]any
	calls   int
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.calls++
	_ = json.NewDecoder(r.Body).Decode(&f.lastReq)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": map[string]any{"content": f.reply}}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 20},
	})
}

type env struct {
	t     *testing.T
	root  string
	st    *store.Store
	h     http.Handler
	model *fakeModel
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	st := store.New(root)
	if err := st.Create("my-piece", "My Piece"); err != nil {
		t.Fatal(err)
	}
	fm := &fakeModel{reply: "A reply."}
	srv := httptest.NewServer(fm)
	t.Cleanup(srv.Close)
	h, err := New(Config{
		Store: st,
		LLM:   llm.New(srv.URL, "k", "test-model"),
		Token: testToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, root: root, st: st, h: h, model: fm}
}

// do sends an authenticated same-origin request.
func (e *env) do(method, target string, body io.Reader, contentType string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, target, body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) form(target string, vals url.Values) *httptest.ResponseRecorder {
	return e.do("POST", target, strings.NewReader(vals.Encode()), "application/x-www-form-urlencoded")
}

func (e *env) file(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.root, rel))
	if err != nil {
		e.t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// --- auth ------------------------------------------------------------

func TestHealthzNeedsNoAuth(t *testing.T) {
	e := newEnv(t)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz = %d", rec.Code)
	}
}

func TestUnauthenticatedPageRedirectsToLogin(t *testing.T) {
	e := newEnv(t)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/article?slug=my-piece", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("code = %d", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/login?next=") || !strings.Contains(loc, url.QueryEscape("/article?slug=my-piece")) {
		t.Fatalf("Location = %q", loc)
	}
}

func TestUnauthenticatedAPIIs401(t *testing.T) {
	e := newEnv(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/list-revisions?slug=my-piece", nil)
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestWrongBearerIs401(t *testing.T) {
	e := newEnv(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/list-revisions?slug=my-piece", nil)
	req.Header.Set("Authorization", "Bearer nope")
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d", rec.Code)
	}
}

func login(t *testing.T, h http.Handler, token, next string) *httptest.ResponseRecorder {
	t.Helper()
	vals := url.Values{"token": {token}, "next": {next}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLoginSetsCookieAndCookieAuthenticates(t *testing.T) {
	e := newEnv(t)
	rec := login(t, e.h, testToken, "/article?slug=my-piece")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/article?slug=my-piece" {
		t.Fatalf("login = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || !c.Secure {
		t.Fatalf("cookie flags wrong: %+v", c)
	}
	if strings.Contains(c.Value, testToken) {
		t.Fatal("cookie must not carry the raw token")
	}
	req := httptest.NewRequest("GET", "/article?slug=my-piece", nil)
	req.AddCookie(c)
	rec2 := httptest.NewRecorder()
	e.h.ServeHTTP(rec2, req)
	if rec2.Code != 200 {
		t.Fatalf("authenticated page = %d", rec2.Code)
	}
}

func TestLoginWrongTokenRejected(t *testing.T) {
	e := newEnv(t)
	rec := login(t, e.h, "wrong", "/")
	if rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Fatalf("code = %d cookies = %v", rec.Code, rec.Result().Cookies())
	}
}

func TestLoginRejectsOpenRedirect(t *testing.T) {
	e := newEnv(t)
	for _, next := range []string{"//evil.example/x", "https://evil.example/", "/\\evil.example", "javascript:alert(1)"} {
		rec := login(t, e.h, testToken, next)
		if loc := rec.Header().Get("Location"); loc != "/" {
			t.Fatalf("next=%q → Location %q, want /", next, loc)
		}
	}
}

func TestNoTokenRequiresExplicitOptOut(t *testing.T) {
	_, err := New(Config{Store: store.New(t.TempDir())})
	if err == nil {
		t.Fatal("New without Token or AllowNoAuth must fail")
	}
	h, err := New(Config{Store: store.New(t.TempDir()), AllowNoAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 200 {
		t.Fatalf("no-auth index = %d", rec.Code)
	}
}

func TestCrossSiteWriteBlocked(t *testing.T) {
	e := newEnv(t)
	vals := url.Values{"slug": {"my-piece"}, "body": {"hijacked"}}
	req := httptest.NewRequest("POST", "/api/update-project", strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d", rec.Code)
	}
	if strings.Contains(e.file("04_Projects/my-piece.md"), "hijacked") {
		t.Fatal("cross-site write landed")
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/", nil, "")
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
}

// --- pages -----------------------------------------------------------

func TestIndexListsProjects(t *testing.T) {
	e := newEnv(t)
	rec := e.do("GET", "/", nil, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "My Piece") || !strings.Contains(rec.Body.String(), "/article?slug=my-piece") {
		t.Fatalf("index = %d\n%s", rec.Code, rec.Body.String())
	}
}

func TestCreateProject(t *testing.T) {
	e := newEnv(t)
	rec := e.form("/api/create-project", url.Values{"title": {"Notes on Ferns!"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/article?slug=notes-on-ferns" {
		t.Fatalf("create = %d %q %s", rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if !strings.Contains(e.file("04_Projects/notes-on-ferns.md"), "# Notes on Ferns!") {
		t.Fatal("project file missing title")
	}
	rec = e.form("/api/create-project", url.Values{"title": {"Notes on Ferns"}})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create = %d", rec.Code)
	}
	rec = e.form("/api/create-project", url.Values{"title": {"!!!"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsluggable create = %d", rec.Code)
	}
}

func TestArticlePage(t *testing.T) {
	e := newEnv(t)
	if _, err := e.st.SaveBody("my-piece", "# My Piece\n\nHello <b>there</b>.", "manual"); err != nil {
		t.Fatal(err)
	}
	rec := e.do("GET", "/article?slug=my-piece", nil, "")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "My Piece") {
		t.Fatalf("article = %d", rec.Code)
	}
	if strings.Contains(body, "<b>there</b>") {
		t.Fatal("raw HTML from the draft must be escaped in preview and textarea")
	}
	if rec := e.do("GET", "/article?slug=../etc", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad slug = %d", rec.Code)
	}
	if rec := e.do("GET", "/article?slug=nope", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("missing = %d", rec.Code)
	}
}

// --- saving ----------------------------------------------------------

func TestUpdateProjectBodyKeepsLogAndWritesRevision(t *testing.T) {
	e := newEnv(t)
	path := "04_Projects/my-piece.md"
	before := e.file(path)
	if err := os.WriteFile(filepath.Join(e.root, path), []byte(before+"- 2026-01-01 kept entry\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := e.form("/api/update-project", url.Values{"slug": {"my-piece"}, "body": {"# My Piece\n\nNew words."}, "quiet": {"1"}, "source": {"auto"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "form-result ok") {
		t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "reload") {
		t.Fatal("quiet save must not reload the page")
	}
	got := e.file(path)
	if !strings.Contains(got, "New words.") || !strings.Contains(got, "kept entry") {
		t.Fatalf("saved file:\n%s", got)
	}
	revs, _ := e.st.Revisions("my-piece")
	if len(revs) != 1 || revs[0].Source != "auto" {
		t.Fatalf("revisions = %+v", revs)
	}
}

func TestUpdateProjectValidation(t *testing.T) {
	e := newEnv(t)
	cases := map[string]url.Values{
		"bad slug":   {"slug": {"../x"}, "body": {"x"}},
		"nothing":    {"slug": {"my-piece"}},
		"bad status": {"slug": {"my-piece"}, "status": {"deleted"}},
		"too big":    {"slug": {"my-piece"}, "body": {strings.Repeat("x", store.MaxBody+1)}},
	}
	for name, vals := range cases {
		rec := e.form("/api/update-project", vals)
		if !strings.Contains(rec.Body.String(), "form-result err") {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestUpdateProjectStatus(t *testing.T) {
	e := newEnv(t)
	rec := e.form("/api/update-project", url.Values{"slug": {"my-piece"}, "status": {"paused"}})
	if !strings.Contains(rec.Body.String(), "form-result ok") {
		t.Fatal(rec.Body.String())
	}
	if !strings.Contains(e.file("04_Projects/my-piece.md"), "status: paused") {
		t.Fatal("status not written")
	}
}

func TestPreviewEscapesRawHTMLAndRewritesAttachments(t *testing.T) {
	e := newEnv(t)
	rec := e.form("/api/preview", url.Values{"slug": {"my-piece"}, "body": {"**hi** <script>x</script>\n\n![](attachments/2026-01-01-120000-abcdef01.png)"}})
	out := rec.Body.String()
	if !strings.Contains(out, "<strong>hi</strong>") || strings.Contains(out, "<script>") {
		t.Fatalf("preview:\n%s", out)
	}
	if !strings.Contains(out, "/api/attachment?name=2026-01-01-120000-abcdef01.png&slug=my-piece") {
		t.Fatalf("attachment src not rewritten:\n%s", out)
	}
}

// --- revisions -------------------------------------------------------

func TestRevisions(t *testing.T) {
	e := newEnv(t)
	if _, err := e.st.SaveBody("my-piece", "first draft", "manual"); err != nil {
		t.Fatal(err)
	}
	rec := e.do("GET", "/api/list-revisions?slug=my-piece", nil, "")
	var revs []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &revs); err != nil || len(revs) != 1 {
		t.Fatalf("list = %s (%v)", rec.Body.String(), err)
	}
	name := revs[0]["name"].(string)
	rec = e.do("GET", "/api/load-revision?slug=my-piece&name="+name, nil, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "first draft") {
		t.Fatalf("load = %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", "/api/load-revision?slug=my-piece&name=../../x", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad revision name = %d", rec.Code)
	}
}

// --- attachments -----------------------------------------------------

func upload(e *env, data []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("slug", "my-piece")
	fw, _ := mw.CreateFormFile("image", "paste.png")
	_, _ = fw.Write(data)
	_ = mw.Close()
	return e.do("POST", "/api/upload-attachment", &buf, mw.FormDataContentType())
}

var tinyPNG = append([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{0}, 32)...)

func TestUploadAndServeAttachment(t *testing.T) {
	e := newEnv(t)
	rec := upload(e, tinyPNG)
	var got struct {
		Filename string `json:"filename"`
		MIME     string `json:"mime"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.MIME != "image/png" {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body.String())
	}
	rec = e.do("GET", "/api/attachment?slug=my-piece&name="+got.Filename, nil, "")
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), tinyPNG) || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("serve = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestUploadRejectsDisguisedFile(t *testing.T) {
	e := newEnv(t)
	rec := upload(e, []byte("#!/bin/sh\necho this is not an image at all\n"))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestAttachmentRejectsTraversal(t *testing.T) {
	e := newEnv(t)
	if rec := e.do("GET", "/api/attachment?slug=my-piece&name=../../my-piece.md", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d", rec.Code)
	}
}

// --- sources ---------------------------------------------------------

func TestRemoveSource(t *testing.T) {
	e := newEnv(t)
	name, err := e.st.AddSource("my-piece", store.NewSource{Title: "Ref", Kind: "text", Body: "Summary."})
	if err != nil {
		t.Fatal(err)
	}
	rec := e.form("/api/remove-source", url.Values{"slug": {"my-piece"}, "name": {name}})
	if !strings.Contains(rec.Body.String(), "form-result ok") {
		t.Fatal(rec.Body.String())
	}
	if srcs, _ := e.st.Sources("my-piece"); len(srcs) != 0 {
		t.Fatalf("source still present: %+v", srcs)
	}
	rec = e.form("/api/remove-source", url.Values{"slug": {"my-piece"}, "name": {"../x"}})
	if !strings.Contains(rec.Body.String(), "form-result err") {
		t.Fatal("bad source name accepted")
	}
}

func TestAddSourceNotYetAvailable(t *testing.T) {
	e := newEnv(t)
	rec := e.form("/api/add-source", url.Values{"slug": {"my-piece"}, "kind": {"url"}, "url": {"https://example.com"}})
	if !strings.Contains(rec.Body.String(), "form-result err") {
		t.Fatal(rec.Body.String())
	}
}

// --- model calls -----------------------------------------------------

func TestTransformDoesNotWrite(t *testing.T) {
	e := newEnv(t)
	e.model.reply = "## Polished\n\nBetter words."
	before := e.file("04_Projects/my-piece.md")
	rec := e.form("/api/transform-article", url.Values{"slug": {"my-piece"}, "body": {"rough words"}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Better words.") {
		t.Fatalf("transform = %d %s", rec.Code, rec.Body.String())
	}
	if e.file("04_Projects/my-piece.md") != before {
		t.Fatal("transform must not write the project")
	}
	if e.model.lastReq["model"] != "test-model" {
		t.Fatalf("model = %v", e.model.lastReq["model"])
	}
	rec = e.form("/api/transform-article", url.Values{"slug": {"my-piece"}, "body": {"   "}})
	if !strings.Contains(rec.Body.String(), "form-result err") {
		t.Fatal("empty draft accepted")
	}
}

func TestChatPersistsAndResets(t *testing.T) {
	e := newEnv(t)
	e.model.reply = "Have you considered **ferns**?"
	rec := e.form("/api/article-chat", url.Values{"slug": {"my-piece"}, "body": {"draft"}, "message": {"What next?"}})
	out := rec.Body.String()
	// The editor draws the user's bubble itself; the server returns
	// only the assistant turn.
	if !strings.Contains(out, "<strong>ferns</strong>") {
		t.Fatalf("chat:\n%s", out)
	}
	turns, _ := e.st.Chat("my-piece")
	if len(turns) != 2 || turns[0].Content != "What next?" {
		t.Fatalf("turns = %+v", turns)
	}
	// Second turn sends the first as history.
	e.form("/api/article-chat", url.Values{"slug": {"my-piece"}, "message": {"And then?"}})
	if msgs, _ := e.model.lastReq["messages"].([]any); len(msgs) != 4 { // system + 2 history + new
		t.Fatalf("messages sent = %d", len(msgs))
	}
	rec = e.form("/api/article-chat-reset", url.Values{"slug": {"my-piece"}})
	if !strings.Contains(rec.Body.String(), "form-result ok") {
		t.Fatal(rec.Body.String())
	}
	if turns, _ := e.st.Chat("my-piece"); len(turns) != 0 {
		t.Fatal("chat not reset")
	}
}

func TestChatImageAttachmentSentOnce(t *testing.T) {
	e := newEnv(t)
	name, err := e.st.SaveAttachment("my-piece", "image/png", tinyPNG)
	if err != nil {
		t.Fatal(err)
	}
	e.form("/api/article-chat", url.Values{"slug": {"my-piece"}, "message": {"Look"}, "attachments": {name + ",../../bad.png"}})
	msgs, _ := e.model.lastReq["messages"].([]any)
	last, _ := msgs[len(msgs)-1].(map[string]any)
	parts, ok := last["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("expected text + one image part, got %v", last["content"])
	}
	turns, _ := e.st.Chat("my-piece")
	if !strings.Contains(turns[0].Content, "![](attachments/"+name+")") {
		t.Fatalf("persisted user turn lacks image ref: %q", turns[0].Content)
	}
}

func TestChatEmptyMessage(t *testing.T) {
	e := newEnv(t)
	rec := e.form("/api/article-chat", url.Values{"slug": {"my-piece"}, "message": {"  "}})
	if !strings.Contains(rec.Body.String(), "chat-error") || e.model.calls != 0 {
		t.Fatalf("%s calls=%d", rec.Body.String(), e.model.calls)
	}
}
