// Package forager is a thin client for the forager gather service
// (github.com/scheff-of-staff/forager, contract 0.4). The writer uses
// it to fetch one URL source: forager owns the network side (browser
// headers, Wayback fallback, the non-public-address guard), so the
// writer never fetches arbitrary URLs itself.
//
// Only the fields the writer reads are declared; unknown fields in
// forager's response are ignored.
package forager

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MaxFetchBytes is the per-document cap the writer asks for: room for
// a 30 MB PDF, the same ceiling the model accepts.
const MaxFetchBytes = 30 << 20

// maxResponse bounds what we read back: a 30 MB PDF is ~40 MB as
// base64, plus a little JSON.
const maxResponse = 48 << 20

type scope struct {
	MaxDocs       int   `json:"max_docs"`
	MaxFetchBytes int64 `json:"max_fetch_bytes"`
}

type request struct {
	ID        string   `json:"id"`
	Directive string   `json:"directive"`
	Seeds     []string `json:"seeds"`
	Scope     scope    `json:"scope"`
	Providers []string `json:"providers"`
	RawMIME   []string `json:"raw_mime"`
}

// Doc is the subset of forager's EvidenceDoc the writer uses.
type Doc struct {
	Source struct {
		URL string `json:"url"`
		Via string `json:"via"`
	} `json:"source"`
	Title     string `json:"title"`
	MIME      string `json:"mime"`
	Text      string `json:"text"`
	Raw       []byte `json:"raw"`
	Truncated bool   `json:"truncated"`
}

type result struct {
	Docs   []Doc `json:"docs"`
	Errors []struct {
		Stage   string `json:"stage"`
		Message string `json:"message"`
	} `json:"errors"`
}

// Client calls forager's POST /gather synchronously.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a client. The timeout covers a direct fetch, the Wayback
// retry and a large PDF transfer.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 4 * time.Minute},
	}
}

// FetchOne fetches a single URL through forager's seeds provider and
// returns the document. PDFs come back with their bytes in Raw (unless
// they exceeded MaxFetchBytes, in which case Truncated is set and Raw
// is empty).
func (c *Client) FetchOne(ctx context.Context, url string) (*Doc, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, errors.New("URL must start with http:// or https://")
	}
	body, err := json.Marshal(request{
		ID:        "writer-" + randomID(),
		Directive: "Fetch this page as a source for an article the author is writing.",
		Seeds:     []string{url},
		Scope:     scope{MaxDocs: 1, MaxFetchBytes: MaxFetchBytes},
		Providers: []string{"seeds"},
		RawMIME:   []string{"application/pdf"},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/gather", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forager-Token", c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("forager unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("forager read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("forager HTTP %d", resp.StatusCode)
	}
	var out result
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("forager response unreadable: %w", err)
	}
	if len(out.Docs) == 0 {
		for _, e := range out.Errors {
			if e.Message != "" {
				return nil, errors.New("fetch failed: " + e.Message)
			}
		}
		return nil, errors.New("fetch failed: nothing readable at that URL")
	}
	return &out.Docs[0], nil
}

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
