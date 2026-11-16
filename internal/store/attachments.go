package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// MaxAttachment caps one image.
const MaxAttachment = 10 * 1024 * 1024

var attachmentNameRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9]{6}-[a-f0-9]{8}\.(png|jpg|jpeg|webp|gif)$`)

var mimeExt = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp", "image/gif": "gif"}

// SaveAttachment stores an image after checking its magic bytes
// against the claimed type (so a script can't arrive as "image/png"),
// and returns the generated filename.
func (s *Store) SaveAttachment(slug, mimeType string, data []byte) (string, error) {
	mimeType = strings.ToLower(mimeType)
	ext, ok := mimeExt[mimeType]
	if !ok {
		return "", errors.New("attachment must be png, jpeg, webp or gif")
	}
	if len(data) < 8 {
		return "", errors.New("attachment is too small to be an image")
	}
	if len(data) > MaxAttachment {
		return "", fmt.Errorf("attachment exceeds %d bytes", MaxAttachment)
	}
	if !magicMatches(mimeType, data) {
		return "", errors.New("file contents don't match the claimed image type")
	}
	var rnd [4]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", err
	}
	name := stamp(s.now()) + "-" + hex.EncodeToString(rnd[:]) + "." + ext
	p, err := s.projectFile(slug, "attachments", name)
	if err != nil {
		return "", err
	}
	return name, atomicWrite(p, data)
}

// Attachment returns an image and its content type.
func (s *Store) Attachment(slug, name string) ([]byte, string, error) {
	if !attachmentNameRe.MatchString(name) {
		return nil, "", errors.New("invalid attachment name")
	}
	p, err := s.projectFile(slug, "attachments", name)
	if err != nil {
		return nil, "", err
	}
	raw, err := readFile(p)
	if err != nil {
		return nil, "", err
	}
	ct := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif"}[strings.ToLower(filepath.Ext(name))]
	return []byte(raw), ct, nil
}

func magicMatches(mimeType string, b []byte) bool {
	switch mimeType {
	case "image/png":
		return bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	case "image/jpeg":
		return bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff})
	case "image/gif":
		return bytes.HasPrefix(b, []byte("GIF8"))
	case "image/webp":
		return len(b) >= 12 && bytes.HasPrefix(b, []byte("RIFF")) && string(b[8:12]) == "WEBP"
	}
	return false
}
