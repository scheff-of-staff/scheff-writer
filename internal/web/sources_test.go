package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scheff-of-staff/scheff-writer/internal/forager"
	"github.com/scheff-of-staff/scheff-writer/internal/llm"
	"github.com/scheff-of-staff/scheff-writer/internal/store"
)

// --- composition --------------------------------------------------

func TestComposeSourceBodyMatchesWorkflow(t *testing.T) {
	ex := &llm.Extraction{
		Summary:   "First para.\n\nSecond para.",
		KeyPoints: []string{"One", "Two"},
		Tables: []llm.Table{
			{Caption: "RACI | Appendix B", Rows: [][]string{{"Task", "Owner"}, {"Ship", "R|A"}, {}, {"Multi\nline", ""}}},
			{Caption: "", Rows: nil},
		},
		Charts: []llm.Chart{
			{Caption: "Growth", Description: "Revenue by year", Data: [][]string{{"2024", "10"}, {"2025"}}},
			{Caption: "Empty", Description: "", Data: nil},
		},
	}
	got := composeSourceBody("My Title", ex)
	want := "# My Title\n\nFirst para.\n\nSecond para." +
		"\n\n## Key points\n\n- One\n- Two" +
		"\n\n## Tables\n\n" +
		"**RACI | Appendix B**\n\n| Task | Owner |\n| --- | --- |\n| Ship | R\\|A |\n| Multi line |  |" +
		"\n\n_(table data not extracted)_" +
		"\n\n## Charts\n\n" +
		"### Growth\n\nRevenue by year\n\n| Label | Value |\n| --- | --- |\n| 2024 | 10 |\n| 2025 |  |" +
		"\n\n### Empty\n\n_(chart data not extracted)_"
	if got != want {
		t.Fatalf("got:\n%s\n---\nwant:\n%s", got, want)
	}
	plain := composeSourceBody("T", &llm.Extraction{Summary: "S"})
	if plain != "# T\n\nS" {
		t.Fatalf("plain = %q", plain)
	}
}

// --- handler harness ----------------------------------------------

type fakeFetcher struct {
	doc   *forager.Doc
	err   error
	calls int
	url   string
}

func (f *fakeFetcher) FetchOne(_ context.Context, u string) (*forager.Doc, error) {
	f.calls++
	f.url = u
	return f.doc, f.err
}

func htmlDoc(final, text string) *forager.Doc {
	d := &forager.Doc{Title: "Page", MIME: "text/html", Text: text}
	d.Source.URL = final
	return d
}

func pdfDoc(final string, raw []byte, truncated bool) *forager.Doc {
	d := &forager.Doc{Title: "a.pdf", MIME: "application/pdf", Raw: raw, Truncated: truncated}
	d.Source.URL = final
	return d
}

var fixedNow = time.Date(2026, 11, 21, 15, 4, 5, 0, time.UTC)

func newSourceEnv(t *testing.T, f SourceFetcher) *env {
	t.Helper()
	root := t.TempDir()
	st := store.New(root)
	st.Now = func() time.Time { return fixedNow }
	if err := st.Create("my-piece", "My Piece"); err != nil {
		t.Fatal(err)
	}
	fm := &fakeModel{reply: `{"title":"Extracted Title","summary":"The summary.","key_points":["Point A"]}`}
	srv := httptest.NewServer(fm)
	t.Cleanup(srv.Close)
	h, err := New(Config{Store: st, LLM: llm.New(srv.URL, "k", "test-model"), Token: testToken, Fetcher: f})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, root: root, st: st, h: h, model: fm}
}

func (e *env) sourceFiles() []string {
	e.t.Helper()
	matches, _ := filepath.Glob(filepath.Join(e.root, "04_Projects", "my-piece", "sources", "*.md"))
	return matches
}

func (e *env) onlySource() string {
	e.t.Helper()
	files := e.sourceFiles()
	if len(files) != 1 {
		e.t.Fatalf("source files = %v", files)
	}
	b, _ := os.ReadFile(files[0])
	return string(b)
}

func addOK(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if !strings.Contains(rec.Body.String(), "form-result ok") {
		t.Fatalf("not ok: %s", rec.Body.String())
	}
}

