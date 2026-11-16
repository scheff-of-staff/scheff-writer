package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MaxBody caps the editable body (matches the workflow it replaces).
const MaxBody = 256 * 1024

// ProjectStatuses are the coarse project states (contracts §3).
var ProjectStatuses = map[string]bool{"active": true, "done": true, "paused": true, "archived": true}

// Project is a parsed project file.
type Project struct {
	Slug        string
	Title       string
	Status      string
	FrontMatter map[string]string
	// Editable is the operator-owned body: everything above the first
	// line starting "## Log". Log is that section onward, preserved
	// verbatim on save (other packages append to it).
	Editable string
	Log      string
}

var logHeadingRe = regexp.MustCompile(`(?m)^## Log\b`)

// splitBody divides a project body at the first "## Log" line.
func splitBody(body string) (editable, log string) {
	if loc := logHeadingRe.FindStringIndex(body); loc != nil {
		return strings.TrimSpace(body[:loc[0]]), body[loc[0]:]
	}
	return strings.TrimSpace(body), ""
}

// Load reads a project.
func (s *Store) Load(slug string) (*Project, error) {
	path, err := s.masterPath(slug)
	if err != nil {
		return nil, err
	}
	raw, err := readFile(path)
	if err != nil {
		return nil, err
	}
	fmText, body, ok := splitFrontMatter(raw)
	if !ok {
		return nil, fmt.Errorf("project %s has no front matter", slug)
	}
	fm := parseFM(fmText)
	p := &Project{Slug: slug, FrontMatter: fm, Status: strings.ToLower(fm["status"])}
	if p.Status == "" {
		p.Status = "active"
	}
	p.Editable, p.Log = splitBody(body)
	p.Title = fm["title"]
	for _, line := range strings.Split(p.Editable, "\n") {
		if strings.HasPrefix(line, "# ") {
			p.Title = strings.TrimSpace(line[2:])
			break
		}
	}
	if p.Title == "" {
		p.Title = slug
	}
	return p, nil
}

// ProjectMeta is the listing view.
type ProjectMeta struct {
	Slug, Title, Status, Kind string
	ModTime                   time.Time
}

