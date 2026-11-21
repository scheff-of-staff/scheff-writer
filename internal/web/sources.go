package web

// Adding a source: a URL (fetched through forager), pasted text, or an
// uploaded PDF becomes a Markdown file under the project's sources/,
// in the same format the source_ingest workflow wrote, so Transform
// and chat read old and new sources alike.

import (
	"context"
	"errors"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/scheff-of-staff/scheff-writer/internal/forager"
	"github.com/scheff-of-staff/scheff-writer/internal/llm"
	"github.com/scheff-of-staff/scheff-writer/internal/store"
)

// SourceFetcher fetches one URL source. *forager.Client implements it.
type SourceFetcher interface {
	FetchOne(ctx context.Context, url string) (*forager.Doc, error)
}

const (
	maxPastedText    = 256 * 1024
	maxPDFUpload     = 30 << 20
	maxSourceURL     = 2048
	maxSourceTitle   = 300
	maxSourcesAtOnce = 2 // large PDFs are memory-heavy
	sourceTimeout    = 7 * time.Minute
)

var sourceModels = map[string]bool{"claude-best": true, "claude-fast": true, "openai-best": true, "openai-fast": true}

type sourceForm struct {
	slug, kind, url, title, text, model, depth string
	pdf                                        []byte
}

// errUserFacing marks validation messages that go to the author as-is.
type errUserFacing string

func (e errUserFacing) Error() string { return string(e) }

