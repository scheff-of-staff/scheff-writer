package render

import (
	"strings"
	"testing"
)

func TestMarkdownEscapesRawHTML(t *testing.T) {
	out, err := Markdown("hi <script>alert(1)</script> <img src=x onerror=y>")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "<script>") || strings.Contains(string(out), "<img src=x") {
		t.Fatalf("raw HTML rendered: %s", out)
	}
}

func TestMarkdownForArticleRewritesOnlyStrictAttachmentNames(t *testing.T) {
	src := "![](attachments/2026-01-01-120000-abcdef01.png)\n\n![](attachments/../../secret.md)"
	out, err := MarkdownForArticle(src, "my-piece")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `src="/api/attachment?name=2026-01-01-120000-abcdef01.png&slug=my-piece"`) {
		t.Fatalf("valid attachment not rewritten: %s", s)
	}
	if strings.Contains(s, "/api/attachment?name=..") {
		t.Fatalf("traversal name rewritten: %s", s)
	}
}