func addErr(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	body := rec.Body.String()
	if !strings.Contains(body, "form-result err") || !strings.Contains(body, want) {
		t.Fatalf("want error containing %q, got %d %s", want, rec.Code, body)
	}
}

func uploadPDF(e *env, fields map[string]string, filename string, data []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if data != nil {
		fw, _ := mw.CreateFormFile("pdf", filename)
		_, _ = fw.Write(data)
	}
	_ = mw.Close()
	return e.do("POST", "/api/add-source", &buf, mw.FormDataContentType())
}

// --- URL sources ------------------------------------------------------

func TestAddSourceURLWritesWorkflowFormat(t *testing.T) {
	f := &fakeFetcher{doc: htmlDoc("https://example.org/final", "The page text.")}
	e := newSourceEnv(t, f)
	rec := e.form("/api/add-source", url.Values{"slug": {"my-piece"}, "kind": {"url"},
		"url": {"https://example.org/start"}, "model": {"openai-fast"}, "depth": {"low"}})
	addOK(t, rec)
	if !strings.Contains(rec.Body.String(), "Extracted Title") {
		t.Fatalf("result = %s", rec.Body.String())
	}
	if f.url != "https://example.org/start" {
		t.Fatalf("fetched %q", f.url)
	}
	got := e.onlySource()
	want := "---\ntype: source\nproject: my-piece\nkind: url\nadded: 2026-11-21T15:04:05Z\n" +
		"title: \"Extracted Title\"\nurl: \"https://example.org/final\"\nmodel_used: openai-fast\ndepth: low\n---\n\n" +
		"# Extracted Title\n\nThe summary.\n\n## Key points\n\n- Point A\n"
	if got != want {
		t.Fatalf("file:\n%s\n---\nwant:\n%s", got, want)
	}
	if filepath.Base(e.sourceFiles()[0]) != "2026-11-21-150405-extracted-title.md" {
		t.Fatalf("name = %s", e.sourceFiles()[0])
	}
	msgs := e.model.lastReq["messages"].([]any)
	user := msgs[1].(map[string]any)["content"].(string)
	if !strings.Contains(user, "URL: https://example.org/final") || !strings.HasSuffix(user, "Content:\nThe page text.") {
		t.Fatalf("model saw %q", user)
	}
	if e.model.lastReq["model"] != "openai-fast" {
		t.Fatalf("model = %v", e.model.lastReq["model"])
	}
}

func TestAddSourceURLTitleOverride(t *testing.T) {
	e := newSourceEnv(t, &fakeFetcher{doc: htmlDoc("https://example.org/a", "text")})
	addOK(t, e.form("/api/add-source", url.Values{"slug": {"my-piece"}, "kind": {"url"},
		"url": {"https://example.org/a"}, "title": {"My \"Own\" Title"}}))
	got := e.onlySource()
	if !strings.Contains(got, `title: "My \"Own\" Title"`) || !strings.Contains(got, "# My \"Own\" Title\n") {
		t.Fatalf("file = %s", got)
	}
	if !strings.Contains(got, "model_used: claude-best\ndepth: medium\n") {
		t.Fatalf("defaults not recorded: %s", got)
	}
}

func TestAddSourceURLPDF(t *testing.T) {
	pdf := []byte("%PDF-1.7 fake pdf bytes")
	e := newSourceEnv(t, &fakeFetcher{doc: pdfDoc("https://example.org/r.pdf", pdf, false)})
	addOK(t, e.form("/api/add-source", url.Values{"slug": {"my-piece"}, "kind": {"url"}, "url": {"https://example.org/r.pdf"}}))
	if !strings.Contains(e.onlySource(), "kind: pdf\n") {
		t.Fatalf("file = %s", e.onlySource())
	}
	parts := e.model.lastReq["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if u := parts[0].(map[string]any)["image_url"].(map[string]any)["url"].(string); !strings.HasPrefix(u, "data:application/pdf;base64,JVBERi0") {
		t.Fatalf("pdf not sent to the model: %.60s", u)
	}
}

