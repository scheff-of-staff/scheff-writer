package store

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Profile returns the voice/background reference (profile.md body),
// or "" when there is none. Read-only: the writer never edits it.
func (s *Store) Profile() (string, error) {
	raw, err := readFile(filepath.Join(s.Root, s.ReferenceDir, "profile.md"))
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	_, body, ok := splitFrontMatter(raw)
	if !ok {
		body = raw
	}
	return strings.TrimSpace(body), nil
}

// ToneSamples returns up to n writing samples from tone/ (newest
// first, README excluded).
func (s *Store) ToneSamples(n int) ([]string, error) {
	dir := filepath.Join(s.Root, s.ReferenceDir, "tone")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	type f struct {
		path string
		mod  int64
	}
	var files []f
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || strings.EqualFold(e.Name(), "README.md") {
			continue
		}
		if info, err := e.Info(); err == nil {
			files = append(files, f{filepath.Join(dir, e.Name()), info.ModTime().UnixNano()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod > files[j].mod })
	var out []string
	for _, x := range files {
		if n > 0 && len(out) >= n {
			break
		}
		raw, err := readFile(x.path)
		if err != nil {
			continue
		}
		_, body, ok := splitFrontMatter(raw)
		if !ok {
			body = raw
		}
		out = append(out, strings.TrimSpace(body))
	}
	return out, nil
}
