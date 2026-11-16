package store

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// MaxTurn caps one chat message (matches the workflow it replaces).
const MaxTurn = 32 * 1024

// ChatTurn is one side of the writing-assistant conversation.
type ChatTurn struct {
	Role    string // "user" | "assistant"
	At      time.Time
	Content string
}

// chatHeaderRe matches "## User <rfc3339>" / "## Assistant <rfc3339>"
// at line start. The format is part of the file contract.
var chatHeaderRe = regexp.MustCompile(`(?m)^##\s+(User|Assistant)\s+(\S+)\s*$`)

// Chat loads the thread; nil, nil when there is none yet.
func (s *Store) Chat(slug string) ([]ChatTurn, error) {
	p, err := s.projectFile(slug, ".", "chat.md")
	if err != nil {
		return nil, err
	}
	raw, err := readFile(p)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_, body, ok := splitFrontMatter(raw)
	if !ok {
		body = raw
	}
	return ParseChat(body), nil
}

// ParseChat extracts turns; malformed headers are skipped.
func ParseChat(body string) []ChatTurn {
	locs := chatHeaderRe.FindAllStringSubmatchIndex(body, -1)
	var out []ChatTurn
	for i, loc := range locs {
		at, err := time.Parse(time.RFC3339, body[loc[4]:loc[5]])
		if err != nil {
			continue
		}
		end := len(body)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		out = append(out, ChatTurn{Role: strings.ToLower(body[loc[2]:loc[3]]), At: at, Content: strings.TrimSpace(body[loc[1]:end])})
	}
	return out
}

// AppendChat adds a user + assistant pair, creating chat.md if
// needed. Timestamps are generated here, never taken from a client.
func (s *Store) AppendChat(slug, user, assistant string) error {
	user, assistant = strings.TrimSpace(user), strings.TrimSpace(assistant)
	if user == "" || assistant == "" {
		return errors.New("both user and assistant messages are required")
	}
	if len(user) > MaxTurn || len(assistant) > MaxTurn {
		return fmt.Errorf("a chat message exceeds %d bytes", MaxTurn)
	}
	p, err := s.projectFile(slug, ".", "chat.md")
	if err != nil {
		return err
	}
	defer s.lock(slug)()
	existing, err := readFile(p)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	now := s.now().Format(time.RFC3339)
	if existing == "" {
		existing = "---\ntype: chat\nproject: " + slug + "\ncreated: " + now + "\n---\n"
	}
	block := "\n## User " + now + "\n\n" + user + "\n\n## Assistant " + now + "\n\n" + assistant + "\n"
	return atomicWrite(p, []byte(strings.TrimRight(existing, " \t\r\n")+"\n"+block))
}

// ResetChat deletes the thread (no-op if absent).
func (s *Store) ResetChat(slug string) error {
	p, err := s.projectFile(slug, ".", "chat.md")
	if err != nil {
		return err
	}
	defer s.lock(slug)()
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
