package forager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", "tok-123")
}

func TestFetchOneRequestShape(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gather" || r.Method != "POST" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		hdr = r.Header
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"id":"x","docs":[{"source":{"url":"https://final.example/a"},"title":"T","mime":"text/html","text":"body"}]}`))
	})
	doc, err := c.FetchOne(context.Background(), "https://example.org/a")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Source.URL != "https://final.example/a" || doc.Text != "body" || doc.Title != "T" {
		t.Fatalf("doc = %+v", doc)
	}
	if hdr.Get("X-Forager-Token") != "tok-123" || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatalf("headers = %v", hdr)
	}
	seeds, _ := got["seeds"].([]any)
	provs, _ := got["providers"].([]any)
	raw, _ := got["raw_mime"].([]any)
	scope, _ := got["scope"].(map[string]any)
	if len(seeds) != 1 || seeds[0] != "https://example.org/a" {
		t.Errorf("seeds = %v", got["seeds"])
	}
	if len(provs) != 1 || provs[0] != "seeds" {
		t.Errorf("providers = %v", got["providers"])
	}
	if len(raw) != 1 || raw[0] != "application/pdf" {
		t.Errorf("raw_mime = %v", got["raw_mime"])
	}
	if scope["max_docs"] != float64(1) || scope["max_fetch_bytes"] != float64(MaxFetchBytes) {
		t.Errorf("scope = %v", scope)
	}
	if id, _ := got["id"].(string); id == "" || got["directive"] == "" {
		t.Errorf("id/directive missing: %v", got)
	}
}

func TestFetchOneDecodesRawPDF(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"docs":[{"source":{"url":"https://x/a.pdf"},"title":"a.pdf","mime":"application/pdf","text":"","raw":"JVBERi0xLjcK"}]}`))
	})
	doc, err := c.FetchOne(context.Background(), "https://x/a.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if string(doc.Raw) != "%PDF-1.7\n" || doc.MIME != "application/pdf" {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestFetchOneNoDocReportsForagerError(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"docs":[],"errors":[{"url":"https://10.0.0.1/","stage":"fetch","message":"http: blocked non-public address"}]}`))
	})
	_, err := c.FetchOne(context.Background(), "https://10.0.0.1/")
	if err == nil || !strings.Contains(err.Error(), "blocked non-public address") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchOneNoDocNoErrors(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"docs":[]}`))
	})
	if _, err := c.FetchOne(context.Background(), "https://x/"); err == nil {
		t.Fatal("expected an error for an empty result")
	}
}

func TestFetchOneHTTPError(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
	_, err := c.FetchOne(context.Background(), "https://x/")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchOneRejectsNonHTTPURL(t *testing.T) {
	called := false
	c := serve(t, func(w http.ResponseWriter, r *http.Request) { called = true })
	for _, u := range []string{"file:///etc/passwd", "ftp://x/", "javascript:alert(1)", ""} {
		if _, err := c.FetchOne(context.Background(), u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
	if called {
		t.Fatal("forager called for a non-http URL")
	}
}