// List returns every project, most recently modified first.
func (s *Store) List() ([]ProjectMeta, error) {
	dir := filepath.Join(s.Root, s.ProjectsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []ProjectMeta
	for _, e := range entries {
		slug := strings.TrimSuffix(e.Name(), ".md")
		if e.IsDir() || slug == e.Name() || !SlugRe.MatchString(slug) {
			continue
		}
		p, err := s.Load(slug)
		if err != nil {
			continue
		}
		info, _ := e.Info()
		m := ProjectMeta{Slug: slug, Title: p.Title, Status: p.Status, Kind: p.FrontMatter["kind"]}
		if info != nil {
			m.ModTime = info.ModTime()
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// Create makes a new project file if the slug is free.
func (s *Store) Create(slug, title string) error {
	path, err := s.masterPath(slug)
	if err != nil {
		return err
	}
	defer s.lock(slug)()
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("project %s already exists", slug)
	}
	now := s.now()
	if strings.TrimSpace(title) == "" {
		title = slug
	}
	content := "---\ncreated: " + now.Format(time.RFC3339) + "\ntype: project\nstatus: active\nstarted: " +
		now.Format("2006-01-02") + "\ntags:\n  - project\n---\n\n# " + oneLine(title) + "\n\n## Log\n\n"
	return atomicWrite(path, []byte(content))
}

// SetStatus rewrites the coarse status.
func (s *Store) SetStatus(slug, status string) error {
	status = strings.ToLower(strings.TrimSpace(status))
	if !ProjectStatuses[status] {
		return fmt.Errorf("status must be one of active|done|paused|archived")
	}
	path, err := s.masterPath(slug)
	if err != nil {
		return err
	}
	defer s.lock(slug)()
	raw, err := readFile(path)
	if err != nil {
		return err
	}
	fmText, body, ok := splitFrontMatter(raw)
	if !ok {
		return fmt.Errorf("project %s has no front matter", slug)
	}
	return atomicWrite(path, []byte("---\n"+setField(fmText, "status", status)+"\n---"+body))
}

// SaveResult reports what a body save did.
type SaveResult struct {
	RevisionWritten bool
	RevisionName    string
	WordCount       int
}

// SaveBody replaces the editable body, keeps the ## Log section
// verbatim, and snapshots the body under revisions/ unless the newest
// snapshot is under a minute old with the same byte length (an
// autosave re-save of unchanged text). source is "auto" or "manual".
func (s *Store) SaveBody(slug, body, source string) (SaveResult, error) {
	var res SaveResult
	if len(body) > MaxBody {
		return res, fmt.Errorf("body exceeds %d bytes", MaxBody)
	}
	if source != "auto" {
		source = "manual"
	}
	path, err := s.masterPath(slug)
	if err != nil {
		return res, err
	}
	defer s.lock(slug)()
	raw, err := readFile(path)
	if err != nil {
		return res, err
	}
	fmText, oldBody, ok := splitFrontMatter(raw)
	if !ok {
		return res, fmt.Errorf("project %s has no front matter", slug)
	}
	_, logPart := splitBody(oldBody)
	newEditable := strings.TrimRight(body, " \t\r\n")
	var combined string
	if logPart != "" {
		if newEditable != "" {
			combined = newEditable + "\n\n" + logPart
		} else {
			combined = logPart
		}
	} else {
		combined = newEditable + "\n"
	}
	if err := atomicWrite(path, []byte("---\n"+fmText+"\n---\n\n"+strings.TrimLeft(combined, "\n"))); err != nil {
		return res, err
	}

	res.WordCount = len(wordRe.FindAllString(newEditable, -1))
	bodyBytes := len(newEditable)
	revDir := filepath.Join(s.Root, s.ProjectsDir, slug, "revisions")
	if newest := newestFile(revDir, ".md"); newest != "" {
		if info, err := os.Stat(newest); err == nil && s.now().Sub(info.ModTime()) < time.Minute {
			if r, err := readFile(newest); err == nil {
				if fm, _, ok := splitFrontMatter(r); ok && parseFM(fm)["body_bytes"] == strconv.Itoa(bodyBytes) {
					return res, nil
				}
			}
		}
	}
	now := s.now()
	name := stamp(now)
	revPath, err := s.projectFile(slug, "revisions", name+".md")
	if err != nil {
		return res, err
	}
	snap := "---\nsaved_at: " + now.Format(time.RFC3339) + "\nword_count: " + strconv.Itoa(res.WordCount) +
		"\nsource: " + source + "\nbody_bytes: " + strconv.Itoa(bodyBytes) + "\n---\n\n" + newEditable + "\n"
	if err := atomicWrite(revPath, []byte(snap)); err != nil {
		return res, err
	}
	res.RevisionWritten, res.RevisionName = true, name
	return res, nil
}

var wordRe = regexp.MustCompile(`\b[\w']+\b`)

func newestFile(dir, ext string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ext) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return filepath.Join(dir, names[len(names)-1])
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Revision is one body snapshot.
type Revision struct {
	Name      string
	SavedAt   time.Time
	WordCount int
	Source    string
	BodyBytes int
}

var revisionNameRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9]{6}$`)

// Revisions lists snapshots, newest first.
func (s *Store) Revisions(slug string) ([]Revision, error) {
	if err := checkSlug(slug); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Root, s.ProjectsDir, slug, "revisions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Revision
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".md")
		if e.IsDir() || !revisionNameRe.MatchString(name) {
			continue
		}
		raw, err := readFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		fmText, _, _ := splitFrontMatter(raw)
		fm := parseFM(fmText)
		r := Revision{Name: name, Source: fm["source"]}
		r.WordCount, _ = strconv.Atoi(fm["word_count"])
		r.BodyBytes, _ = strconv.Atoi(fm["body_bytes"])
		if t, err := time.Parse(time.RFC3339, fm["saved_at"]); err == nil {
			r.SavedAt = t
		} else if info, err := e.Info(); err == nil {
			r.SavedAt = info.ModTime()
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt.After(out[j].SavedAt) })
	return out, nil
}

// Revision returns one snapshot's body (front matter stripped).
func (s *Store) Revision(slug, name string) (string, error) {
	if !revisionNameRe.MatchString(name) {
		return "", errors.New("invalid revision name")
	}
	p, err := s.projectFile(slug, "revisions", name+".md")
	if err != nil {
		return "", err
	}
	raw, err := readFile(p)
	if err != nil {
		return "", err
	}
	_, body, ok := splitFrontMatter(raw)
	if !ok {
		body = raw
	}
	return strings.TrimSpace(body), nil
}
