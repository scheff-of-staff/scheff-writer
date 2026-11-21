package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/scheff-of-staff/scheff-writer/internal/llm"
	"github.com/scheff-of-staff/scheff-writer/internal/render"
	"github.com/scheff-of-staff/scheff-writer/internal/store"
)

// toneSampleLimit bounds how many tone files are read per model call;
// the llm package applies its own byte budget on top.
const toneSampleLimit = 8

// --- pages -----------------------------------------------------------

type statusGroup struct {
	Label    string
	Projects []store.ProjectMeta
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	list, err := s.cfg.Store.List()
	if err != nil {
		log.Printf("index: list: %v", err)
		http.Error(w, "could not list projects", http.StatusInternalServerError)
		return
	}
	order := []string{"active", "paused", "done", "archived"}
	byStatus := map[string][]store.ProjectMeta{}
	for _, p := range list {
		st := p.Status
		if !store.ProjectStatuses[st] {
			st = "active"
		}
		byStatus[st] = append(byStatus[st], p)
	}
	var groups []statusGroup
	for _, st := range order {
		if len(byStatus[st]) > 0 {
			groups = append(groups, statusGroup{Label: strings.ToUpper(st[:1]) + st[1:], Projects: byStatus[st]})
		}
	}
	s.render(w, "index.html", map[string]any{"Groups": groups, "Count": len(list), "Auth": s.cfg.Token != ""})
}

type chatBubble struct {
	Role    string
	At      time.Time
	Content string
	HTML    template.HTML
}

func (s *server) handleArticle(w http.ResponseWriter, r *http.Request) {
	slug := r.URL.Query().Get("slug")
	if !store.SlugRe.MatchString(slug) {
		http.Error(w, "invalid project slug", http.StatusBadRequest)
		return
	}
	p, err := s.cfg.Store.Load(slug)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Printf("article: load %s: %v", slug, err)
		}
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	preview, err := render.MarkdownForArticle(p.Editable, slug)
	if err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	turns, err := s.cfg.Store.Chat(slug)
	if err != nil {
		log.Printf("article: chat %s: %v", slug, err)
	}
	bubbles := make([]chatBubble, 0, len(turns))
	for _, t := range turns {
		h, err := render.MarkdownForArticle(t.Content, slug)
		if err != nil {
			h = template.HTML(template.HTMLEscapeString(t.Content))
		}
		bubbles = append(bubbles, chatBubble{Role: t.Role, At: t.At, Content: t.Content, HTML: h})
	}
	sources, err := s.cfg.Store.Sources(slug)
	if err != nil {
		log.Printf("article: sources %s: %v", slug, err)
	}
	s.render(w, "article.html", map[string]any{
		"Slug":         slug,
		"Title":        p.Title,
		"EditableBody": p.Editable,
		"PreviewHTML":  preview,
		"ChatBubbles":  bubbles,
		"Sources":      sources,
	})
}

// --- projects --------------------------------------------------------

