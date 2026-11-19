// Package render turns Markdown into safe HTML for the editor.
//
// We use goldmark in its default configuration plus the GFM table /
// strikethrough / task-list extensions. Raw HTML inside Markdown is
// NOT enabled, so any <script>, <iframe>, or onclick attributes that
// might appear in project content are escaped rather than rendered.
package render

import (
	"bytes"
	"html/template"
	"net/url"
	"regexp"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

// engine is the configured goldmark instance. Constructed once at
// package init so requests share it (it's safe for concurrent use).
var engine = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM, // tables, strikethrough, task lists, autolinks
	),
	goldmark.WithParserOptions(
		parser.WithAutoHeadingID(),
	),
	goldmark.WithRendererOptions(
		// Deliberately NOT html.WithUnsafe(): raw HTML in project Markdown
		// must be escaped. The LLM-generated summaries are the most
		// likely place where weird content could appear.
		html.WithHardWraps(),
	),
)

// Markdown renders a Markdown source string to HTML safe to include in
// a Go html/template via {{ . }} unescaped (use template.HTML or the
// trustedHTML pipeline function defined in the templates).
//
// Returns template.HTML so the calling template can render it as-is.
// goldmark has already escaped any raw HTML in the source.
func Markdown(src string) (template.HTML, error) {
	var buf bytes.Buffer
	if err := engine.Convert([]byte(src), &buf); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

// attachmentSrcPattern matches `<img ... src="attachments/<name>">` in
// the rendered HTML so we can rewrite it to point at the writer's
// /api/attachment endpoint with the article slug pinned. Restricted to
// the exact filename shape store.SaveAttachment produces
// (matches store.attachmentNameRe) so a pathological Markdown source
// can't sneak an arbitrary relative URL through the rewrite.
var attachmentSrcPattern = regexp.MustCompile(
	`(<img\b[^>]*\bsrc=")attachments/([0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9]{6}-[a-f0-9]{8}\.(?:png|jpg|jpeg|webp|gif))(")`,
)

// MarkdownForArticle renders like Markdown but ALSO rewrites relative
// `attachments/<name>` image sources into writer URLs
// `/api/attachment?slug=<slug>&name=<name>` so the preview renders
// pasted images correctly from any context. The Markdown source on
// disk keeps the clean relative path — the rewrite only touches the
// rendered HTML.
//
// The rewrite applies only to filenames matching the strict shape
// produced by the attachment upload flow, so a hand-authored
// `![](attachments/../../secret.md)` or similar wouldn't be
// mistakenly rewritten (it would render as-is and 404 on load).
func MarkdownForArticle(src, slug string) (template.HTML, error) {
	var buf bytes.Buffer
	if err := engine.Convert([]byte(src), &buf); err != nil {
		return "", err
	}
	rewritten := attachmentSrcPattern.ReplaceAllStringFunc(buf.String(), func(match string) string {
		parts := attachmentSrcPattern.FindStringSubmatch(match)
		if len(parts) != 4 {
			return match // shouldn't happen — regex has 3 groups
		}
		q := url.Values{}
		q.Set("slug", slug)
		q.Set("name", parts[2])
		return parts[1] + "/api/attachment?" + q.Encode() + parts[3]
	})
	return template.HTML(rewritten), nil
}
