package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const projectFixture = `---
created: 2026-11-01T00:00:00Z
type: project
status: active
kind: research
custom_key: "kept"
---

# My Article

Old body paragraph.

## Log

- **2026-11-01** — capture appended by another package
`

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "04_Projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "04_Projects", "my-article.md"), []byte(projectFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &clock{time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)}
	s := New(root)
	s.Now = c.now
	return s, c
}

func read(t *testing.T, s *Store, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoad(t *testing.T) {
	s, _ := newStore(t)
	p, err := s.Load("my-article")
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "My Article" || p.Status != "active" || p.FrontMatter["custom_key"] != "kept" {
		t.Fatalf("project = %+v", p)
	}
	if p.Editable != "# My Article\n\nOld body paragraph." || !strings.HasPrefix(p.Log, "## Log") {
		t.Fatalf("split = %q / %q", p.Editable, p.Log)
	}
	if _, err := s.Load("nope"); err != ErrNotFound {
		t.Fatalf("missing project err = %v", err)
	}
}

func TestSaveBody_PreservesLogAndFrontMatter_WritesRevision(t *testing.T) {
	s, _ := newStore(t)
	res, err := s.SaveBody("my-article", "# My Article\n\nNew text here.\n\n\n", "auto")
	if err != nil {
		t.Fatal(err)
	}
	got := read(t, s, "04_Projects/my-article.md")
	want := strings.Replace(projectFixture, "Old body paragraph.", "New text here.", 1)
	if got != want {
		t.Fatalf("file mismatch:\n--- got\n%s\n--- want\n%s", got, want)
	}
	if !res.RevisionWritten || res.RevisionName != "2026-11-15-120000" || res.WordCount != 5 {
		t.Fatalf("result = %+v", res)
	}
	rev := read(t, s, "04_Projects/my-article/revisions/2026-11-15-120000.md")
	wantRev := "---\nsaved_at: 2026-11-15T12:00:00Z\nword_count: 5\nsource: auto\nbody_bytes: 28\n---\n\n# My Article\n\nNew text here.\n"
	if rev != wantRev {
		t.Fatalf("revision:\n%q\nwant\n%q", rev, wantRev)
	}
}

func TestSaveBody_RevisionDedupe(t *testing.T) {
	s, c := newStore(t)
	if _, err := s.SaveBody("my-article", "same text", "auto"); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(20 * time.Second)
	res, _ := s.SaveBody("my-article", "same text  ", "auto") // trailing space trimmed → same bytes
	if res.RevisionWritten {
		t.Fatal("unchanged re-save within a minute must not snapshot")
	}
	c.t = c.t.Add(time.Second)
	res, _ = s.SaveBody("my-article", "same text, edited", "manual")
	if !res.RevisionWritten {
		t.Fatal("a changed body must snapshot")
	}
	revs, _ := s.Revisions("my-article")
	if len(revs) != 2 || revs[0].Source != "manual" || revs[0].WordCount != 3 {
		t.Fatalf("revisions = %+v", revs)
	}
	body, err := s.Revision("my-article", revs[1].Name)
	if err != nil || body != "same text" {
		t.Fatalf("revision body = %q %v", body, err)
	}
	if _, err := s.Revision("my-article", "../../x"); err == nil {
		t.Fatal("bad revision name must be refused")
	}
}

func TestSaveBody_NoLogSection(t *testing.T) {
	s, _ := newStore(t)
	_ = os.WriteFile(filepath.Join(s.Root, "04_Projects", "plain.md"), []byte("---\ntype: project\n---\n\nOld\n"), 0o644)
	if _, err := s.SaveBody("plain", "New", "manual"); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, "04_Projects/plain.md"); got != "---\ntype: project\n---\n\nNew\n" {
		t.Fatalf("got %q", got)
	}
}

func TestSaveBody_Limits(t *testing.T) {
	s, _ := newStore(t)
	if _, err := s.SaveBody("my-article", strings.Repeat("x", MaxBody+1), "auto"); err == nil {
		t.Fatal("oversize body must be refused")
	}
	if _, err := s.SaveBody("../escape", "x", "auto"); err == nil {
		t.Fatal("traversal slug must be refused")
	}
}

