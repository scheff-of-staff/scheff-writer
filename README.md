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

**Status: P2a, the storage layer.** The project folder format, scoped
writes and tests are in place. The web service, editor UI, Transform,
chat and forager-backed source ingest follow (P2b–P2d).

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

No third-party dependencies.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