func (s *server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(r.PostFormValue("title"))
	if len(title) > 200 {
		http.Error(w, "title too long", http.StatusBadRequest)
		return
	}
	slug := slugify(title)
	if slug == "" {
		http.Error(w, "give the piece a title with some letters or numbers", http.StatusBadRequest)
		return
	}
	if _, err := s.cfg.Store.Load(slug); err == nil {
		http.Error(w, "a project with that name already exists", http.StatusConflict)
		return
	}
	if err := s.cfg.Store.Create(slug, title); err != nil {
		log.Printf("create %s: %v", slug, err)
		http.Error(w, "could not create project", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/article?slug="+slug, http.StatusSeeOther)
}

func (s *server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	slug, ok := formSlug(r)
	if !ok {
		fragmentErr(w, "Invalid project slug.")
		return
	}
	status := strings.ToLower(strings.TrimSpace(r.PostFormValue("status")))
	body := r.PostFormValue("body") // not trimmed: the author's whitespace is theirs
	quiet := r.PostFormValue("quiet") == "1"
	if status == "" && body == "" {
		fragmentErr(w, "Nothing to update.")
		return
	}
	if status != "" && !store.ProjectStatuses[status] {
		fragmentErr(w, "Unknown status.")
		return
	}
	if len(body) > store.MaxBody {
		fragmentErr(w, fmt.Sprintf("Body too long (max %d KB).", store.MaxBody/1024))
		return
	}
	if status != "" {
		if err := s.cfg.Store.SetStatus(slug, status); err != nil {
			log.Printf("set status %s: %v", slug, err)
			fragmentErr(w, "Could not update status: "+errClause(err))
			return
		}
	}
	if body != "" {
		if _, err := s.cfg.Store.SaveBody(slug, body, r.PostFormValue("source")); err != nil {
			log.Printf("save %s: %v", slug, err)
			fragmentErr(w, "Save failed: "+errClause(err))
			return
		}
	}
	var msg string
	switch {
	case status != "" && body != "":
		msg = "Status: <strong>" + template.HTMLEscapeString(status) + "</strong>. Body saved."
	case status != "":
		msg = "Status: <strong>" + template.HTMLEscapeString(status) + "</strong>."
	default:
		msg = "Saved."
	}
	reload := 2
	if quiet {
		reload = 0
	}
	fragmentOK(w, msg, reload)
}

func (s *server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	body := r.PostFormValue("body")
	if len(body) > store.MaxBody {
		body = body[:store.MaxBody]
	}
	var out template.HTML
	var err error
	if slug, ok := formSlug(r); ok {
		out, err = render.MarkdownForArticle(body, slug)
	} else {
		out, err = render.Markdown(body)
	}
	if err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, string(out))
}

// --- model calls -----------------------------------------------------

// voice loads the read-only voice references. Missing ones are normal.
func (s *server) voice() (profile string, tone []string) {
	profile, err := s.cfg.Store.Profile()
	if err != nil {
		log.Printf("voice: profile: %v", err)
	}
	tone, err = s.cfg.Store.ToneSamples(toneSampleLimit)
	if err != nil {
		log.Printf("voice: tone: %v", err)
	}
	return profile, tone
}

func (s *server) sourceRefs(slug string) []llm.SourceRef {
	metas, err := s.cfg.Store.Sources(slug)
	if err != nil {
		log.Printf("sources %s: %v", slug, err)
		return nil
	}
	out := make([]llm.SourceRef, 0, len(metas))
	for _, m := range metas {
		src, err := s.cfg.Store.Source(slug, m.Name)
		if err != nil {
			continue
		}
		out = append(out, llm.SourceRef{Title: src.Title, URL: src.URL, Summary: src.Body})
	}
	return out
}

func (s *server) title(slug string) string {
	if p, err := s.cfg.Store.Load(slug); err == nil && p.Title != "" {
		return p.Title
	}
	return slug
}

// handleTransform polishes the on-screen draft. It never writes: the
// author reviews the result and Accept goes through update-project.
func (s *server) handleTransform(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	slug, ok := formSlug(r)
	if !ok {
		http.Error(w, "invalid project slug", http.StatusBadRequest)
		return
	}
	draft := r.PostFormValue("body")
	if strings.TrimSpace(draft) == "" {
		fragmentErr(w, "Draft is empty. Write something before polishing.")
		return
	}
	if len(draft) > store.MaxBody {
		fragmentErr(w, "Draft too large to polish in one pass.")
		return
	}
	profile, tone := s.voice()
	ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
	defer cancel()
	res, err := s.cfg.LLM.Transform(ctx, llm.TransformRequest{
		ArticleTitle: s.title(slug),
		RawDraft:     draft,
		Profile:      profile,
		ToneSamples:  tone,
		Sources:      s.sourceRefs(slug),
	})
	if err != nil {
		log.Printf("transform %s: %v", slug, err)
		fragmentErr(w, "Polish failed: "+errClause(err))
		return
	}
	polishedHTML, err := render.MarkdownForArticle(res.Polished, slug)
	if err != nil {
		fragmentErr(w, "Polished output could not be rendered.")
		return
	}
	s.render(w, "polished-result", map[string]any{
		"Slug":         slug,
		"Model":        res.Model,
		"ElapsedSec":   fmt.Sprintf("%.1f", res.Elapsed.Seconds()),
		"InputTokens":  res.InputTokens,
		"OutputTokens": res.OutputTokens,
		"Polished":     res.Polished,
		"PolishedHTML": polishedHTML,
		"HasProfile":   profile != "",
		"ToneCount":    len(tone),
	})
}

func chatError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, `<div class="chat-bubble chat-error">%s</div>`, template.HTMLEscapeString(msg))
}

const (
	maxChatMessage   = 64 * 1024
	maxImagesPerTurn = 8
	chatTimeout      = 120 * time.Second
)

