# dashd — Project Review & Roadmap (`PLAN.md`)

A comprehensive review of `dashd` across **UI, UX, Security, Agent, Server, Installation, Data, and Reporting**, organized into concrete items to **develop**, **change**, and **remove**.

---

## 1. UI (`internal/web/templates/index.html`, `dashboard.css`, `dashboard.js`)

### Develop
- **Machine Breakdown Panel & Chart**: `machine` is now stored on `session`, `call`, and `tool_call` and filterable via `?machine=`, but there is no dedicated **Machines** breakdown table (like Models/Providers/Projects) showing cost, calls, tokens, and last seen per host.
- **Interactive Chart Tooltips**: Daily Spending and Activity & Throughput bars currently rely on native HTML `title=` attributes, which have hover delay and don't work on touch or keyboard focus. Add a lightweight floating popover showing exact cost, token split, and date/bucket on hover/focus.
- **Session Detail (`session.html`) Enhancements**: Display the originating `Machine` badge, per-model cost subtotal within the session, and a `Copy resume command` button directly on the session detail page.

### Change
- **Replace `<select multiple size="5">` in `#filter-panel`**: Native multi-select boxes require `Ctrl/Cmd+Click` and are awkward on mobile/touch. Replace them with searchable checkbox popovers synced to the form inputs so multi-filtering is one-click and works cleanly on all devices.
- **Table Column Priority & Sticky First Column**: `Models` and `Model Providers` (11 columns) and `All Sessions` (10 columns) scroll horizontally on laptops/tablets. Pin the first column (`position: sticky; left: 0`) and add a "Compact / Detailed" column toggle (hiding `Cache Write`, `Reasoning`, etc. in compact mode).
- **Scanner Status on Remote-Only Servers**: When `dashd serve` runs as a central receiver for `dashd push` agents, the `Scanner` table and `Sources` panel reflect the server's empty local home directory (`no logs`), which looks like an error. Switch the `System` tab to show **Connected Machines / Last Push** alongside local scanner status, and suppress the `SourcesNeedAction` warning when `serve`-only mode has ingested remote sessions.

### Remove
- **Duplicate `Refresh for latest` Link in `.page-header`**: Now that the sticky `.topbar` has a dedicated `Refresh` button, the inline `Generated on ... · Refresh for latest` link in `.page-header` is redundant.

---

## 2. UX

### Develop
- **Live / Auto-Refresh Option**: Add an optional auto-refresh toggle (`Off / 30s / 60s`) or SSE/polling `/healthz` check in the topbar that shows a subtle `"New data available — Refresh"` pill when `calls` increases, rather than forcing manual reloads.
- **Empty Filtered State CTA**: When filters match 0 rows, tables currently say `"No sessions in this view."` Add a one-click **"Reset filters"** button inside empty filtered states.
- **CSV / JSON Export**: Add an export button on Sessions, Models, Projects, and Daily Spending for accounting/reimbursement workflows.

### Change
- **Filter Apply Without Losing Tab State**: Submitting `#filter-form` performs a full `GET /?...` reload, which drops the `#hash` if the user wasn't careful (though `localStorage` restores it). Preserve the active view tab seamlessly and consider client-side or `fetch`-based partial updates for faster filtering.
- **Unpriced Calls Visibility**: Surface a clickable banner or filter shortcut (`?unpriced=1`) when `Totals.UnpricedCalls > 0` so users can immediately see which model names lack rates instead of hunting through `/session` pages.

### Remove
- **Native `alert`/silent copy failures**: `copyResumeCommand` logs to `console.error` on clipboard failure; surface an error toast (`showDashdToast`) if clipboard write fails.

---

## 3. Security (`internal/web/auth.go`, `headers.go`, `ingest.go`)

### Develop
- **Per-Machine / Scoped Ingest Tokens (or Read vs. Write Token Split)**: Currently `-auth-token` is a single shared secret for both reading the web UI and pushing via `POST /api/ingest`. Separating `-ingest-token` (write-only for agents) from `-auth-token` (UI read access) limits blast radius if an agent machine is compromised.
- **Rate Limiting / Body & Concurrency Guards on `/api/ingest`**: `handleIngest` caps request size at `32 MiB`, which is good, but has no per-IP/per-request session count cap or rate limit. Add a max sessions-per-request guard and a gzip-bomb decompression limit (already `io.LimitReader(gz, maxIngestBytes)`, keep this!).
- **Browser Auth UX**: When `-auth-token` is enabled and a user opens the dashboard in a browser without a proxy injecting `Authorization`, they get a raw `401 unauthorized`. Optionally support an HTTP-only signed session cookie login form (when `-auth-token` is set) or document the reverse-proxy pattern clearly.

### Change
- **`/api/models` Auth Alignment**: Ensure `/api/models` stays behind `WithAuth` (it already is, since only `/healthz` is exempt in `auth.go`), and validate that `s.ModelsPath` is strictly the configured file path (no user-controlled path).
- **CSP Tightening Audit**: Keep the per-response nonce and `escapeHtml` discipline (`jsxss_regress_test.go`). Avoid adding any inline event handlers or third-party CDNs.

