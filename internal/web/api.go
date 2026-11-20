package web

// The JSON API: a stable, versioned surface for scripts and other
// services, separate from the editor's htmx endpoints (which return
// HTML fragments shaped for the page and may change with it).
//
// Off unless Config.APIToken is set. It accepts only that token, as a
// bearer header; the sign-in token and session cookie don't work here,
// and the API token doesn't work anywhere else.
//
//	POST /api/v1/projects/{slug}/chat
//	  {"message": "...", "draft": "...", "model": "...", "persist": true, "history": true}
//	→ {"reply": "...", "model": "...", "usage": {...}, "elapsed_ms": 0, "persisted": true, "history_turns": 0}
//
// Errors are {"error": "..."} with a 4xx/5xx status.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"

	"github.com/scheff-of-staff/scheff-writer/internal/store"
)

const (
	maxAPIBody              = 1 << 20 // 1 MiB: 64 KB message, the rest for the draft
	defaultAPIMaxConcurrent = 4
)

type apiChatRequest struct {
	Message string  `json:"message"`
	Draft   string  `json:"draft"`
	Model   *string `json:"model"`
	Persist *bool   `json:"persist"`
	History *bool   `json:"history"`
}

type apiUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type apiChatResponse struct {
	Reply        string   `json:"reply"`
	Model        string   `json:"model"`
	Usage        apiUsage `json:"usage"`
	ElapsedMS    int64    `json:"elapsed_ms"`
	Persisted    bool     `json:"persisted"`
	HistoryTurns int      `json:"history_turns"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeJSONBody reads exactly one JSON object with no unknown fields.
// It writes the error response itself and reports whether to go on.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || ct != "application/json" {
		apiError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAPIBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			apiError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		apiError(w, http.StatusBadRequest, "invalid JSON: "+errClause(err))
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		apiError(w, http.StatusBadRequest, "invalid JSON: trailing data")
		return false
	}
	return true
}

func (s *server) handleAPIChat(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !store.SlugRe.MatchString(slug) {
		apiError(w, http.StatusBadRequest, "invalid project slug")
		return
	}
	var in apiChatRequest
	if !decodeJSONBody(w, r, &in) {
		return
	}
	msg := strings.TrimSpace(in.Message)
	if msg == "" {
		apiError(w, http.StatusBadRequest, "message is required")
		return
	}
	if len(msg) > maxChatMessage {
		apiError(w, http.StatusBadRequest, "message too long (max 64 KB)")
		return
	}
	client := s.cfg.LLM
	if in.Model != nil && *in.Model != client.Model {
		if !s.apiModels[*in.Model] {
			apiError(w, http.StatusBadRequest, "model not allowed")
			return
		}
		c := *client // per-request copy; the shared client is never mutated
		c.Model = *in.Model
		client = &c
	}
	if _, err := s.cfg.Store.Load(slug); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			apiError(w, http.StatusNotFound, "project not found")
			return
		}
		log.Printf("api chat %s: load: %v", slug, err)
		apiError(w, http.StatusInternalServerError, "project unreadable")
		return
	}

	select {
	case s.apiSem <- struct{}{}:
		defer func() { <-s.apiSem }()
	default:
		w.Header().Set("Retry-After", "5")
		apiError(w, http.StatusTooManyRequests, "too many concurrent requests")
		return
	}

	in.History = orTrue(in.History)
	in.Persist = orTrue(in.Persist)
	ctx, cancel := context.WithTimeout(r.Context(), chatTimeout)
	defer cancel()
	turn, err := s.chatTurn(ctx, chatInput{
		Slug:    slug,
		Draft:   in.Draft,
		Message: msg,
		Client:  client,
		History: *in.History,
		Persist: *in.Persist,
	})
	// Metadata only: never the message, draft or reply.
	if err != nil {
		log.Printf("api chat %s: model=%s msg_bytes=%d: %v", slug, client.Model, len(msg), err)
		apiError(w, http.StatusBadGateway, "model call failed: "+errClause(err))
		return
	}
	res := turn.Result
	log.Printf("api chat %s: model=%s msg_bytes=%d draft_bytes=%d history=%t persisted=%t in=%d out=%d ms=%d",
		slug, res.Model, len(msg), len(in.Draft), *in.History, turn.Persisted,
		res.InputTokens, res.OutputTokens, res.Elapsed.Milliseconds())
	writeJSON(w, http.StatusOK, apiChatResponse{
		Reply:        res.Reply,
		Model:        res.Model,
		Usage:        apiUsage{InputTokens: res.InputTokens, OutputTokens: res.OutputTokens},
		ElapsedMS:    res.Elapsed.Milliseconds(),
		Persisted:    turn.Persisted,
		HistoryTurns: res.HistoryTurns,
	})
}

func orTrue(b *bool) *bool {
	if b == nil {
		t := true
		return &t
	}
	return b
}
