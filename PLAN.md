# dashd — Project Review & Roadmap (`PLAN.md`)

A full project review of `dashd` at **`v0.6.1`** across **UI, UX, Security, Agent, Server, Installation, Data, and Reporting**, listing shipped foundations and remaining items to **develop**, **change**, and **remove**.

---

## Shipped Through v0.6.1
- **Multi-machine & remote push**: `machine` column (`v6Machine`), `machine_status` heartbeats (`v7MachineStatus`), `POST /api/ingest` (gzip + `ingestMu` + 500-session cap + `413` guard), `dashd push` (`push-state.db`, exponential backoff, `5m` idle heartbeats), and `dashd doctor`.
- **Pricing & sync**: `dashd update-models`, `-models-refresh` with auto-reprice on `serve`/`serve-and-scan`, `GET /api/models` (`ETag`/`304`), and `models_etag` sync on `/api/ingest`.
- **Security**: Split `-ingest-token` (`DASHD_INGEST_TOKEN`) vs `-auth-token` (`DASHD_AUTH_TOKEN`), CSP with per-response nonce, constant-time token comparison, and `escapeHtml` attribute guards.
- **UI, UX & Mobile**: Sticky topbar, view tabs (`Overview`, `Insights`, `Sessions`, `Breakdowns`, `System`, `All`), `MTD` + `Today`/`7d`/`30d`/`All time` presets, `Daily | Weekly | Monthly` spending rollups, `Compact columns` mobile toggle, tap/hover chart tooltips, `Machines` breakdown, `Connected Machines` panel, and `Cache hit %`.
- **Insights & Reporting**: `Store.InsightSignals` + `BuildInsights` powering **Key Insights** (Overview), the **Insights** tab, and `dashd insights` CLI.
- **Installers & Docker**: Interactive [install.sh](packaging/install.sh) (`--upgrade`, `--uninstall`) and [install.ps1](packaging/install.ps1) (`-Upgrade`, `-Uninstall`), `windows/arm64` in [.goreleaser.yml](.goreleaser.yml), and [docker-compose.yml](docker-compose.yml) with `/healthz` healthcheck.

---

## 1. UI (`internal/web/templates/index.html`, `dashboard.css`, `dashboard.js`)

### Develop
- **Searchable Multi-Select Filter Popovers**: Replace `<select multiple size="5">` in `#filter-panel` with compact searchable checkbox dropdowns so users can multi-select Models, Programs, Projects, and Machines on desktop and mobile without `Ctrl/Cmd+Click`.
- **Session Detail (`/session`) Upgrades**: Add a `Copy resume command` button, per-model cost breakdown within the session, and a `Filter by this project` link directly on `/session`.
- **Insight Dismissal / Pinning**: Allow dismissing low-priority `info` insights in `localStorage` so users can keep the **Key Insights** strip focused on actionable warnings.

### Change
- **Inline `<style>` Cleanup**: Move remaining inline `style="..."` attributes in `dashboard.js` (e.g., `.bar-container` width `100px`) into `dashboard.css` classes.
- **Responsive Mobile Card Layout for Sessions (Optional)**: Even with `Compact columns` and sticky first column, `All Sessions` has 10 columns; on `< 480px` screens, consider a cleaner 2-line summary cell (`Project + Machine + Date` stacked) so less horizontal scrolling is needed.

### Remove
- **Redundant `#scan-table` Empty Row Logic**: Already hidden on remote-only servers; simplify the fallback message for local-only installs when `Scan` is empty.

---

## 2. UX

### Develop
- **Unpriced Calls Clickable Filter (`?unpriced=1`)**: Make the `Unpriced Calls` stat card and `unpriced-calls` insight link directly to the unpriced sessions/calls so users can inspect which models lack rates without running `dashd stats`.
- **CSV / JSON Export**: Add an export button on Sessions, Models, Projects, Machines, and Daily Spending for expense reporting and offline analysis.
- **Auto-Refresh Toggle (`Off / 60s`)**: Complement the `60s` `/healthz` `Refresh (new data)` indicator with an optional auto-reload toggle for wall-mounted or always-open monitor tabs.

### Change
- **Fetch-Based Filter Updates**: Submitting `#filter-form` currently triggers a full page reload (`GET /?...`). Fetching updated payloads or preserving scroll position cleanly would make slice-and-dice filtering feel instant.
- **Activity Chart Error Feedback**: Surface a `showDashdToast` notification if `/api/activity` fails instead of silently failing.

### Remove
- **Hard-to-discover `All` Tab Clutter**: Keep `All` for power users, or move it into a subtle icon/overflow toggle so the primary tab bar stays even tighter on mobile.

---

## 3. Security (`internal/web/auth.go`, `headers.go`, `ingest.go`)

### Develop
- **Per-IP Rate Limiting on `/api/ingest` and `/healthz`**: Add lightweight token-bucket rate limiting on `/api/ingest` to protect against misbehaving clients or accidental push loops.
- **Optional Browser Login Page**: When `-auth-token` is set and someone opens `/` in a browser without a reverse proxy injecting `Authorization`, serve a minimal login form that sets an `HttpOnly; SameSite=Strict; Secure` cookie instead of a raw `401 unauthorized`.
- **Rate-Limited Auth Failure Logging**: Log repeated `401` rejections at `WARN` (throttled per IP) so brute-force or misconfigured-agent attempts are visible in server logs.