// chatInput is one conversation turn, independent of how it arrived
// (editor form or JSON API).
type chatInput struct {
	Slug, Draft, Message string
	Images               []llm.ImageRef
	ImageRefs            []string // Markdown refs saved with the user turn
	Client               *llm.Client
	History              bool // send the saved thread as context
	Persist              bool // append this turn to chat.md
}

type chatOutput struct {
	Result    *llm.ChatResult
	Persisted bool
}

// chatTurn runs one turn: gather context, call the model, and append
// the pair to the thread when asked. Shared by the editor and the API
// so both send the model exactly the same thing.
func (s *server) chatTurn(ctx context.Context, in chatInput) (*chatOutput, error) {
	var turns []llm.ChatTurn
	if in.History {
		history, err := s.cfg.Store.Chat(in.Slug)
		if err != nil {
			log.Printf("chat %s: history: %v", in.Slug, err)
		}
		turns = make([]llm.ChatTurn, 0, len(history))
		for _, t := range history {
			turns = append(turns, llm.ChatTurn{Role: t.Role, Content: t.Content})
		}
	}
	profile, tone := s.voice()
	res, err := in.Client.Chat(ctx, llm.ChatRequest{
		ArticleTitle: s.title(in.Slug),
		RawDraft:     in.Draft,
		Profile:      profile,
		ToneSamples:  tone,
		Sources:      s.sourceRefs(in.Slug),
		History:      turns,
		UserMessage:  in.Message,
		Images:       in.Images,
	})
	if err != nil {
		return nil, err
	}
	out := &chatOutput{Result: res}
	if !in.Persist {
		return out, nil
	}
	// The saved user turn carries image references so the bubble
	// re-renders on reload; later turns see them only as text.
	saved := in.Message
	if len(in.ImageRefs) > 0 {
		saved = in.Message + "\n\n" + strings.Join(in.ImageRefs, "\n")
	}
	if err := s.cfg.Store.AppendChat(in.Slug, saved, res.Reply); err != nil {
		log.Printf("chat %s: persist: %v", in.Slug, err)
		return out, nil
	}
	out.Persisted = true
	return out, nil
}

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	slug, ok := formSlug(r)
	if !ok {
		http.Error(w, "invalid project slug", http.StatusBadRequest)
		return
	}
	msg := strings.TrimSpace(r.PostFormValue("message"))
	if msg == "" {
		chatError(w, "Type a message first.")
		return
	}
	if len(msg) > maxChatMessage {
		chatError(w, "Message too long (max 64 KB).")
		return
	}

	// Images pasted this turn were uploaded first; the form carries
	// their generated names. The store re-validates each name.
	var images []llm.ImageRef
	var imageRefs []string
	if raw := strings.TrimSpace(r.PostFormValue("attachments")); raw != "" {
		names := strings.Split(raw, ",")
		if len(names) > maxImagesPerTurn {
			names = names[:maxImagesPerTurn]
		}
		for _, n := range names {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			data, mime, err := s.cfg.Store.Attachment(slug, n)
			if err != nil {
				continue
			}
			images = append(images, llm.ImageRef{MIME: mime, Base64: base64.StdEncoding.EncodeToString(data)})
			imageRefs = append(imageRefs, "![](attachments/"+n+")")
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), chatTimeout)
	defer cancel()
	turn, err := s.chatTurn(ctx, chatInput{
		Slug:      slug,
		Draft:     r.PostFormValue("body"),
		Message:   msg,
		Images:    images,
		ImageRefs: imageRefs,
		Client:    s.cfg.LLM,
		History:   true,
		Persist:   true,
	})
	if err != nil {
		log.Printf("chat %s: model: %v", slug, err)
		chatError(w, "Chat call failed: "+errClause(err))
		return
	}
	res, persisted := turn.Result, turn.Persisted
	replyHTML, err := render.MarkdownForArticle(res.Reply, slug)
	if err != nil {
		replyHTML = template.HTML(template.HTMLEscapeString(res.Reply))
	}
	s.render(w, "chat-turn", map[string]any{
		"Slug":         slug,
		"Model":        res.Model,
		"ElapsedSec":   fmt.Sprintf("%.1f", res.Elapsed.Seconds()),
		"InputTokens":  res.InputTokens,
		"OutputTokens": res.OutputTokens,
		"UserMessage":  msg,
		"ReplyHTML":    replyHTML,
		"Reply":        res.Reply,
		"Persisted":    persisted,
		"SourcesCount": res.SourcesCount,
		"SourcesKB":    kb(res.SourcesBytes),
		"ProfileKB":    kb(res.ProfileBytes),
		"ToneCount":    res.ToneCount,
		"ToneKB":       kb(res.ToneBytes),
		"DraftKB":      kb(res.DraftBytes),
		"HistoryTurns": res.HistoryTurns,
	})
}

