package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// extractServer answers with content and records the request body.
func extractServer(t *testing.T, content string) (*Client, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
			"usage":   map[string]any{"prompt_tokens": 50, "completion_tokens": 9},
		})
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "k", "claude-best"), &got
}

const okJSON = `{"title":"T","summary":"S.","key_points":["a"]}`

func TestExtractTextRequestShape(t *testing.T) {
	c, got := extractServer(t, okJSON)
	ex, err := c.ExtractSource(context.Background(), ExtractRequest{
		URL: "https://example.org/a", TitleHint: "Hint", Text: "page body", Depth: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := *got
	if req["model"] != "claude-best" || req["max_tokens"] != float64(2000) || req["temperature"] != 0.1 {
		t.Fatalf("knobs = model %v max %v temp %v", req["model"], req["max_tokens"], req["temperature"])
	}
	if rf, _ := req["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Fatalf("response_format = %v", req["response_format"])
	}
	msgs := req["messages"].([]any)
	sys := msgs[0].(map[string]any)["content"].(string)
	user := msgs[1].(map[string]any)["content"].(string)
	if sys != textExtractPrompt || !strings.Contains(sys, "UNTRUSTED") {
		t.Fatal("text system prompt differs from the workflow's")
	}
	want := "URL: https://example.org/a\nOperator-provided title hint: Hint\n\nContent:\npage body"
	if user != want {
		t.Fatalf("user message:\n%q\nwant\n%q", user, want)
	}
	if ex.Model != "claude-best" || ex.Depth != "low" || ex.DepthClamped || ex.InputTokens != 50 {
		t.Fatalf("result = %+v", ex)
	}
}

func TestExtractNoURLIntro(t *testing.T) {
	c, got := extractServer(t, okJSON)
	if _, err := c.ExtractSource(context.Background(), ExtractRequest{Text: "x", Depth: "medium"}); err != nil {
		t.Fatal(err)
	}
	user := (*got)["messages"].([]any)[1].(map[string]any)["content"].(string)
	if !strings.HasPrefix(user, "URL: (no URL — text submitted directly)\n\nContent:\nx") {
		t.Fatalf("user = %q", user)
	}
}

func TestExtractPDFRequestShape(t *testing.T) {
	c, got := extractServer(t, okJSON)
	pdf := []byte("%PDF-1.7 fake")
	if _, err := c.ExtractSource(context.Background(), ExtractRequest{URL: "https://x/a.pdf", PDF: pdf, Depth: "high"}); err != nil {
		t.Fatal(err)
	}
	msgs := (*got)["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != pdfExtractPrompt {
		t.Fatal("pdf system prompt differs from the workflow's")
	}
	parts := msgs[1].(map[string]any)["content"].([]any)
	img := parts[0].(map[string]any)
	url := img["image_url"].(map[string]any)["url"].(string)
	if img["type"] != "image_url" || url != "data:application/pdf;base64,"+base64.StdEncoding.EncodeToString(pdf) {
		t.Fatalf("pdf part = %v", img)
	}
	text := parts[1].(map[string]any)["text"].(string)
	if text != "URL: https://x/a.pdf\n\nExtract metadata from this PDF. Be especially thorough on any tables and charts." {
		t.Fatalf("pdf text part = %q", text)
	}
	if (*got)["max_tokens"] != float64(16000) {
		t.Fatalf("high depth max_tokens = %v", (*got)["max_tokens"])
	}
}

func TestExtractDepthClampForClaudeFast(t *testing.T) {
	c, got := extractServer(t, okJSON)
	c.Model = "claude-fast"
	ex, err := c.ExtractSource(context.Background(), ExtractRequest{Text: "x", Depth: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if (*got)["max_tokens"] != float64(8000) || !ex.DepthClamped || ex.Depth != "high" {
		t.Fatalf("max_tokens %v clamped %v depth %q", (*got)["max_tokens"], ex.DepthClamped, ex.Depth)
	}
}

func TestExtractBadDepth(t *testing.T) {
	c, _ := extractServer(t, okJSON)
	if _, err := c.ExtractSource(context.Background(), ExtractRequest{Text: "x", Depth: "huge"}); err == nil {
		t.Fatal("bad depth accepted")
	}
	if _, err := c.ExtractSource(context.Background(), ExtractRequest{Depth: "low"}); err == nil {
		t.Fatal("empty input accepted")
	}
}

func TestExtractParsesTablesChartsLeniently(t *testing.T) {
	content := "```json\n" + `{"title":" T ","summary":" S ","key_points":[" a ","",3],
	 "tables":[{"caption":"Cap","rows":[["h1","h2"],[1,null],[]]}],
	 "charts":[{"caption":"C","description":"D","data":[["x",2.5],["y"]]}]}` + "\n```"
	c, _ := extractServer(t, content)
	ex, err := c.ExtractSource(context.Background(), ExtractRequest{Text: "x", Depth: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Title != "T" || ex.Summary != "S" {
		t.Fatalf("title/summary = %q %q", ex.Title, ex.Summary)
	}
	if strings.Join(ex.KeyPoints, "|") != "a|3" {
		t.Fatalf("key points = %q", ex.KeyPoints)
	}
	if len(ex.Tables) != 1 || ex.Tables[0].Caption != "Cap" ||
		len(ex.Tables[0].Rows) != 3 || ex.Tables[0].Rows[1][0] != "1" || ex.Tables[0].Rows[1][1] != "" {
		t.Fatalf("tables = %+v", ex.Tables)
	}
	if len(ex.Charts) != 1 || ex.Charts[0].Data[0][1] != "2.5" || len(ex.Charts[0].Data[1]) != 1 {
		t.Fatalf("charts = %+v", ex.Charts)
	}
}

func TestExtractCapsLists(t *testing.T) {
	var pts []string
	for i := 0; i < 30; i++ {
		pts = append(pts, `"p"`)
	}
	tbl := strings.Repeat(`{"rows":[["a"]]},`, 25)
	content := `{"title":"T","summary":"S","key_points":[` + strings.Join(pts, ",") + `],"tables":[` + strings.TrimSuffix(tbl, ",") + `]}`
	c, _ := extractServer(t, content)
	ex, err := c.ExtractSource(context.Background(), ExtractRequest{Text: "x", Depth: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.KeyPoints) != 15 || len(ex.Tables) != 20 {
		t.Fatalf("caps: %d points, %d tables", len(ex.KeyPoints), len(ex.Tables))
	}
}

func TestExtractErrors(t *testing.T) {
	for name, content := range map[string]string{
		"not json":      "Sure! Here is the summary.",
		"empty summary": `{"title":"T","summary":"  "}`,
	} {
		c, _ := extractServer(t, content)
		if _, err := c.ExtractSource(context.Background(), ExtractRequest{Text: "x", Depth: "low"}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestExtractTruncatesLongText(t *testing.T) {
	c, got := extractServer(t, okJSON)
	long := strings.Repeat("é", MaxExtractText) // 2 bytes each
	if _, err := c.ExtractSource(context.Background(), ExtractRequest{Text: long, Depth: "low"}); err != nil {
		t.Fatal(err)
	}
	user := (*got)["messages"].([]any)[1].(map[string]any)["content"].(string)
	body := strings.TrimPrefix(user, "URL: (no URL — text submitted directly)\n\nContent:\n")
	if len(body) > MaxExtractText || !strings.HasPrefix(long, body) {
		t.Fatalf("content not cut on a rune boundary within %d bytes (got %d)", MaxExtractText, len(body))
	}
}
