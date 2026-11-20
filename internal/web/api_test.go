package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/scheff-of-staff/scheff-writer/internal/llm"
	"github.com/scheff-of-staff/scheff-writer/internal/store"
)

const testAPIToken = "api-token-fedcba9876543210"

const chatPath = "/api/v1/projects/my-piece/chat"

// newAPIEnv is newEnv with the JSON API turned on. mod may adjust the
// config before the server is built.
func newAPIEnv(t *testing.T, mod func(*Config)) *env {
	t.Helper()
	root := t.TempDir()
	st := store.New(root)
	if err := st.Create("my-piece", "My Piece"); err != nil {
		t.Fatal(err)
	}
	fm := &fakeModel{reply: "A reply."}
	srv := httptest.NewServer(fm)
	t.Cleanup(srv.Close)
	cfg := Config{
		Store:     st,
		LLM:       llm.New(srv.URL, "k", "test-model"),
		Token:     testToken,
		APIToken:  testAPIToken,
		APIModels: []string{"test-model", "other-model"},
	}
	if mod != nil {
		mod(&cfg)
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, root: root, st: st, h: h, model: fm}
}

// api sends a request the way an automation client would: bearer API
// token, no browser fetch metadata.
func (e *env) api(target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest("POST", target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

type apiReply struct {
	Reply string `json:"reply"`
	Model string `json:"model"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	ElapsedMS    int64  `json:"elapsed_ms"`
	Persisted    bool   `json:"persisted"`
	HistoryTurns int    `json:"history_turns"`
	Error        string `json:"error"`
}

func decodeAPI(t *testing.T, rec *httptest.ResponseRecorder) apiReply {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content type %q, body %s", ct, rec.Body.String())
	}
	var out apiReply
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func lastUser(t *testing.T, req map[string]any) string {
	t.Helper()
	msgs, _ := req["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages sent")
	}
	m, _ := msgs[len(msgs)-1].(map[string]any)
	s, _ := m["content"].(string)
	return s
}

// --- auth and gating ----------------------------------------------

func TestAPIOffWithoutToken(t *testing.T) {
	e := newAPIEnv(t, func(c *Config) { c.APIToken = "" })
	// With no API token configured, /api/v1/ falls under the normal
	// sign-in rules: an unknown bearer is 401, as on any other route.
	rec := e.api(chatPath, `{"message":"hi"}`, nil)
	if rec.Code != http.StatusUnauthorized || e.model.calls != 0 {
		t.Fatalf("got %d calls=%d", rec.Code, e.model.calls)
	}
	// The sign-in token doesn't switch it on either.
	rec = e.api(chatPath, `{"message":"hi"}`, map[string]string{"Authorization": "Bearer " + testToken})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("sign-in token: got %d", rec.Code)
	}
}

func TestAPIAuth(t *testing.T) {
	e := newAPIEnv(t, nil)
	cases := map[string]map[string]string{
		"no token":       {"Authorization": ""},
		"wrong token":    {"Authorization": "Bearer nope-nope-nope-nope"},
		"sign-in token":  {"Authorization": "Bearer " + testToken},
		"not bearer":     {"Authorization": "Basic " + testAPIToken},
		"session cookie": {"Authorization": "", "Cookie": sessionCookie + "=" + newAuth(testToken, true).session},
	}
	for name, hdr := range cases {
		rec := e.api(chatPath, `{"message":"hi"}`, hdr)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d", name, rec.Code)
			continue
		}
		if decodeAPI(t, rec).Error == "" {
			t.Errorf("%s: no JSON error", name)
		}
	}
	if e.model.calls != 0 {
		t.Fatal("model called without valid API token")
	}
}

func TestAPITokenDoesNotOpenTheUI(t *testing.T) {
	e := newAPIEnv(t, nil)
	rec := e.api("/api/article-chat", url.Values{"slug": {"my-piece"}, "message": {"hi"}}.Encode(),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("API token on editor route: got %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIToken)
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	if rr.Code == http.StatusOK {
		t.Fatal("API token opened the projects index")
	}
}

func TestAPITokenConfigValidation(t *testing.T) {
	st := store.New(t.TempDir())
	for name, tok := range map[string]string{"short": "too-short", "same as sign-in": testToken} {
		_, err := New(Config{Store: st, Token: testToken, APIToken: tok})
		if err == nil {
			t.Errorf("%s API token accepted", name)
		}
	}
}

func TestAPICrossSiteBrowserBlocked(t *testing.T) {
	e := newAPIEnv(t, nil)
	rec := e.api(chatPath, `{"message":"hi"}`, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rec.Code != http.StatusForbidden || e.model.calls != 0 {
		t.Fatalf("got %d calls=%d", rec.Code, e.model.calls)
	}
}

// --- happy path --------------------------------------------------------

func TestAPIChat(t *testing.T) {
	e := newAPIEnv(t, nil)
	e.model.reply = "Try **ferns**."
	rec := e.api(chatPath, `{"message":"What next?","draft":"My draft."}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	out := decodeAPI(t, rec)
	if out.Reply != "Try **ferns**." || out.Model != "test-model" || !out.Persisted {
		t.Fatalf("reply = %+v", out)
	}
	if out.Usage.InputTokens != 100 || out.Usage.OutputTokens != 20 {
		t.Errorf("usage = %+v", out.Usage)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("missing Cache-Control: no-store")
	}
	if got := lastUser(t, e.model.lastReq); got != "What next?" {
		t.Errorf("model saw %q", got)
	}
	turns, _ := e.st.Chat("my-piece")
	if len(turns) != 2 || turns[0].Content != "What next?" || turns[1].Content != "Try **ferns**." {
		t.Fatalf("turns = %+v", turns)
	}
}

func TestAPIAndEditorShareHistory(t *testing.T) {
	e := newAPIEnv(t, nil)
	e.form("/api/article-chat", url.Values{"slug": {"my-piece"}, "message": {"From the editor"}})
	rec := e.api(chatPath, `{"message":"From the API"}`, nil)
	if out := decodeAPI(t, rec); out.HistoryTurns != 2 {
		t.Fatalf("history turns = %d", out.HistoryTurns)
	}
	if msgs, _ := e.model.lastReq["messages"].([]any); len(msgs) != 4 {
		t.Fatalf("messages sent = %d, want system + 2 history + new", len(msgs))
	}
}

// --- side-effect controls ----------------------------------------

func TestAPIPersistFalse(t *testing.T) {
	e := newAPIEnv(t, nil)
	rec := e.api(chatPath, `{"message":"probe","persist":false}`, nil)
	out := decodeAPI(t, rec)
	if rec.Code != http.StatusOK || out.Persisted {
		t.Fatalf("got %d %+v", rec.Code, out)
	}
	if turns, _ := e.st.Chat("my-piece"); len(turns) != 0 {
		t.Fatalf("persist:false wrote %d turns", len(turns))
	}
}

func TestAPIHistoryFalse(t *testing.T) {
	e := newAPIEnv(t, nil)
	if err := e.st.AppendChat("my-piece", "earlier question", "earlier answer"); err != nil {
		t.Fatal(err)
	}
	rec := e.api(chatPath, `{"message":"fresh","history":false,"persist":false}`, nil)
	if out := decodeAPI(t, rec); out.HistoryTurns != 0 {
		t.Fatalf("history turns = %d", out.HistoryTurns)
	}
	msgs, _ := e.model.lastReq["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages sent = %d, want system + new", len(msgs))
	}
	if strings.Contains(mustJSON(t, e.model.lastReq), "earlier question") {
		t.Fatal("history leaked into a history:false request")
	}
}

// --- model selection ----------------------------------------------

func TestAPIModelAllowed(t *testing.T) {
	e := newAPIEnv(t, nil)
	rec := e.api(chatPath, `{"message":"hi","model":"other-model"}`, nil)
	if out := decodeAPI(t, rec); rec.Code != http.StatusOK || out.Model != "other-model" {
		t.Fatalf("got %d %+v", rec.Code, out)
	}
	if e.model.lastReq["model"] != "other-model" {
		t.Fatalf("model sent = %v", e.model.lastReq["model"])
	}
	// The override must not stick to the shared client.
	e.form("/api/article-chat", url.Values{"slug": {"my-piece"}, "message": {"editor"}})
	if e.model.lastReq["model"] != "test-model" {
		t.Fatalf("override leaked into the editor: %v", e.model.lastReq["model"])
	}
}

func TestAPIModelRejected(t *testing.T) {
	e := newAPIEnv(t, nil)
	for _, m := range []string{"gpt-anything", "", "test-model "} {
		rec := e.api(chatPath, mustJSON(t, map[string]any{"message": "hi", "model": m}), nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("model %q: got %d", m, rec.Code)
		}
	}
	if e.model.calls != 0 {
		t.Fatal("model called for a rejected alias")
	}
}

func TestAPIModelsDefaultToConfiguredModel(t *testing.T) {
	e := newAPIEnv(t, func(c *Config) { c.APIModels = nil })
	if rec := e.api(chatPath, `{"message":"hi","model":"test-model"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("default model rejected: %d", rec.Code)
	}
	if rec := e.api(chatPath, `{"message":"hi","model":"other-model"}`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("unlisted model accepted: %d", rec.Code)
	}
}

// --- invalid input ----------------------------------------------------

func TestAPIBadInput(t *testing.T) {
	e := newAPIEnv(t, nil)
	cases := map[string]struct {
		path, body string
		want       int
	}{
		"missing message":    {chatPath, `{}`, 400},
		"blank message":      {chatPath, `{"message":" \n "}`, 400},
		"message wrong type": {chatPath, `{"message":["hi"]}`, 400},
		"persist wrong type": {chatPath, `{"message":"hi","persist":"no"}`, 400},
		"truncated":          {chatPath, `{"message":"hi"`, 400},
		"not json":           {chatPath, `message=hi`, 400},
		"array":              {chatPath, `[{"message":"hi"}]`, 400},
		"empty body":         {chatPath, ``, 400},
		"trailing data":      {chatPath, `{"message":"hi"}{}`, 400},
		"unknown field":      {chatPath, `{"message":"hi","attachments":"x.png"}`, 400},
		"bad slug":           {"/api/v1/projects/Bad_Slug/chat", `{"message":"hi"}`, 400},
		"unknown project":    {"/api/v1/projects/no-such-piece/chat", `{"message":"hi"}`, 404},
		"message too long":   {chatPath, mustJSON(t, map[string]string{"message": strings.Repeat("x", maxChatMessage+1)}), 400},
	}
	for name, c := range cases {
		rec := e.api(c.path, c.body, nil)
		if rec.Code != c.want {
			t.Errorf("%s: got %d want %d (%s)", name, rec.Code, c.want, rec.Body.String())
			continue
		}
		if decodeAPI(t, rec).Error == "" {
			t.Errorf("%s: no JSON error body", name)
		}
	}
	if e.model.calls != 0 {
		t.Fatal("model called for invalid input")
	}
	if _, err := e.st.Load("no-such-piece"); err == nil {
		t.Fatal("unknown project was created")
	}
}

func TestAPIContentType(t *testing.T) {
	e := newAPIEnv(t, nil)
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
		rec := e.api(chatPath, `{"message":"hi"}`, map[string]string{"Content-Type": ct})
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%q: got %d", ct, rec.Code)
		}
	}
	rec := e.api(chatPath, `{"message":"hi"}`, map[string]string{"Content-Type": "application/json; charset=utf-8"})
	if rec.Code != http.StatusOK {
		t.Errorf("charset parameter: got %d", rec.Code)
	}
}

func TestAPIBodyTooLarge(t *testing.T) {
	e := newAPIEnv(t, nil)
	body := mustJSON(t, map[string]string{"message": "hi", "draft": strings.Repeat("a", maxAPIBody)})
	if rec := e.api(chatPath, body, nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", rec.Code)
	}
	if e.model.calls != 0 {
		t.Fatal("model called for an oversize body")
	}
}

// --- text handling ------------------------------------------------

const trickyText = "a & b = c % d + e \"double\" 'single' <script>x</script> \\ / ? #\n" +
	"second line\r\ntab\there · café · 日本語 · العربية · 🔥🧪 · ​ zero-width"

func TestAPISpecialCharactersRoundTrip(t *testing.T) {
	e := newAPIEnv(t, nil)
	e.model.reply = trickyText
	rec := e.api(chatPath, mustJSON(t, map[string]string{"message": trickyText, "draft": trickyText}), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if got := lastUser(t, e.model.lastReq); got != strings.TrimSpace(trickyText) {
		t.Errorf("model saw %q", got)
	}
	if got := decodeAPI(t, rec).Reply; got != trickyText {
		t.Errorf("reply = %q", got)
	}
}

// --- model failure and concurrency ------------------------------

func TestAPIModelFailureIs502(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream secret detail", http.StatusInternalServerError)
	}))
	t.Cleanup(down.Close)
	e := newAPIEnv(t, func(c *Config) { c.LLM = llm.New(down.URL, "k", "test-model") })
	rec := e.api(chatPath, `{"message":"hi"}`, nil)
	out := decodeAPI(t, rec)
	if rec.Code != http.StatusBadGateway || out.Error == "" {
		t.Fatalf("got %d %+v", rec.Code, out)
	}
	if strings.Contains(rec.Body.String(), "upstream secret detail") {
		t.Fatal("upstream body leaked to the client")
	}
	if turns, _ := e.st.Chat("my-piece"); len(turns) != 0 {
		t.Fatal("failed turn was persisted")
	}
}

func TestAPIConcurrencyLimit(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{}}`))
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	e := newAPIEnv(t, func(c *Config) {
		c.LLM = llm.New(slow.URL, "k", "test-model")
		c.APIMaxConcurrent = 1
	})

	done := make(chan int, 1)
	go func() {
		done <- e.api(chatPath, `{"message":"first","persist":false}`, nil).Code
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the model")
	}
	rec := e.api(chatPath, `{"message":"second","persist":false}`, nil)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second request: got %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	release <- struct{}{}
	if code := <-done; code != http.StatusOK {
		t.Fatalf("first request: got %d", code)
	}
}
