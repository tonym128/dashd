# dashd — Project Review & Roadmap (`PLAN.md`)

A comprehensive review of `dashd` (as of `v0.4.1`) across **UI, UX, Security, Agent, Server, Installation, Data, and Reporting**, organized into what is already shipped and what remains to **develop**, **change**, and **remove**.

---

## Recently Completed (v0.2.0 – v0.4.1)
- Multi-machine tracking (`machine` column on `session`, `call`, `tool_call`, `machine_status` heartbeats, `?machine=` filter).
- Remote ingestion (`POST /api/ingest` with gzip + `ingestMu` serialization + 500-session cap) and `dashd push` agent (`push-state.db` default).
- Server → client pricing sync (`GET /api/models` with `ETag`/`304`, `models_etag` in `/api/ingest` ack) + `dashd update-models` + `-models-refresh`.
- Split authentication (`-ingest-token` / `DASHD_INGEST_TOKEN` vs `-auth-token` / `DASHD_AUTH_TOKEN`).
- Sticky topbar, view tabs (`Overview`, `Sessions`, `Breakdowns`, `System`, `All`), `Machines` breakdown & `Connected Machines` panel, **Cache hit %** on Total Tokens card, `60s` `/healthz` new-data indicator, and `scanSummary` machine-push status.
- Interactive installers ([packaging/install.sh](packaging/install.sh) with `--upgrade`/`--uninstall`, [packaging/install.ps1](packaging/install.ps1)) and [docker-compose.yml](docker-compose.yml) with `/healthz` healthcheck.

---

## 1. UI (`internal/web/templates/index.html`, `dashboard.css`, `dashboard.js`)

### Develop
- **Interactive Chart Tooltips**: Daily Spending and Activity & Throughput bars currently rely on native HTML `title=` attributes, which have hover delay and don't work on touch or keyboard focus. Add a lightweight floating popover showing exact cost, token split, and date/bucket on hover/focus.
- **Session Detail (`session.html`) Enhancements**: Display the originating `Machine` badge, per-model cost subtotal within the session, and a `Copy resume command` button directly on `/session`.
- **Compact / Detailed Table Column Toggle**: Allow toggling secondary token sub-columns (`Cache Write`, `Reasoning`) on `Models`, `Model Providers`, and `Machines` to reduce horizontal scrolling on laptops.

### Change
- **Replace `<select multiple size="5">` in `#filter-panel`**: Native multi-select boxes require `Ctrl/Cmd+Click` and are awkward on mobile/touch. Replace them with searchable checkbox popovers synced to the form inputs so multi-filtering is one-click and works cleanly on all devices.
- **Hide Local Scanner Table on Remote-Only Servers**: In the `System` tab, when `len(.Scan) == 0 && len(.Machines) > 0`, hide `#scan-table` completely rather than rendering a single placeholder row underneath `Connected Machines`.

### Remove
- **Hardcoded Inline Widths on `.bar-container`**: Move `style="width: 100px; display: inline-block; vertical-align: middle;"` in `dashboard.js` into `.bar-container` in `dashboard.css`.

---

## 2. UX

### Develop
- **Empty Filtered State "Reset filters" CTA**: When filters match 0 rows, add a one-click **"Reset filters"** button inside empty table states instead of plain text.
- **Unpriced Calls Filter Shortcut**: Make the `Unpriced Calls` stat card clickable (e.g. `?unpriced=1`) so users can immediately see which model names lack rates without hunting through `/session` pages.
- **CSV / JSON Export**: Add an export button on Sessions, Models, Projects, Machines, and Daily Spending for accounting/reimbursement workflows.

### Change
- **Client-Side / Partial Filter Updates**: Submitting `#filter-form` currently triggers a full `GET /?...` page reload. Fetching updated JSON or preserving scroll/tab state smoothly would feel snappier.
- **Auto-Refresh Toggle**: The 60s `/healthz` poll currently highlights `Refresh (new data)`. Add an optional auto-reload toggle (`Off / Auto`) in the topbar for wall-mounted or always-open monitor tabs.

### Remove
- **Dead/Redundant `console.error` Only Failures**: Surface user-visible error toasts (`showDashdToast`) for any failed `/api/activity` fetch as well as clipboard failures.

---

## 3. Security (`internal/web/auth.go`, `headers.go`, `ingest.go`)

### Develop
- **Rate Limiting on `/api/ingest` and `/healthz`**: Add lightweight per-IP token-bucket rate limiting on `/api/ingest` to protect against misbehaving clients or accidental loops.
- **Optional Browser Login Page for `-auth-token`**: When `-auth-token` is set and a user visits `/` in a browser without a reverse proxy injecting `Authorization`, serve a minimal login form that sets an `HttpOnly; SameSite=Strict; Secure` session cookie instead of a raw `401 unauthorized`.
- **Audit Log for Admin/Ingest Rejections**: Log structured `WARN` events (rate-limited) on repeated `401`s or oversized `/api/ingest` payloads.

### Change
- **Strict Gzip Decompression Ratio Guard**: `handleIngest` limits decompressed JSON to `32 MiB` via `io.LimitReader(gz, maxIngestBytes)`, which is solid; also return `413 Request Entity Too Large` explicitly when `MaxBytesReader` or the limit is hit instead of a generic `invalid json: unexpected EOF`.

### Remove
- **Plaintext Token in `com.dashd.push.plist` Example**: Document reading `DASHD_INGEST_TOKEN` from a `0600` file or Keychain rather than pasting it directly into a world-readable plist where possible.

---

## 4. Agent (`cmd/dashd/push.go`, `internal/scan/scan.go`)