func (s *server) handleAddSource(w http.ResponseWriter, r *http.Request) {
	// Uploads and extraction outlast the server-wide read/write limits.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(5 * time.Minute))
	_ = rc.SetWriteDeadline(time.Now().Add(sourceTimeout + time.Minute))

	f, err := readSourceForm(w, r)
	if err != nil {
		var msg errUserFacing
		if errors.As(err, &msg) {
			fragmentErr(w, string(msg))
		} else {
			fragmentErr(w, "Couldn't read the form.")
		}
		return
	}
	if err := s.validateSource(f); err != nil {
		fragmentErr(w, err.Error())
		return
	}

	select {
	case s.sourceSem <- struct{}{}:
		defer func() { <-s.sourceSem }()
	default:
		fragmentErr(w, "Another source is being added. Try again in a moment.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), sourceTimeout)
	defer cancel()

	req := llm.ExtractRequest{TitleHint: f.title, Depth: f.depth}
	kind, recordedURL := f.kind, f.url
	switch f.kind {
	case "url":
		doc, err := s.cfg.Fetcher.FetchOne(ctx, f.url)
		if err != nil {
			log.Printf("add-source %s: fetch: %v", f.slug, err)
			fragmentErr(w, "Couldn't fetch that URL: "+fetchReason(err)+". Try the Text tab and paste the content.")
			return
		}
		if doc.Source.URL != "" {
			recordedURL = doc.Source.URL
		}
		req.URL = recordedURL
		if doc.MIME == "application/pdf" {
			switch {
			case doc.Truncated:
				fragmentErr(w, "That PDF is larger than 30 MB.")
				return
			case len(doc.Raw) == 0:
				fragmentErr(w, "The PDF came back without its contents (forager 0.4 or newer is needed).")
				return
			}
			req.PDF, kind = doc.Raw, "pdf"
		} else {
			if strings.TrimSpace(doc.Text) == "" {
				fragmentErr(w, "No readable text at that URL. Try the Text tab and paste the content.")
				return
			}
			req.Text = doc.Text
		}
	case "text":
		req.URL, req.Text = f.url, f.text
	case "pdf":
		req.PDF = f.pdf
	}

	client := *s.cfg.LLM // per-request copy; the shared client is never mutated
	client.Model = f.model
	ex, err := client.ExtractSource(ctx, req)
	if err != nil {
		log.Printf("add-source %s: kind=%s model=%s: %v", f.slug, kind, f.model, err)
		fragmentErr(w, "Summarising failed: "+errClause(err))
		return
	}

	title := f.title
	if title == "" {
		title = ex.Title
	}
	if title == "" {
		if u, err := url.Parse(recordedURL); err == nil {
			title = u.Hostname()
		}
	}
	if title == "" {
		title = "Untitled source"
	}
	title = strings.Join(strings.Fields(title), " ")

	name, err := s.cfg.Store.AddSource(f.slug, store.NewSource{
		Title: title, URL: recordedURL, Kind: kind,
		Model: ex.Model, Depth: ex.Depth, DepthClamped: ex.DepthClamped,
		Body: composeSourceBody(title, ex),
	})
	if err != nil {
		log.Printf("add-source %s: save: %v", f.slug, err)
		fragmentErr(w, "Saving the source failed.")
		return
	}
	// Metadata only: never the content or the extract.
	log.Printf("add-source %s: %s kind=%s model=%s depth=%s clamped=%t text_bytes=%d pdf_bytes=%d in=%d out=%d ms=%d",
		f.slug, name, kind, ex.Model, ex.Depth, ex.DepthClamped, len(req.Text), len(req.PDF),
		ex.InputTokens, ex.OutputTokens, ex.Elapsed.Milliseconds())
	fragmentOK(w, "Added source <strong>"+template.HTMLEscapeString(title)+"</strong>.", 2)
}

// readSourceForm reads either the url/text form (urlencoded) or the
// PDF upload (multipart, streamed so nothing touches disk).
func readSourceForm(w http.ResponseWriter, r *http.Request) (*sourceForm, error) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "multipart/form-data" {
		r.Body = http.MaxBytesReader(w, r.Body, maxPastedText+64*1024)
		if err := r.ParseForm(); err != nil {
			return nil, errUserFacing("Pasted text is too long (max 256 KB).")
		}
		v := r.PostForm
		return &sourceForm{
			slug: strings.TrimSpace(v.Get("slug")), kind: strings.TrimSpace(v.Get("kind")),
			url: strings.TrimSpace(v.Get("url")), title: strings.TrimSpace(v.Get("title")),
			text: v.Get("text"), model: strings.TrimSpace(v.Get("model")), depth: strings.TrimSpace(v.Get("depth")),
		}, nil
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPDFUpload+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, err
	}
	f := &sourceForm{}
	fields := map[string]*string{"slug": &f.slug, "kind": &f.kind, "title": &f.title, "model": &f.model, "depth": &f.depth}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				return nil, errUserFacing("That PDF is larger than 30 MB.")
			}
			return nil, err
		}
		name := part.FormName()
		switch {
		case name == "pdf":
			data, err := io.ReadAll(io.LimitReader(part, maxPDFUpload+1))
			if err != nil {
				var tooBig *http.MaxBytesError
				if errors.As(err, &tooBig) {
					return nil, errUserFacing("That PDF is larger than 30 MB.")
				}
				return nil, err
			}
			if len(data) > maxPDFUpload {
				return nil, errUserFacing("That PDF is larger than 30 MB.")
			}
			f.pdf = data
		case fields[name] != nil:
			b, err := io.ReadAll(io.LimitReader(part, 4096))
			if err != nil {
				return nil, err
			}
			*fields[name] = strings.TrimSpace(string(b))
		}
		part.Close()
	}
	return f, nil
}

func (s *server) validateSource(f *sourceForm) error {
	if !store.SlugRe.MatchString(f.slug) {
		return errors.New("Invalid project slug.")
	}
	if _, err := s.cfg.Store.Load(f.slug); err != nil {
		return errors.New("No such project.")
	}
	if f.model == "" {
		f.model = "claude-best"
	}
	if !sourceModels[f.model] {
		return errors.New("Unsupported model.")
	}
	if f.depth == "" {
		f.depth = "medium"
	}
	if _, ok := llm.DepthTokens[f.depth]; !ok {
		return errors.New("Unsupported depth.")
	}
	if len(f.title) > maxSourceTitle {
		return errors.New("Title is too long (max 300 characters).")
	}
	switch f.kind {
	case "url":
		if !validSourceURL(f.url) {
			return errors.New("Enter a full http:// or https:// URL.")
		}
		if s.cfg.Fetcher == nil {
			return errors.New("Fetching URLs isn't set up on this writer. Use the Text tab or upload a PDF.")
		}
	case "text":
		if strings.TrimSpace(f.text) == "" {
			return errors.New("Paste some text to summarise.")
		}
		if len(f.text) > maxPastedText {
			return errors.New("Pasted text is too long (max 256 KB).")
		}
		if f.url != "" && !validSourceURL(f.url) {
			return errors.New("The source URL must be a full http:// or https:// URL.")
		}
	case "pdf":
		if len(f.pdf) == 0 {
			return errors.New("Choose a PDF file to upload.")
		}
		if !strings.HasPrefix(string(f.pdf[:min(5, len(f.pdf))]), "%PDF-") {
			return errors.New("That file is not a PDF.")
		}
		f.url = ""
	default:
		return errors.New("Unsupported source kind.")
	}
	return nil
}

