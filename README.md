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

**Status: 0.1.0.** Editor, preview, autosave and revision history,
Transform, chat, image paste and source removal work. Adding new
sources (fetched through forager) arrives in the next release; until
then the add-source form reports that it isn't available yet.

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
  -v ~/writing:/data ghcr.io/scheff-of-staff/scheff-writer:0.1.0
```

The container runs as a non-root user; give it a uid that can write
the mounted folder (`--user 1000:1000` or similar).

## Security

- **Sign-in is required** unless `WRITER_ALLOW_NO_AUTH=1` is set
  explicitly. Browsers sign in once with the token and get an
  HttpOnly, SameSite=Strict, Secure cookie holding an HMAC of the
  token, not the token itself. Other services send
  `Authorization: Bearer <token>`.
- **Cross-site writes are refused** (Go's `CrossOriginProtection`), so
  another web page can't post into the editor's API.
- **Drafts are untrusted input to the model.** The prompts say so, and
  neither Transform nor chat writes anything on the model's say-so:
  Transform output is shown for review, and only Accept saves it.
- Run it behind TLS (Tailscale Serve, a reverse proxy) on a private
  network. It is a single-author tool, not a multi-user service.

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
