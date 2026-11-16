package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Source is one research source attached to a project.
type Source struct {
	Name         string // filename stem; the stable id
	Title        string
	URL          string
	Kind         string // url | text | pdf
	Model        string
	Depth        string
	DepthClamped bool
	AddedAt      time.Time
	Body         string // the extract (Markdown), on Load only
	Excerpt      string
}

// NewSource is what callers supply to AddSource; Body is the composed
// Markdown extract (summary, key points, tables, …).
type NewSource struct {
	Title, URL, Kind, Model, Depth string
	DepthClamped                   bool
	Body                           string
}

var sourceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,79}$`)

// Sources lists a project's sources, newest first.
func (s *Store) Sources(slug string) ([]Source, error) {
	if err := checkSlug(slug); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Root, s.ProjectsDir, slug, "sources")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Source
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".md")
		if e.IsDir() || name == e.Name() || !sourceNameRe.MatchString(name) {
			continue
		}
		src, err := s.Source(slug, name)
		if err != nil {
			continue
		}
		src.Body = ""
		out = append(out, *src)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AddedAt.After(out[j].AddedAt) })
	return out, nil
}

// Source loads one source including its body.
func (s *Store) Source(slug, name string) (*Source, error) {
	if !sourceNameRe.MatchString(name) {
		return nil, errors.New("invalid source name")
	}
	p, err := s.projectFile(slug, "sources", name+".md")
	if err != nil {
		return nil, err
	}
	raw, err := readFile(p)
	if err != nil {
		return nil, err
	}
	fmText, body, _ := splitFrontMatter(raw)
	fm := parseFM(fmText)
	src := &Source{Name: name, Title: fm["title"], URL: fm["url"], Kind: fm["kind"], Model: fm["model_used"],
		Depth: fm["depth"], DepthClamped: fm["depth_clamped"] == "true", Body: strings.TrimSpace(body)}
	if t, err := time.Parse(time.RFC3339, fm["added"]); err == nil {
		src.AddedAt = t
	} else if info, err := os.Stat(p); err == nil {
		src.AddedAt = info.ModTime()
	}
	if src.Title == "" {
		src.Title = name
	}
	for _, line := range strings.Split(src.Body, "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			src.Excerpt = truncateRunes(l, 200)
			break
		}
	}
	return src, nil
}

// AddSource writes a new source file and returns its name.
func (s *Store) AddSource(slug string, ns NewSource) (string, error) {
	kind := ns.Kind
	switch kind {
	case "url", "text", "pdf":
	default:
		return "", fmt.Errorf("source kind must be url, text or pdf")
	}
	title := oneLine(ns.Title)
	if title == "" {
		title = "Untitled source"
	}
	now := s.now()
	name := stamp(now) + "-" + shortSlug(title)
	p, err := s.projectFile(slug, "sources", name+".md")
	if err != nil {
		return "", err
	}
	defer s.lock(slug)()
	lines := []string{"---", "type: source", "project: " + slug, "kind: " + kind, "added: " + now.Format(time.RFC3339),
		"title: " + quote(title)}
	if ns.URL != "" {
		lines = append(lines, "url: "+quote(ns.URL))
	} else {
		lines = append(lines, `url: ""`)
	}
	if ns.Model != "" {
		lines = append(lines, "model_used: "+ns.Model)
	}
	if ns.Depth != "" {
		lines = append(lines, "depth: "+ns.Depth)
	}
	if ns.DepthClamped {
		lines = append(lines, "depth_clamped: true")
	}
	lines = append(lines, "---")
	return name, atomicWrite(p, []byte(strings.Join(lines, "\n")+"\n\n"+strings.TrimSpace(ns.Body)+"\n"))
}

// RemoveSource deletes a source (no-op if already gone).
func (s *Store) RemoveSource(slug, name string) error {
	if !sourceNameRe.MatchString(name) {
		return errors.New("invalid source name")
	}
	p, err := s.projectFile(slug, "sources", name+".md")
	if err != nil {
		return err
	}
	defer s.lock(slug)()
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// shortSlug matches the workflow's 40-char lowercase-hyphen shape.
func shortSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "source"
	}
	return out
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