func validSourceURL(raw string) bool {
	if raw == "" || len(raw) > maxSourceURL {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// fetchReason is the author-facing part of a fetch error.
func fetchReason(err error) string {
	msg := strings.TrimPrefix(err.Error(), "fetch failed: ")
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}

// composeSourceBody renders the extract exactly as the source_ingest
// workflow did: H1 title, summary, then optional key points, tables
// and charts sections.
func composeSourceBody(title string, ex *llm.Extraction) string {
	var b strings.Builder
	b.WriteString("# " + title + "\n\n" + ex.Summary)
	if len(ex.KeyPoints) > 0 {
		b.WriteString("\n\n## Key points\n\n")
		for i, p := range ex.KeyPoints {
			if i > 0 {
				b.WriteString("\n")
			}
			b.WriteString("- " + p)
		}
	}
	if len(ex.Tables) > 0 {
		b.WriteString("\n\n## Tables\n\n")
		blocks := make([]string, 0, len(ex.Tables))
		for _, t := range ex.Tables {
			blocks = append(blocks, composeTable(t))
		}
		b.WriteString(strings.Join(blocks, "\n\n"))
	}
	if len(ex.Charts) > 0 {
		b.WriteString("\n\n## Charts\n\n")
		blocks := make([]string, 0, len(ex.Charts))
		for _, c := range ex.Charts {
			blocks = append(blocks, composeChart(c))
		}
		b.WriteString(strings.Join(blocks, "\n\n"))
	}
	return b.String()
}

func composeTable(t llm.Table) string {
	caption := ""
	if c := strings.TrimSpace(t.Caption); c != "" {
		caption = "**" + c + "**\n\n"
	}
	rows := nonEmptyRows(t.Rows)
	if len(rows) == 0 {
		return caption + "_(table data not extracted)_"
	}
	lines := []string{mdRow(rows[0])}
	sep := make([]string, len(rows[0]))
	for i := range sep {
		sep[i] = "---"
	}
	lines = append(lines, "| "+strings.Join(sep, " | ")+" |")
	for _, r := range rows[1:] {
		lines = append(lines, mdRow(r))
	}
	return caption + strings.Join(lines, "\n")
}

func composeChart(c llm.Chart) string {
	head := ""
	if v := strings.TrimSpace(c.Caption); v != "" {
		head += "### " + v + "\n\n"
	}
	if v := strings.TrimSpace(c.Description); v != "" {
		head += v + "\n\n"
	}
	data := nonEmptyRows(c.Data)
	if len(data) == 0 {
		return head + "_(chart data not extracted)_"
	}
	lines := []string{"| Label | Value |", "| --- | --- |"}
	for _, d := range data {
		value := ""
		if len(d) > 1 {
			value = d[1]
		}
		lines = append(lines, "| "+mdCell(d[0])+" | "+mdCell(value)+" |")
	}
	return head + strings.Join(lines, "\n")
}

func nonEmptyRows(rows [][]string) [][]string {
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		if len(r) > 0 {
			out = append(out, r)
		}
	}
	return out
}

func mdRow(cells []string) string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = mdCell(c)
	}
	return "| " + strings.Join(out, " | ") + " |"
}

// mdCell escapes pipes and folds newlines so a cell stays in its row.
func mdCell(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\r\n", " ")
	return strings.ReplaceAll(s, "\n", " ")
}
