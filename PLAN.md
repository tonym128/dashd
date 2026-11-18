# dashd — roadmap (`PLAN.md`)

Status of the full project review (2026-11-18). Shipped items are described in
[CHANGELOG.md](CHANGELOG.md) under *Unreleased*; this file tracks what is left.

## Done in this round

Security (Host/Origin/CSRF guards, refusal to expose without a token, cookie
login, roles and admin token, machine-bound ingest tokens, token files, TLS,
throttled auth logging, checksum-verified installs and self-update, cosign
signing), push agent (batching, resync on server change, redaction, failure
heartbeats, `status`, auto-update), server (SQL paging, exports, admin
endpoints, payload cache, metrics as gauges), data (server-side pricing and
day bucketing, uid namespacing, prune floor and recompute, chunked reprice,
pre-migration backups, `backup`/`export`), reporting (forecast, cache savings,
comparison, tags, per-unit costs, webhook alerts and digests, filtered CLI
insights), UI (session page, saved views, auto-refresh, shortcuts, onboarding,
ARIA tabs, maintenance panel), installation (non-interactive installers,
`--server`, Windows arm64, packages, CI checks), Antigravity unknown-model
naming.

## Still open

### UI / UX
- **In-place filter updates.** Filtering still navigates; scroll position and
  the active tab are now kept, but a JSON re-render without reload would need
  the server-rendered cards (burn, stats, insights) to move into the payload.
- **Machine-vs-machine comparison.** The comparison is period-over-period
  only; the Machines table covers side-by-side totals.
- **Dynamic inline styles.** Bar widths and chart colours are still `style`
  attributes, so `style-src 'unsafe-inline'` stays. Moving them to CSS custom
  properties set from JS would allow dropping it.

### Frontend structure
- **ES modules and browser tests.** `dashboard.js` was split into
  `dashboard.js`, `app.js` and `session.js`, but they remain classic scripts:
  the node `vm` harness (`jsdom_test.go`) reads top-level bindings, which
  modules would hide. A module split needs that harness reworked first;
  Playwright would add browser coverage but needs browsers in CI.

### Agent
- **On-disk outbound queue.** Unsent batches are re-read from the logs on the
  next pass (cursors only advance after the server accepts a batch), which is
  durable as long as the logs exist. A queue would cover logs rotated while a
  laptop is offline.
- **Rate-limit (429) signal** from agent logs as an insight.
- **macOS Keychain** for the token (today: a 0600 file).

### Installation
- **winget** manifest and a native Windows service (today: a hidden,
  auto-restarting scheduled task).
- Run `goreleaser check` after these `.goreleaser.yml` changes (signs, nfpms,
  brews, scoops); it could not be run when they were written. The first
  tagged release should be watched, and the `homebrew-tap` / `scoop-bucket`
  repositories and their tokens created if those channels are wanted.

### Data
- **Fallback price table.** Record the date `manual_pricing.json` was
  generated and log it with the fallback warning; trim it to an emergency
  floor.
- **Free and local models** (OpenCode `*-free`, GGUF/local models) are counted
  as unpriced; a `pricing_overrides.json` entry pricing them at $0 would move
  them out of the unpriced warning. Paid models currently unpriced on the
  maintainer's machine include `gemini-3.8-pro` and `claude-fable-5-2`.
