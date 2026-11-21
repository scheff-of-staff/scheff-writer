# scheff-writer

A writing surface for long-form pieces: a Markdown editor with live
preview, an AI **Transform** pass that polishes a draft in your own
voice, a writing-assistant chat, research **sources** attached to each
piece, image attachments and full revision history.

It works **standalone** against any folder of Markdown, and **with
[Scheff of Staff](https://github.com/scheff-of-staff/scheffofstaff)**
by pointing it at the vault, where it owns each project's body and
its `sources/`, `revisions/`, `attachments/` and `chat.md`
(see the Scheff contracts, §1 and §3).

**Status: 0.3.0.** Editor, preview, autosave and revision history,
Transform, chat, image paste, and sources (add by URL, pasted text or
PDF upload; remove) all work. Chat is also available as a JSON API
(`/api/v1/`).

## Run it

Locally, against any folder, with no sign-in (localhost only):

```
make run ROOT=~/writing
```

Then open http://127.0.0.1:8445. For Transform and chat, also set
`WRITER_LLM_URL`, `WRITER_LLM_KEY` and `WRITER_MODEL`
(see [config.example.env](config.example.env)).

As a container:

```
docker run --rm -p 127.0.0.1:8445:8445 \
  -e WRITER_ROOT=/data -e WRITER_TOKEN="$(openssl rand -hex 24)" \
  -e WRITER_LLM_URL=... -e WRITER_LLM_KEY=... -e WRITER_MODEL=... \
  -v ~/writing:/data ghcr.io/scheff-of-staff/scheff-writer:0.3.0
```

The container runs as a non-root user; give it a uid that can write
the mounted folder (`--user 1000:1000` or similar).

## Security

- **Sign-in is required** unless `WRITER_ALLOW_NO_AUTH=1` is set
  explicitly. Browsers sign in once with the token and get an
  HttpOnly, SameSite=Strict, Secure cookie holding an HMAC of the
  token, not the token itself. Other services send
  `Authorization: Bearer <token>`.
- **The JSON API has its own token** (`WRITER_API_TOKEN`). It is the
  only credential `/api/v1/` accepts, and it opens nothing else, so
  revoking a script's access never signs you out.
- **Cross-site writes are refused** (Go's `CrossOriginProtection`), so
  another web page can't post into the editor's API.
- **Drafts are untrusted input to the model.** The prompts say so, and
  neither Transform nor chat writes anything on the model's say-so:
  Transform output is shown for review, and only Accept saves it.
- **The writer never fetches URLs itself.** URL sources go through
  [forager](https://github.com/scheff-of-staff/forager) (0.4 or
  newer), which refuses private and internal addresses, so an author
  can't point it at the host's own network. Without forager
  configured, only pasted text and PDF uploads are available.
- **Sources are untrusted input too.** Fetched pages, pasted text and
  PDFs go to the model with an instruction to ignore anything inside
  them and return JSON only; nothing in a source is executed or
  followed. PDF uploads are checked by content (`%PDF-`), capped at
  30 MB, and read in memory; only the extract is saved, never the file.
- Run it behind TLS (Tailscale Serve, a reverse proxy) on a private
  network. It is a single-author tool, not a multi-user service.

## JSON API

A versioned API for scripts and other services. It is off unless
`WRITER_API_TOKEN` is set. Requests send that token as
`Authorization: Bearer <token>` and a JSON body
(`Content-Type: application/json`, at most 1 MiB).

### `POST /api/v1/projects/{slug}/chat`

One turn of the same writing-assistant chat the editor uses: same
prompt, same profile and tone samples, same sources, same thread.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `message` | string | required | The question, at most 64 KB. |
| `draft` | string | `""` | The current draft, sent as context. |
| `model` | string | `WRITER_MODEL` | A model alias from `WRITER_API_MODELS`. |
| `history` | bool | `true` | Send the saved thread as context. |
| `persist` | bool | `true` | Append this turn to the project's `chat.md`. |

Use `"history": false, "persist": false` for runs that should neither
see nor change the thread (automated tests, batch jobs).

```bash
curl -sS https://writer.example/api/v1/projects/my-piece/chat \
  -H "Authorization: Bearer $WRITER_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"message":"Where is the argument weakest?","persist":false}'
```

```json
{"reply":"…","model":"your-model-name","usage":{"input_tokens":812,"output_tokens":140},
 "elapsed_ms":2310,"persisted":false,"history_turns":0}
```

Errors return `{"error": "…"}`:

| Status | When |
|---|---|
| 400 | Invalid JSON, unknown field, missing or oversized message, bad slug, model not allowed |
| 401 | Missing or wrong API token |
| 404 | No such project |
| 413 | Body over 1 MiB |
| 415 | Content type is not `application/json` |
| 429 | More than `WRITER_API_MAX_CONCURRENT` calls in flight (`Retry-After` is set) |
| 502 | The model call failed |

Logs record the slug, model, sizes, token counts and timing, never
the message, draft or reply.

## Sources

The editor's Sources tab adds a source three ways. Each becomes one
Markdown file under the project's `sources/`, which Transform and chat
read as grounding.

| Tab | Input | Needs |
|---|---|---|
| URL | A web page or a PDF link (up to 30 MB) | forager: `WRITER_FORAGER_URL`, `WRITER_FORAGER_TOKEN` |
| Text | Pasted text (up to 256 KB; the first 64 KB goes to the model) | nothing extra |
| PDF | An uploaded PDF (up to 30 MB) | nothing extra |

You pick the model (`claude-best`, `claude-fast`, `openai-best`,
`openai-fast`) and the depth (output budget: low 2K, medium 8K, high
16K tokens; `claude-fast` is held to 8K and the file records
`depth_clamped: true`). Pages and text give a summary and key points;
PDFs are sent whole to the model, which also transcribes tables and
charts. PDF reading needs a model that accepts PDF input through your
gateway (Claude models do).

A source file looks like this:

```markdown
---
type: source
project: my-piece
kind: url | text | pdf
added: 2026-11-21T15:04:05Z
title: "…"
url: "https://…"        (empty for text without a URL and for uploads)
model_used: claude-best
depth: medium
---

# Title

Summary…

## Key points
## Tables     (PDFs, when present)
## Charts     (PDFs, when present)
```

At most two sources are added at once (large PDFs are memory-heavy);
a third gets "try again in a moment". Logs record the project, file
name, kind, model, depth, sizes, tokens and timing, never the content.

## Folder layout

```
<root>/04_Projects/<slug>.md             front matter, body, ## Log
<root>/04_Projects/<slug>/revisions/     YYYY-MM-DD-HHMMSS.md snapshots
<root>/04_Projects/<slug>/sources/       YYYY-MM-DD-HHMMSS-<title>.md extracts
<root>/04_Projects/<slug>/attachments/   YYYY-MM-DD-HHMMSS-<hex>.<png|jpg|webp|gif>
<root>/04_Projects/<slug>/chat.md        "## User <time>" / "## Assistant <time>" turns
<root>/06_Reference/profile.md, tone/    voice references (read only)
```

Rules the store enforces:

- **Body saves never touch `## Log`.** Everything from the first
  `## Log` line onward is preserved verbatim, because other tools
  append there. Unknown front-matter keys are kept.
- **Revisions on every substantive save.** A re-save with identical
  bytes within a minute is skipped, so autosave doesn't flood history.
- **Scoped writes.** Slugs and file names are validated and every path
  is re-checked after cleaning to stay inside the project's own files.
- **Images are checked by content,** not just declared type (PNG,
  JPEG, WebP, GIF; 10 MB max).

## Development

```
go test ./...
```

One third-party dependency: [goldmark](https://github.com/yuin/goldmark)
(MIT), pinned, for Markdown rendering with raw HTML disabled.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
