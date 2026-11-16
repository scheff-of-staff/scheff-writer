// Package store reads and writes writing projects in a folder of
// Markdown files: the same layout as a Scheff of Staff vault's
// 04_Projects/ (see scheffofstaff docs/contracts.md §1, §3).
//
//	<root>/<projects>/<slug>.md             project: front matter, body, ## Log
//	<root>/<projects>/<slug>/revisions/     body snapshots
//	<root>/<projects>/<slug>/sources/       source extracts
//	<root>/<projects>/<slug>/attachments/   images
//	<root>/<projects>/<slug>/chat.md        writing-assistant thread
//	<root>/<reference>/profile.md, tone/    voice references (read-only)
//
// Standalone, <root> is any folder. With Scheff, <root> is the vault
// and the writer becomes the owner of those paths per the contracts.
//
// Write scope is enforced here, not by callers: every write path is
// built from a validated slug and a validated name, then re-checked
// after cleaning to sit inside the project's own files.
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// SlugRe is the project slug shape shared by every Scheff package.
var SlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// ErrNotFound is returned when a project or file doesn't exist.
var ErrNotFound = errors.New("not found")

// Store is a handle on a writing folder.
type Store struct {
	Root         string // vault or standalone folder
	ProjectsDir  string // relative; "04_Projects" by default
	ReferenceDir string // relative; "06_Reference" by default
	Now          func() time.Time

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New returns a Store with the Scheff vault defaults.
func New(root string) *Store {
	return &Store{Root: root, ProjectsDir: "04_Projects", ReferenceDir: "06_Reference", locks: map[string]*sync.Mutex{}}
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// lock serialises writes per project.
func (s *Store) lock(slug string) func() {
	s.mu.Lock()
	m, ok := s.locks[slug]
	if !ok {
		m = &sync.Mutex{}
		s.locks[slug] = m
	}
	s.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func checkSlug(slug string) error {
	if !SlugRe.MatchString(slug) {
		return fmt.Errorf("invalid project slug %q", slug)
	}
	return nil
}

// masterPath is <root>/<projects>/<slug>.md.
func (s *Store) masterPath(slug string) (string, error) {
	if err := checkSlug(slug); err != nil {
		return "", err
	}
	return filepath.Join(s.Root, s.ProjectsDir, slug+".md"), nil
}

// projectFile returns <root>/<projects>/<slug>/<sub>/<name>, refusing
// anything that cleans to a path outside the project's folder.
func (s *Store) projectFile(slug, sub, name string) (string, error) {
	if err := checkSlug(slug); err != nil {
		return "", err
	}
	base := filepath.Join(s.Root, s.ProjectsDir, slug)
	p := filepath.Clean(filepath.Join(base, sub, name))
	if !strings.HasPrefix(p, base+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes project folder: %s/%s", sub, name)
	}
	return p, nil
}

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp-writer"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", err
	}
	return string(b), nil
}

// stamp is the YYYY-MM-DD-HHMMSS UTC filename prefix every writer
// file uses (sortable lexicographically).
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02-150405") }