func (s *server) handleChatReset(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	slug, ok := formSlug(r)
	if !ok {
		http.Error(w, "invalid project slug", http.StatusBadRequest)
		return
	}
	if err := s.cfg.Store.ResetChat(slug); err != nil {
		log.Printf("chat reset %s: %v", slug, err)
		fragmentErr(w, "Reset failed.")
		return
	}
	fragmentOK(w, "Chat thread reset.", 2)
}

// --- sources ---------------------------------------------------------

func (s *server) handleRemoveSource(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	slug, ok := formSlug(r)
	if !ok {
		fragmentErr(w, "Invalid project slug.")
		return
	}
	if err := s.cfg.Store.RemoveSource(slug, strings.TrimSpace(r.PostFormValue("name"))); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, store.ErrNotFound) {
			fragmentErr(w, "That source is already gone.")
			return
		}
		fragmentErr(w, "Remove failed: "+errClause(err))
		return
	}
	fragmentOK(w, "Source removed.", 1)
}

// --- attachments -----------------------------------------------------

func (s *server) handleUploadAttachment(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, store.MaxAttachment+(1<<20))
	if err := r.ParseMultipartForm(2 << 20); err != nil {
		http.Error(w, "bad or oversized upload", http.StatusRequestEntityTooLarge)
		return
	}
	slug, ok := formSlug(r)
	if !ok {
		http.Error(w, "invalid project slug", http.StatusBadRequest)
		return
	}
	f, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "missing image", http.StatusBadRequest)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, store.MaxAttachment+1))
	if err != nil || len(data) > store.MaxAttachment {
		http.Error(w, "image exceeds 10 MB", http.StatusRequestEntityTooLarge)
		return
	}
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		http.Error(w, "unsupported image type (png, jpeg, gif or webp)", http.StatusUnsupportedMediaType)
		return
	}
	name, err := s.cfg.Store.SaveAttachment(slug, mime, data)
	if err != nil {
		log.Printf("attachment %s: %v", slug, err)
		http.Error(w, "upload failed", http.StatusUnsupportedMediaType)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"filename": name, "mime": mime, "bytes": len(data)})
}

func (s *server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	slug := q.Get("slug")
	if !store.SlugRe.MatchString(slug) {
		http.Error(w, "invalid project slug", http.StatusBadRequest)
		return
	}
	data, mime, err := s.cfg.Store.Attachment(slug, q.Get("name"))
	if err != nil {
		http.Error(w, "attachment not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(data)
}

// --- revisions -------------------------------------------------------

func (s *server) handleListRevisions(w http.ResponseWriter, r *http.Request) {
	slug := r.URL.Query().Get("slug")
	if !store.SlugRe.MatchString(slug) {
		http.Error(w, "invalid project slug", http.StatusBadRequest)
		return
	}
	revs, err := s.cfg.Store.Revisions(slug)
	if err != nil {
		log.Printf("revisions %s: %v", slug, err)
		http.Error(w, "read failed", http.StatusInternalServerError)
		return
	}
	type row struct {
		Name      string `json:"name"`
		SavedAt   string `json:"saved_at"`
		WordCount int    `json:"word_count"`
		Source    string `json:"source"`
		BodyBytes int    `json:"body_bytes"`
	}
	out := make([]row, 0, len(revs))
	for _, rv := range revs {
		out = append(out, row{rv.Name, rv.SavedAt.Format(time.RFC3339), rv.WordCount, rv.Source, rv.BodyBytes})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *server) handleLoadRevision(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	slug := q.Get("slug")
	if !store.SlugRe.MatchString(slug) {
		http.Error(w, "invalid project slug", http.StatusBadRequest)
		return
	}
	body, err := s.cfg.Store.Revision(slug, q.Get("name"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "revision not found", http.StatusNotFound)
			return
		}
		http.Error(w, "invalid revision", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, body)
}