func TestCreateAndStatus(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Create("new-piece", "A New Piece"); err != nil {
		t.Fatal(err)
	}
	if err := s.Create("new-piece", "again"); err == nil {
		t.Fatal("duplicate create must fail")
	}
	p, _ := s.Load("new-piece")
	if p.Title != "A New Piece" || p.Status != "active" || !strings.HasPrefix(p.Log, "## Log") {
		t.Fatalf("created = %+v", p)
	}
	if err := s.SetStatus("new-piece", "Paused"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Load("new-piece"); p.Status != "paused" {
		t.Fatalf("status = %s", p.Status)
	}
	if err := s.SetStatus("new-piece", "deleted"); err == nil {
		t.Fatal("invalid status must be refused")
	}
	list, _ := s.List()
	if len(list) != 2 {
		t.Fatalf("list = %+v", list)
	}
}

func TestChat(t *testing.T) {
	s, c := newStore(t)
	if turns, err := s.Chat("my-article"); err != nil || turns != nil {
		t.Fatalf("empty chat = %v %v", turns, err)
	}
	if err := s.AppendChat("my-article", "Tighten the intro?", "Here's a tighter intro.\n\n## User not a header"); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Minute)
	_ = s.AppendChat("my-article", "Thanks", "Welcome")
	raw := read(t, s, "04_Projects/my-article/chat.md")
	if !strings.HasPrefix(raw, "---\ntype: chat\nproject: my-article\ncreated: 2026-11-15T12:00:00Z\n---\n\n## User 2026-11-15T12:00:00Z\n\nTighten the intro?\n") {
		t.Fatalf("chat file:\n%s", raw)
	}
	turns, _ := s.Chat("my-article")
	if len(turns) != 4 || turns[1].Role != "assistant" || !strings.Contains(turns[1].Content, "## User not a header") || turns[3].Content != "Welcome" {
		t.Fatalf("turns = %+v", turns)
	}
	if err := s.AppendChat("my-article", "", "x"); err == nil {
		t.Fatal("empty message must be refused")
	}
	if err := s.ResetChat("my-article"); err != nil {
		t.Fatal(err)
	}
	if turns, _ := s.Chat("my-article"); turns != nil {
		t.Fatal("reset must clear the thread")
	}
	if err := s.ResetChat("my-article"); err != nil {
		t.Fatal("reset on missing chat must be a no-op")
	}
}

func TestSources(t *testing.T) {
	s, c := newStore(t)
	name, err := s.AddSource("my-article", NewSource{Title: `Zero "Trust" — Field Guide`, URL: "https://ex.example/a", Kind: "url", Model: "claude-best", Depth: "medium",
		Body: "Summary line.\n\n## Key points\n\n- one"})
	if err != nil {
		t.Fatal(err)
	}
	if name != "2026-11-15-120000-zero-trust-field-guide" {
		t.Fatalf("name = %s", name)
	}
	raw := read(t, s, "04_Projects/my-article/sources/"+name+".md")
	want := "---\ntype: source\nproject: my-article\nkind: url\nadded: 2026-11-15T12:00:00Z\ntitle: \"Zero \\\"Trust\\\" — Field Guide\"\nurl: \"https://ex.example/a\"\nmodel_used: claude-best\ndepth: medium\n---\n\nSummary line.\n\n## Key points\n\n- one\n"
	if raw != want {
		t.Fatalf("source file:\n%q\nwant\n%q", raw, want)
	}
	c.t = c.t.Add(time.Second)
	_, _ = s.AddSource("my-article", NewSource{Title: "Pasted", Kind: "text", Body: "text", DepthClamped: true})
	list, _ := s.Sources("my-article")
	if len(list) != 2 || list[0].Title != "Pasted" || !list[0].DepthClamped || list[1].URL != "https://ex.example/a" || list[1].Excerpt != "Summary line." {
		t.Fatalf("sources = %+v", list)
	}
	if _, err := s.AddSource("my-article", NewSource{Title: "x", Kind: "exe"}); err == nil {
		t.Fatal("unknown kind must be refused")
	}
	if err := s.RemoveSource("my-article", "../../my-article"); err == nil {
		t.Fatal("traversal name must be refused")
	}
	if err := s.RemoveSource("my-article", name); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Sources("my-article"); len(list) != 1 {
		t.Fatalf("after remove = %+v", list)
	}
}

func TestAttachments(t *testing.T) {
	s, _ := newStore(t)
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, []byte("rest")...)
	name, err := s.SaveAttachment("my-article", "image/png", png)
	if err != nil {
		t.Fatal(err)
	}
	if !attachmentNameRe.MatchString(name) {
		t.Fatalf("name = %s", name)
	}
	data, ct, err := s.Attachment("my-article", name)
	if err != nil || ct != "image/png" || string(data) != string(png) {
		t.Fatalf("load = %v %s", err, ct)
	}
	if _, err := s.SaveAttachment("my-article", "image/png", []byte("#!/bin/sh\necho pwned")); err == nil {
		t.Fatal("a script claiming to be png must be refused")
	}
	if _, err := s.SaveAttachment("my-article", "text/html", png); err == nil {
		t.Fatal("non-image mime must be refused")
	}
	if _, _, err := s.Attachment("my-article", "../my-article.md"); err == nil {
		t.Fatal("traversal attachment name must be refused")
	}
}

func TestReference(t *testing.T) {
	s, _ := newStore(t)
	if p, err := s.Profile(); err != nil || p != "" {
		t.Fatalf("missing profile = %q %v", p, err)
	}
	ref := filepath.Join(s.Root, "06_Reference")
	_ = os.MkdirAll(filepath.Join(ref, "tone"), 0o755)
	_ = os.WriteFile(filepath.Join(ref, "profile.md"), []byte("---\ntype: reference\n---\n\n# Profile\n\nWho I am.\n"), 0o644)
	_ = os.WriteFile(filepath.Join(ref, "tone", "README.md"), []byte("docs"), 0o644)
	_ = os.WriteFile(filepath.Join(ref, "tone", "a.md"), []byte("Sample A"), 0o644)
	p, _ := s.Profile()
	samples, _ := s.ToneSamples(5)
	if p != "# Profile\n\nWho I am." || len(samples) != 1 || samples[0] != "Sample A" {
		t.Fatalf("profile=%q samples=%v", p, samples)
	}
}