### Remove
- **`X-Auth-Token` Query/Header Ambiguity**: Keep `Authorization: Bearer` and `X-Auth-Token` headers only; never accept tokens in URL query parameters (already avoided, keep it that way).

---

## 4. Agent (`cmd/dashd/push.go`, `internal/scan/scan.go`, `internal/source/`)

### Develop
- **Batched `POST /api/ingest` Flushes**: Currently `remoteSink.RecomputeSession(uid)` sends one HTTP POST per changed session file. On an initial cold push of 450+ files, that's 450+ sequential HTTP requests. Batching up to N sessions (e.g., 25 sessions or 2 MiB uncompressed) per `POST /api/ingest` while only advancing `scan_state` cursors for the sessions in a successful batch will make initial pushes 10x–20x faster.
- **Exponential Backoff & Offline Resilience**: If the remote server is unreachable (e.g., laptop offline / Tailnet down), `dashd push` retries every `-interval` (30s) and logs errors. Add jittered exponential backoff (capped at e.g. 5m) and clean structured logging (`"remote unreachable, will retry"`).
- **`dashd push status` / `dashd doctor` Subcommand**: A quick diagnostic command that checks local agent log directories, `models.json` status, `push-state.db` cursor counts, and connectivity/auth against `-remote`.
- **Self-Update Command (`dashd self-update`)**: Optional command to check GitHub Releases and replace the local binary (especially helpful on Windows/macOS where there's no package manager).

### Change
- **Session UID Namespacing Across Machines**: `session.uid` is currently the table's `PRIMARY KEY`. While UUID-based sessions won't collide across machines, date/ordinal-based UIDs (e.g. `session-1` or identical fixture/test filenames) on two machines would overwrite each other on the central server. Consider keying `session` by `(machine, uid)` or prefixing `uid` on remote ingest when `machine != ""` (while keeping `/session?uid=...` clean).
- **Cursor State Isolation**: `dashd push` defaults `-db` to `~/.local/share/dashd/dashboard.db` unless overridden. Give `push` its own default path (`~/.local/share/dashd/push-state.db`) so running `dashd scan` and `dashd push` on the same machine never share/interfere with each other's cursors.

### Remove
- **Local `Scanner` Errors on `context.Canceled` During Shutdown**: Notice in `journalctl` during `systemctl --user restart dashd-push`, stopping mid-pass logged `level=ERROR msg="source scan failed" error="context canceled"`. Treat `context.Canceled` during shutdown as `INFO`/`DEBUG`, not `ERROR`.

---

## 5. Server (`cmd/dashd/main.go`, `internal/web/ingest.go`, `Dockerfile`, `docker-compose.yml`)

### Develop
- **Auto-Reprice on Server `models.json` Refresh**: When the server runs with `-models-refresh 24h` and downloads updated pricing, optionally run `Store.Reprice` automatically (or expose a `POST /api/reprice` admin action) so historical unpriced calls get priced as soon as OpenRouter adds new models.
- **Per-Machine Heartbeat / Last-Seen Tracking**: Record a `machine_status` row `(machine, last_seen_at, agent_version, calls_pushed)` on each `POST /api/ingest` (even when `sessions` is empty, as a lightweight heartbeat) so the **System** tab shows every connected machine, its version, and when it last checked in.
- **Prometheus `/metrics` Endpoint (Optional)**: Export `dashd_calls_total`, `dashd_cost_usd_total`, `dashd_tokens_total`, and `dashd_last_push_timestamp` for homelab Grafana setups.

### Change
- **Docker Compose Default Env & Healthcheck**: Add a `healthcheck` block to `docker-compose.yml` hitting `http://127.0.0.1:8753/healthz`, and allow `DASHD_AUTH_TOKEN` to be optional in `docker-compose.yml` (with a clear comment) for Tailnet-only deployments like `dashd.boa-hake.ts.net`.
- **SQLite Write Contention Under Multi-Agent Load**: `handleIngest` writes directly to `s.store` from HTTP handlers. With multiple machines pushing concurrently, `SetMaxOpenConns(4)` + `busy_timeout(10000)` works, but routing all writes through a single serialized writer channel/queue inside `Server` eliminates `SQLITE_BUSY` retries under bursty concurrent pushes.

### Remove
- **Redundant `models.json` Discovery Confusion**: Simplify `defaultModelsPath()` documentation and log the resolved absolute path on startup so container/systemd users immediately see which `models.json` is active.

---

## 6. Installation (`packaging/install.sh`, `packaging/install.ps1`, `.goreleaser.yml`)

### Develop
- **Upgrade Mode in `install.sh` / `install.ps1`**: Add `--upgrade` (or detect existing `~/.config/dashd/push.env` / Task Scheduler task) so users can update the binary in-place without re-entering their server URL, token, and machine name.
- **Uninstall Script / Flag (`--uninstall`)**: Cleanly stop/disable the `systemd --user` unit, `launchd` plist, or Windows Scheduled Task and optionally remove binaries/state.
- **Homebrew Tap / AUR `PKGBUILD`**: Since the user is on CachyOS (Arch-based) and supports macOS, publishing a `PKGBUILD` (AUR) and Homebrew formula alongside `.goreleaser.yml` would make upgrades native (`paru -S dashd` / `brew upgrade dashd`).

### Change
- **`packaging/dashd-push.service` & `com.dashd.push.plist` Consistency**: Ensure both static templates include `-models` and `-models-refresh 24h` (updated in `dashd-push.service`; verify `com.dashd.push.plist` matches).
- **Release Tagging Discipline**: Ensure `.goreleaser.yml` builds `windows/arm64` if needed (currently ignored) or document `windows/amd64` only.

### Remove
- **`update_models.py` Dependency in Docs/Errors**: Error messages in `cmd/dashd/main.go` and `internal/source/pricing.go` still tell users `"run python3 update_models.py"`. Now that `dashd update-models` is built into the Go binary, update those messages to recommend `dashd update-models` (and keep `update_models.py` only as a repo dev helper or remove it).

---

## 7. Data (`internal/model/`, `internal/store/`, `internal/source/`)

### Develop
- **Retention / Compaction Policy (Optional)**: Storing every individual `call` row is great for accuracy, but after 1M+ calls the SQLite DB grows. Adding an optional `dashd vacuum` / `dashd prune --before YYYY-MM-DD` (or rollup-only archiving) keeps long-lived servers fast.
- **Orphaned Session Handling for Remote Machines**: On a remote server, `ClearOrphaned` / `MarkOrphaned` shouldn't mark pushed sessions from other machines as orphaned just because those paths don't exist on the server. Ensure `serve` mode never runs local orphan-sweeping on remote-machine sessions.
- **Data Integrity Check (`dashd stats --verify`)**: Report unpriced models by name + call count, calls with `ts = 0`, and per-machine row counts in `dashd stats`.

### Change
- **Store `agent_version` and `ingest_ts`**: Recording when the server ingested a call (`ingested_at`) helps debug clock skew or delayed offline laptop pushes.
- **Backfill / Label Empty `machine` Values**: Legacy rows from before `v6Machine` have `machine = ''`. In queries/facets, optionally map `''` to `"local"` (or surface `"unassigned"`) so legacy sessions aren't invisible when filtering by machine.

### Remove
- **Hardcoded Fallback Drift**: `internal/source/manual_pricing.json` requires manual edits whenever new models launch. Keep it as a minimal emergency floor, and rely on `dashd update-models` + `-models-refresh` + server→client sync as the primary mechanism.

---

## 8. Reporting (`internal/web/burnrate.go`, `activity.go`, `render.go`)

### Develop
- **Cache Efficiency & Savings Metrics**: Surface **Cache Hit Rate** (`cache_read / (input + cache_read + cache_write)`) and estimated **Cache Savings ($)** on the Overview stat cards and per-model breakdown—this is one of the highest-signal cost-optimization metrics for coding agents.
- **Per-Machine & Per-Project Cost Comparison**: Add a **Machines** tab/table under Breakdowns and allow comparing spend across machines (e.g. `cachiest` vs `macbook` vs `work-vm`).
- **Budget Alerts / Thresholds**: Allow setting a daily/monthly soft budget (`-daily-budget 20`, `-monthly-budget 300`) that highlights burn-rate cards in red when projected spend exceeds budget, and optionally exposes status on `/healthz` or webhook.
- **Weekly / Monthly Rollup View**: Daily Spending shows individual days + month separators; adding a **Weekly** and **Monthly** toggle makes multi-month trends much easier to read.

### Change
- **Cost-Per-Call & Cost-Per-Session Averages**: Add `Avg $/call` and `Avg $/session` to the Models and Projects breakdown tables to make expensive vs. chatty models obvious.
- **Timezone Consistency**: Ensure `burnWindows` and `Daily Spending` clearly indicate the server/browser timezone used for day boundaries when aggregating logs pushed from machines in different timezones.

### Remove
- **Unused/Redundant Payload Fields**: Audit `window.dashboardData` in `render.go` as the database grows (100k+ calls / 1k+ sessions): paginating or lazy-loading `sessions_list` via `/api/sessions` instead of embedding up to 5,000 sessions inline in `index.html` will keep initial page loads fast as multi-machine history scales.

---

## Suggested Implementation Order

1. **Quick Wins (High Impact, Low Risk)**:
   - Update `"run python3 update_models.py"` error messages to `dashd update-models`.
   - Give `dashd push` its own default `-db` (`push-state.db`) and downgrade shutdown `context.Canceled` scan logs from `ERROR` to `INFO`.
   - Suppress local `SourcesNeedAction` warning when `serve`-only has remote sessions.
2. **Agent & Server Scalability**:
   - Batch `POST /api/ingest` payloads in `remoteSink`.
   - Add `machine_status` heartbeats and a **Machines** breakdown table in the UI.
   - Route server writes through a serialized writer queue and paginate `/api/sessions`.
3. **Reporting & UX Polish**:
   - Add Cache Hit Rate & Savings metrics, Weekly/Monthly spending toggle, interactive chart tooltips, and searchable filter dropdowns.
   - Split `-ingest-token` vs `-auth-token` for least-privilege deployments.