func TestAddSourceURLFailures(t *testing.T) {
	cases := map[string]struct {
		f    *fakeFetcher
		want string
	}{
		"fetch error":   {&fakeFetcher{err: errors.New("fetch failed: http: blocked non-public address")}, "blocked non-public address"},
		"empty page":    {&fakeFetcher{doc: htmlDoc("https://x/", "  ")}, "Text tab"},
		"truncated pdf": {&fakeFetcher{doc: pdfDoc("https://x/a.pdf", nil, true)}, "30 MB"},
		"pdf no bytes":  {&fakeFetcher{doc: pdfDoc("https://x/a.pdf", nil, false)}, "PDF"},
	}
	for name, c := range cases {
		e := newSourceEnv(t, c.f)
		rec := e.form("/api/add-source", url.Values{"slug": {"my-piece"}, "kind": {"url"}, "url": {"https://x/"}})
		addErr(t, rec, c.want)
		if name == "fetch error" && !strings.Contains(rec.Body.String(), "Text tab") {
			t.Errorf("fetch error lacks the paste-manually nudge: %s", rec.Body.String())
		}
		if e.model.calls != 0 || len(e.sourceFiles()) != 0 {
			t.Errorf("%s: model calls %d, files %d", name, e.model.calls, len(e.sourceFiles()))
		}
	}
}

func TestAddSourceURLWithoutForager(t *testing.T) {
	e := newSourceEnv(t, nil)
	addErr(t, e.form("/api/add-source", url.Values{"slug": {"my-piece"}, "kind": {"url"}, "url": {"https://x/"}}), "Text tab")
}

func TestAddSourceURLValidation(t *testing.T) {
	f := &fakeFetcher{doc: htmlDoc("https://x/", "t")}
	e := newSourceEnv(t, f)
	for _, u := range []string{"", "ftp://x/", "file:///etc/passwd", "javascript:alert(1)", "https://x/" + strings.Repeat("a", 3000)} {
		addErr(t, e.form("/api/add-source", url.Values{"slug": {"my-piece"}, "kind": {"url"}, "url": {u}}), "URL")
	}
	if f.calls != 0 {
		t.Fatal("fetcher called for an invalid URL")
	}
}

// --- text sources -----------------------------------------------------

func TestAddSourceText(t *testing.T) {
	f := &fakeFetcher{}
	e := newSourceEnv(t, f)
	addOK(t, e.form("/api/add-source", url.Values{"slug": {"my-piece"}, "kind": {"text"},
		"text": {"Pasted & quoted \"text\" 日本語"}, "url": {"https://where.example/it-came-from"}, "model": {"claude-fast"}, "depth": {"high"}}))
	got := e.onlySource()
	for _, want := range []string{"kind: text\n", `url: "https://where.example/it-came-from"`, "model_used: claude-fast\n", "depth: high\ndepth_clamped: true\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if f.calls != 0 {
		t.Fatal("text source went through the fetcher")
	}
	user := e.model.lastReq["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.HasSuffix(user, "Content:\nPasted & quoted \"text\" 日本語") {
		t.Fatalf("model saw %q", user)
	}
}

func TestAddSourceTextValidation(t *testing.T) {
	e := newSourceEnv(t, nil)
	addErr(t, e.form("/api/add-source", url.Values{"slug": {"my-piece"}, "kind": {"text"}, "text": {"  "}}), "Paste")
	addErr(t, e.form("/api/add-source", url.Values{"slug": {"my-piece"}, "kind": {"text"}, "text": {strings.Repeat("x", maxPastedText+1)}}), "too long")
	addErr(t, e.form("/api/add-source", url.Values{"slug": {"my-piece"}, "kind": {"text"}, "text": {"x"}, "url": {"javascript:x"}}), "URL")
	if e.model.calls != 0 {
		t.Fatal("model called for invalid text input")
	}
}

// --- PDF uploads ------------------------------------------------------