### Change
- **Gzip Decompression Limit**: `handleIngest` uses `io.LimitReader(gz, maxIngestBytes)`, which caps decompressed bytes at 32 MiB. Explicitly detect hitting `maxIngestBytes` on the decompressed stream and return `413` consistently (not just on `MaxBytesError` from the compressed body).

### Remove
- **Plaintext Token in `com.dashd.push.plist` Example**: Document sourcing `DASHD_INGEST_TOKEN` from a `0600` env file or macOS Keychain rather than embedding secrets directly in a plist template.

---

## 4. Agent (`cmd/dashd/push.go`, `internal/scan/scan.go`)

### Develop
- **Batched `POST /api/ingest` Flushes**: Currently `remoteSink.RecomputeSession(uid)` sends one HTTP POST per changed session file. On a cold initial push of 450+ files, that is 450+ sequential HTTP round trips. Batching up to 25 sessions per `POST /api/ingest` (and advancing `scan_state` cursors only after the batch succeeds) will make initial syncs 10x–20x faster.
- **`dashd self-update`**: Download and atomically replace the running binary from GitHub Releases (especially helpful on Windows/macOS).
- **Rate-Limit / 429 Log Detection**: Detect API rate-limit errors in agent JSONL logs (where present) and log/surface them as `Tools & Loops` / `Data Health` insights.

### Change
- **Cross-Machine `session.uid` Collision Safety**: `session.uid` is the primary key in `session`. While UUIDs won't collide, sequential or short IDs on two different machines could overwrite each other on the central server. Namespace `session.uid` by `machine` on remote ingest (or key `session` by `(machine, uid)`).

### Remove
- **Full-Path Leakage Option (`-anonymize-paths`)**: Optionally strip `$HOME` prefixes from `project` / `path` before pushing to a shared server if users only want repo names rather than local filesystem paths.

---

## 5. Server (`cmd/dashd/main.go`, `internal/web/ingest.go`, `Dockerfile`, `docker-compose.yml`)

### Develop
- **Prometheus `/metrics` Endpoint**: Optional metrics (`dashd_calls_total`, `dashd_cost_usd_total`, `dashd_tokens_total`, `dashd_machine_last_seen_seconds`) for homelab Grafana dashboards.
- **On-Demand `POST /api/reprice` Admin Endpoint**: Allow triggering `Store.Reprice` from the **System** tab after a manual `models.json` update without restarting the container.

### Change
- **Paginated `/api/sessions` Endpoint**: `buildPayload` still queries `st.Sessions(ctx, filter, 5000)` and embeds all sessions inline in `window.dashboardData`. Moving `All Sessions` to a server-paginated `/api/sessions` endpoint keeps initial HTML payloads small as multi-machine history grows past 10,000+ sessions.

### Remove
- **5,000-Session Inline Cap**: Replaced by `/api/sessions` pagination above.

---

## 6. Installation (`packaging/install.sh`, `packaging/install.ps1`, `.goreleaser.yml`)

### Develop
- **AUR `PKGBUILD` & Homebrew Formula**: Native packages for Arch/CachyOS (`paru -S dashd`) and macOS (`brew install tonym128/tap/dashd`).
- **Automated `install.sh` / `install.ps1` Smoke Test in CI**: Run shellcheck + PowerShell syntax check (`pwsh`) in `.github/workflows/ci.yml`.

### Change
- **Installer `--local` `models.json` Copy**: When `install.sh --local <bin>` is used, also copy sibling `models.json` if present next to `<bin>`.

### Remove
- **`update_models.py`**: Now that `dashd update-models` is built into the Go binary and runs in CI/Docker/agents, `update_models.py` is redundant and can be removed (or moved to `scripts/`).

---

## 7. Data (`internal/model/`, `internal/store/`, `internal/source/`)

### Develop
- **Retention / Compaction (`dashd prune --before YYYY-MM-DD`)**: Allow pruning raw `call` / `tool_call` rows older than a cutoff date (with `VACUUM`) for long-running servers.
- **Record `ingested_at` Timestamp**: Track when the central server received each call (`ingested_at`) alongside `ts` to debug clock skew or delayed offline laptop pushes.

### Change
- **Legacy Pre-v6 `machine = ''` Handling**: In `Store.Machines()` / `Facets`, include `'local'` if empty `machine` rows exist so pre-v6 sessions are filterable alongside named hosts.

### Remove
- **Manual Fallback Table Drift**: Keep `internal/source/manual_pricing.json` minimal as an emergency offline floor; rely on `dashd update-models` + `-models-refresh` + server→client sync as the primary rate source.

---

## 8. Reporting (`internal/web/burnrate.go`, `insights.go`, `render.go`)

### Develop
- **Estimated Cache Savings ($)**: Pair `Cache hit %` with estimated dollars saved (`cache_read_tokens * (input_rate - cache_read_rate)`) in both **Total Tokens** and **Insights**.
- **Budget Thresholds (`-daily-budget`, `-monthly-budget`)**: Highlight burn-rate cards and generate a `warn` insight when projected spend exceeds configured daily/monthly budgets.
- **Cost-Per-Call & Cost-Per-Session Columns**: Add `Avg $/call` to Models and `Avg $/session` to Projects/Machines tables.

### Change
- **Timezone Clarity on Multi-TZ Aggregates**: Note the server's timezone in `Daily Spending` and `Burn` notes when aggregating logs pushed from machines in different timezones.

### Remove
- **Unused Legacy `window.dashboardData` Duplication**: Once `/api/sessions` is extracted, remove duplicate `sessions_list` copies nested under `projects` in `render.go`.