### Develop
- **Batched `POST /api/ingest` Flushes**: Currently `remoteSink.RecomputeSession(uid)` sends one HTTP POST per changed session file. On a cold initial push of 450+ files, that's 450+ sequential requests. Batching up to 25 sessions per `POST /api/ingest` (and deferring `SaveScanState` until the batch succeeds) will make initial syncs 10x–20x faster.
- **Exponential Backoff on Remote Failure**: When the remote server is unreachable (laptop offline / Tailnet down), `dashd push` retries every `-interval` (30s). Add jittered exponential backoff (capped at 5m) and reset on success.
- **`dashd doctor` Subcommand**: A diagnostic command that checks local agent log directories, `models.json` path/age, `push-state.db` cursor counts, and connectivity/auth against `-remote`.
- **`dashd self-update` Subcommand**: Download and atomically replace the running binary from GitHub Releases (especially useful on Windows/macOS).

### Change
- **Cross-Machine `session.uid` Collision Safety**: `session.uid` is the primary key in `session`. While UUIDs won't collide, date/ordinal-based UIDs on two machines could overwrite each other on the central server. Namespace `session.uid` by `machine` on ingest (or key `session` by `(machine, uid)`).
- **Periodic Empty Heartbeat**: If no files changed during a `push` pass, `remoteSink` currently makes zero HTTP calls—meaning `Connected Machines` only updates when a session changes. Send a lightweight empty heartbeat every ~5 minutes so idle machines still show as online.

### Remove
- **Redundant `models.json` Copy in `install.sh` / `install.ps1` When `update-models` Succeeds**: Minor, keep release `models.json` as offline fallback.

---

## 5. Server (`cmd/dashd/main.go`, `internal/web/ingest.go`, `Dockerfile`, `docker-compose.yml`)

### Develop
- **Auto-Reprice After Server `models.json` Refresh**: When `runModelsRefresher` updates `models.json` on the server, optionally run `Store.Reprice` so previously unpriced calls are retroactively priced.
- **Prometheus `/metrics` Endpoint**: Optional metrics (`dashd_calls_total`, `dashd_cost_usd_total`, `dashd_tokens_total`, `dashd_machine_last_seen_seconds`) for Grafana/Prometheus monitoring.

### Change
- **Log Resolved `models.json` Path on Startup**: Log the exact resolved path at `INFO` level on startup so Docker/systemd users can immediately verify which price dump is loaded.

### Remove
- **In-Process `5000` Session Cap in `buildPayload`**: Currently `st.Sessions(ctx, filter, 5000)` embeds up to 5,000 sessions inline into `window.dashboardData` on every `GET /`. Move **All Sessions** to a paginated `/api/sessions` endpoint so initial page loads stay fast past 10,000+ sessions.

---

## 6. Installation (`packaging/install.sh`, `packaging/install.ps1`, `.goreleaser.yml`)

### Develop
- **`-Upgrade` and `-Uninstall` in `install.ps1`**: Bring Windows PowerShell installer to parity with `install.sh`'s `--upgrade` and `--uninstall` flags.
- **AUR `PKGBUILD` & Homebrew Formula**: Publish native packages for Arch/CachyOS (`paru -S dashd`) and macOS (`brew install tonym128/tap/dashd`).

### Change
- **`windows/arm64` Build in `.goreleaser.yml`**: Remove the `windows/arm64` ignore entry so Snapdragon Windows laptops get a native binary too.

### Remove
- **`update_models.py` Script**: Now that `dashd update-models` is built into the Go binary, `update_models.py` is redundant outside legacy dev workflows and can be removed or moved to `scripts/`.

---

## 7. Data (`internal/model/`, `internal/store/`, `internal/source/`)

### Develop
- **Retention / Compaction (`dashd prune --before YYYY-MM-DD`)**: Allow pruning raw `call` / `tool_call` rows older than a retention cutoff (with `VACUUM`) for very large long-running servers.
- **Unpriced Model Breakdown (`dashd stats --unpriced`)**: List every unpriced model name and its call count so users know exactly which models need rates.

### Change
- **Prevent Orphan Sweeps on Remote Sessions**: Ensure `markVanished` only marks sessions belonging to the local machine (or never runs in `serve`-only mode) so remote-pushed sessions are never flagged orphaned just because their file paths don't exist on the server.
- **Record `ingested_at` Timestamp**: Track when the server received a call (`ingested_at`) alongside `ts` to help debug clock skew or delayed offline laptop pushes.

### Remove
- **Legacy Pre-v6 Empty `machine` Ambiguity**: In `Store.Facets` / `Machines()`, include `'local'` when empty `machine` rows exist so legacy local sessions remain filterable alongside named machines.

---

## 8. Reporting (`internal/web/burnrate.go`, `activity.go`, `render.go`)

### Develop
- **Weekly & Monthly Spending Rollup Toggle**: Add a `Daily | Weekly | Monthly` toggle on the Daily Spending chart for long-term trend analysis.
- **Estimated Cache Savings ($)**: Pair `Cache hit %` with estimated dollars saved by cache reads vs. full input pricing.
- **Budget Thresholds (`-daily-budget`, `-monthly-budget`)**: Highlight burn-rate cards in red when projected spend exceeds configured daily/monthly budgets.

### Change
- **Cost-Per-Call & Cost-Per-Session Columns**: Add `Avg $/call` or `Avg $/session` to Models and Projects tables to distinguish expensive single calls from chatty loops.

### Remove
- **Inline 5,000-Session JSON Blob**: Covered under Server/UI pagination (`/api/sessions`).
