package store

import (
	"regexp"
	"strings"
)

// Front matter in these files is machine-written with a fixed shape,
// so a small scalar parser is enough and keeps the module free of
// third-party dependencies. Unknown keys are always preserved on
// rewrite because edits are surgical (setField), never a re-encode.

// splitFrontMatter returns (front-matter text without fences, body,
// ok). body keeps everything after the closing fence.
func splitFrontMatter(s string) (string, string, bool) {
	if !strings.HasPrefix(s, "---\n") {
		return "", s, false
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", s, false
	}
	fm := rest[:end]
	body := rest[end+4:]
	return fm, body, true
}

// parseFM reads top-level `key: value` scalars; nested or list lines
// are skipped. Quoted values are unquoted and unescaped.
func parseFM(fm string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(fm, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '-' || line[0] == '#' {
			continue
		}
		i := strings.Index(line, ":")
		if i <= 0 {
			continue
		}
		out[strings.TrimSpace(line[:i])] = unquote(strings.TrimSpace(line[i+1:]))
	}
	return out
}

func unquote(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
		v = strings.ReplaceAll(v, `\"`, `"`)
		v = strings.ReplaceAll(v, `\\`, `\`)
	}
	return v
}

// quote renders a double-quoted YAML scalar (same escaping as the
// n8n workflows this package replaces).
func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", " ")
	return `"` + s + `"`
}

// setField replaces `key: …` in front matter, or appends it.
func setField(fm, key, value string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `:.*$`)
	line := key + ": " + value
	if re.MatchString(fm) {
		return re.ReplaceAllLiteralString(fm, line)
	}
	if fm != "" && !strings.HasSuffix(fm, "\n") {
		fm += "\n"
	}
	return fm + line
}