func TestAddSourcePDFUpload(t *testing.T) {
	f := &fakeFetcher{}
	e := newSourceEnv(t, f)
	pdf := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte{0xff, 0x00}, 100)...)
	rec := uploadPDF(e, map[string]string{"slug": "my-piece", "kind": "pdf", "title": "Q3 plan", "depth": "high"}, "plan.pdf", pdf)
	addOK(t, rec)
	got := e.onlySource()
	for _, want := range []string{"kind: pdf\n", `title: "Q3 plan"`, `url: ""`, "depth: high\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if f.calls != 0 {
		t.Fatal("an upload went through forager")
	}
	parts := e.model.lastReq["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if !strings.HasPrefix(parts[0].(map[string]any)["image_url"].(map[string]any)["url"].(string), "data:application/pdf;base64,") {
		t.Fatal("upload not sent as a PDF block")
	}
	if text := parts[1].(map[string]any)["text"].(string); !strings.HasPrefix(text, "URL: (no URL — text submitted directly)\nOperator-provided title hint: Q3 plan") {
		t.Fatalf("intro = %q", text)
	}
}

func TestAddSourcePDFUploadRejects(t *testing.T) {
	e := newSourceEnv(t, nil)
	addErr(t, uploadPDF(e, map[string]string{"slug": "my-piece", "kind": "pdf"}, "x.pdf", []byte("<html>not a pdf")), "not a PDF")
	addErr(t, uploadPDF(e, map[string]string{"slug": "my-piece", "kind": "pdf"}, "x.pdf", nil), "Choose a PDF")
	big := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("a"), maxPDFUpload)...)
	rec := uploadPDF(e, map[string]string{"slug": "my-piece", "kind": "pdf"}, "big.pdf", big)
	if !strings.Contains(rec.Body.String(), "30 MB") {
		t.Fatalf("oversize: %d %s", rec.Code, rec.Body.String())
	}
	if e.model.calls != 0 || len(e.sourceFiles()) != 0 {
		t.Fatal("rejected upload reached the model or disk")
	}
}

// --- shared validation ------------------------------------------------

func TestAddSourceSharedValidation(t *testing.T) {
	e := newSourceEnv(t, &fakeFetcher{doc: htmlDoc("https://x/", "t")})
	base := func(kv ...string) url.Values {
		v := url.Values{"slug": {"my-piece"}, "kind": {"text"}, "text": {"x"}}
		for i := 0; i+1 < len(kv); i += 2 {
			v.Set(kv[i], kv[i+1])
		}
		return v
	}
	addErr(t, e.form("/api/add-source", base("model", "gpt-5")), "model")
	addErr(t, e.form("/api/add-source", base("depth", "extreme")), "depth")
	addErr(t, e.form("/api/add-source", base("kind", "video")), "kind")
	addErr(t, e.form("/api/add-source", base("slug", "../etc")), "slug")
	addErr(t, e.form("/api/add-source", base("slug", "no-such-piece")), "No such project")
	addErr(t, e.form("/api/add-source", base("title", strings.Repeat("t", 301))), "Title")
	if e.model.calls != 0 {
		t.Fatal("model called for invalid input")
	}
	if _, err := os.Stat(filepath.Join(e.root, "04_Projects", "no-such-piece")); !os.IsNotExist(err) {
		t.Fatal("unknown project folder was created")
	}
}

func TestAddSourceModelFailure(t *testing.T) {
	e := newSourceEnv(t, nil)
	e.model.reply = "not json at all"
	addErr(t, e.form("/api/add-source", url.Values{"slug": {"my-piece"}, "kind": {"text"}, "text": {"x"}}), "Summarising failed")
	if len(e.sourceFiles()) != 0 {
		t.Fatal("a failed extraction wrote a file")
	}
}

func TestAddSourceRequiresSignIn(t *testing.T) {
	e := newSourceEnv(t, nil)
	req := httptest.NewRequest("POST", "/api/add-source", strings.NewReader("slug=my-piece&kind=text&text=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || e.model.calls != 0 {
		t.Fatalf("got %d, calls %d", rec.Code, e.model.calls)
	}
}

// --- sources reach chat ---------------------------------------------

func TestAddedSourceFeedsChat(t *testing.T) {
	e := newSourceEnv(t, nil)
	addOK(t, e.form("/api/add-source", url.Values{"slug": {"my-piece"}, "kind": {"text"}, "text": {"x"}}))
	e.model.reply = "chat reply"
	e.form("/api/article-chat", url.Values{"slug": {"my-piece"}, "message": {"use the source"}})
	b, _ := json.Marshal(e.model.lastReq)
	if !strings.Contains(string(b), "The summary.") {
		t.Fatal("new source not in the chat context")
	}
}
