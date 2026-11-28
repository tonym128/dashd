# dashd

A long-running cost dashboard for coding agents, written in Go.

> `dashd` is a ground-up Go rewrite of the previous Python codebase, [agent-cost-dashboard](https://github.com/tonym128/agent-cost-dashboard).

`dashd` reads the session logs that **pi**, **Claude Code**, **Codex CLI**, **Gemini CLI**, **Antigravity**, and **OpenCode** write, stores them in a SQLite database, and serves an interactive web dashboard from that database.

The scanner and web server are cleanly split: a background scanner owns writes while the HTTP server only reads. SQLite runs in WAL mode, so page loads see a consistent snapshot even while a scan is in progress, and either half can be run independently.

## Quick start

```bash
go build -o dashd ./cmd/dashd

# Run one scan pass, print what was found, and exit
./dashd scan

# Serve the dashboard and scan every 30 seconds in the background
./dashd serve-and-scan
# → http://127.0.0.1:8753
```

## Commands

| Command | What it does |
|---|---|
| `serve-and-scan` | Serve the dashboard and scan on a timer (default) |
| `serve` | Serve only; data arrives from `dashd push` agents (accepts ingest) |
| `scan` | One pass, then exit — for cron or a systemd timer |
| `push` | Scan local logs and push them, in batches, to a remote `dashd` server |
| `status` | Show this machine's push state: last success, last error, server version |
| `reprice` | Recompute every stored cost (and day bucket) at the price in force when each call was made |
| `prune` | Delete calls before `-before YYYY-MM-DD`; pruned history is not re-ingested |
| `backup` | Write a consistent copy of the database to `-out` (safe while serving) |
| `export` | Write calls as CSV or JSON (`-format`, `-out`, filter flags; supports `-remote`) |
| `digest` | Print a Markdown usage digest, or POST it to `-webhook-url` (supports `-remote`) |
| `insights` | Print usage insights (`-json`, filter flags; supports `-remote`) |
| `query` | Run a read-only SQL query (`-sql`) or inspect `-schema` (local DB or `-remote`) |
| `mcp` | Run a read-only Model Context Protocol (MCP) server over stdio (local DB or `-remote`) |
| `proxy` | Transparent HTTP proxy for `ollama`, `llama-server`, OpenAI and Anthropic APIs (local DB or `-remote`) |
| `update-models` | Download the latest OpenRouter model pricing to `-models` |
| `self-update` | Install the latest release (checksum-verified, never downgrades) |
| `doctor` | Check agent logs, pricing, the database and the remote server |
| `stats` | Print a summary of the database and the last scan of each source (supports `-remote`) |

## Common flags

`dashd -h` lists every flag. The ones most setups need:

| Flag | Default | Meaning |
|---|---|---|
| `-db` | `~/.local/share/dashd/dashboard.db` | Database path |
| `-addr` | `127.0.0.1:8753` | Listen address; a non-loopback address requires a token |
| `-auth-token-file` / `-auth-token` | none | Dashboard token (also `DASHD_AUTH_TOKEN`); browsers sign in at `/login` |
| `-read-token-file` / `-read-token` | none | Read-only token for dashboard, read APIs, `/api/query` and `/mcp` (also `DASHD_READ_TOKEN`); cannot ingest or run admin actions |
| `-ingest-token-file` / `-ingest-token` | none | Write-only token for push agents (also `DASHD_INGEST_TOKEN`) |
| `-admin-token-file` / `-admin-token` | none | Token for reprice/prune/forget/rename (also `DASHD_ADMIN_TOKEN`) |
| `-machine-tokens-file` | none | `machine token` lines; each token may only push as its machine |
| `-accept-ingest` | off (on for `serve`) | Accept pushes on a `serve-and-scan` server |
| `-tls-cert`, `-tls-key` | none | Serve HTTPS directly |
| `-tz` | host zone | Timezone used to bucket calls into days (also `DASHD_TZ`) |
| `-interval` | `30s` | How often to scan or push |
| `-home` | `$HOME` | Where to look for agent logs |
| `-machine` | hostname | Machine name attached to scanned sessions and calls |
| `-remote` | none | Server URL for `push`, `stats`, `insights`, `digest`, `export`, `query` and `mcp` (also `DASHD_REMOTE`) |
| `-redact-paths` | `none` | For `push`: `home`, `basename` or `hash` project paths before sending |
| `-models` | next to the binary, `~/.local/share/dashd`, `/usr/share/dashd` | OpenRouter price dump |
| `-models-refresh` | `0` | Auto-update `models.json` on this interval (e.g. `24h`) and reprice |
| `-backup-every`, `-backup-keep`, `-backup-dir` | off, `7`, beside the database | Rotating automatic backups |
| `-git` | off | Read commit hashes, times and subjects in the projects agents worked in, for cost per commit; `push` sends them |
| `-daily-budget`, `-monthly-budget` | none | Budget alerts in the UI and, with `-webhook-url`, by webhook |
| `-webhook-url`, `-digest-every` | none | Budget alerts and periodic digests to Slack/Discord-style webhooks |
| `-tags-file` | none | JSON `{ "tag": ["/project/prefix", ...] }` for cost-centre reporting |

## How it works

### Calls are stored, not summaries

The unit of storage is **one row per LLM request**. Every figure the dashboard shows is a rollup of those rows.

Storing individual calls keeps historical windows answerable even after old session logs are rotated away. When a session log disappears, its cost and token history are preserved and the session is marked *orphaned* rather than offering a broken transcript link.

```bash
dashd scan -interval 1h     # hourly, via cron
dashd serve                 # and serve from the same database
```

### Incremental scanning

Each scan pass stats every known file and skips unchanged ones. For modified files, the parser reads only what lies beyond the previous pass's cursor:

| Source | Cursor | Incremental by |
|---|---|---|
| pi, Claude, Codex, Gemini | byte offset | seeking to the end of the last complete line |
| Antigravity | row id | `WHERE idx > ?` on its `steps` table |
| OpenCode | max update time | `WHERE time_updated > ?` on messages and parts |

Three details keep incremental scans accurate:

* **Half-written final lines are skipped** until complete (the cursor stops at the last newline).
* **Rewrites are detected** via leading-byte fingerprints so a replaced log is re-parsed rather than treated as an append.
* **Idle passes make no writes** so unchanged sessions are never overwritten.

### Pricing

[`models.json`](models.json) (refreshed with `dashd update-models`, or automatically with `-models-refresh 24h`) is the source of truth for model rates, backed by an embedded fallback table ([`internal/source/manual_pricing.json`](internal/source/manual_pricing.json)) and provider overrides ([`internal/source/pricing_overrides.json`](internal/source/pricing_overrides.json)).

Models without a known rate are recorded as **unpriced** rather than `$0.00` so unknown costs are never confused with free usage. Prices are kept as a history: a change applies from the day it is seen, and `dashd reprice` recomputes each call at the price in force when it was made. `dashd reprice -current-prices` applies today's prices to everything, and the System tab sets prices by hand.

A central server prices every pushed call itself, with its own table, and buckets days in its own timezone (`-tz`), so totals do not depend on which laptop sent them.

## Layout

```
cmd/dashd/           entry point: flags, wiring, supervision
internal/model/      shared types across packages
internal/source/     per-agent parsers, pricing, protobuf reader
internal/store/      SQLite schema, idempotent writes, dashboard queries
internal/scan/       source discovery, incremental cursors, scan loop
internal/web/        HTTP handlers, templates, static assets
```

## Development

```bash
make check          # fmt-check, go vet, staticcheck, go test -race ./..., coverage >= 80%
```

Or run individual checks:

```bash
go test ./...
go test -race ./...
go vet ./...
gofmt -l .
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for architecture notes and how to add a new agent source.

## Container usage

Images are published as `ghcr.io/tonym128/dashd`. Inside the container, `dashd` binds `127.0.0.1` by default.

```bash
# One-off scan without publishing the web UI
docker run --rm \
  -v "$HOME/.pi:/home/dashd/.pi:ro" \
  -v dashd-data:/var/lib/dashd \
  ghcr.io/tonym128/dashd scan
```

To expose the dashboard, bind a non-loopback address and give it a token (dashd refuses to start otherwise):

```bash
docker run -d -p 127.0.0.1:8753:8753 \
  -e DASHD_AUTH_TOKEN="$(openssl rand -hex 32)" \
  -v "$HOME/.claude:/home/dashd/.claude:ro" \
  -v dashd-data:/var/lib/dashd \
  ghcr.io/tonym128/dashd \
  serve-and-scan -addr 0.0.0.0:8753 \
    -models /usr/local/lib/dashd/models.json -db /var/lib/dashd/dashboard.db
```

Note that the container UID must match your local user to read `0700` agent directories, and the named volume must be owned by that UID. The image ships a baked-in price dump at `/usr/local/lib/dashd/models.json`.

### Sidecar push container

If your coding agents run inside another container, you can attach a `dashd push` sidecar sharing the agent's log volume and pointing at your central server:

```yaml
  dashd:
    image: ghcr.io/tonym128/dashd:latest
    restart: unless-stopped
    environment:
      DASHD_REMOTE: "http://dashd-server:8753"
      DASHD_INGEST_TOKEN: "${DASHD_INGEST_TOKEN:?set in .env}"
    command:
      - push
      - -interval=60s
      - -machine=my-service
      - -home=/agent
      - -state-db=/var/lib/dashd/push-state.db
      - -models=/usr/local/lib/dashd/models.json
    volumes:
      - dashd-state:/var/lib/dashd
      - agent-logs:/agent/.claude:ro
```

## Remote server & multi-machine push

Run `dashd serve` as a central server and push incremental logs from your **Linux**, **macOS** and **Windows** machines. Each session and call records its machine (`-machine`, default the hostname); the dashboard filters by machine and shows each one's health on the System tab.

### 1. Start the server

With [docker-compose.yml](docker-compose.yml), which requires both tokens:

```bash
export DASHD_AUTH_TOKEN="$(openssl rand -hex 32)"     # people and the browser
export DASHD_INGEST_TOKEN="$(openssl rand -hex 32)"   # push agents, write-only
docker compose up -d
```

Or natively: `curl -fsSL https://raw.githubusercontent.com/tonym128/dashd/main/packaging/install.sh | bash -s -- --server`.

### 2. Install the push agent on each machine

Give each machine the **ingest** token, not the dashboard token.

- **Linux & macOS**:
  ```bash
  curl -fsSL https://raw.githubusercontent.com/tonym128/dashd/main/packaging/install.sh | bash
  # unattended:
  curl -fsSL .../install.sh | bash -s -- --remote https://dashd.example.com --token-file ./ingest.token --yes
  ```
- **Windows (PowerShell)**:
  ```powershell
  irm https://raw.githubusercontent.com/tonym128/dashd/main/packaging/install.ps1 | iex
  ```

The installers verify the download against the release's signed `checksums.txt`, store the token in a file only you can read, and install a service that restarts itself (systemd user unit, LaunchAgent, or a hidden scheduled task). `dashd status` shows the agent's last push and last error. Options include `--redact-paths`, `--auto-update 24h`, `--version` and `--uninstall`; see `install.sh --help`.

Packages: release pages carry `.deb`, `.rpm` and Arch packages; there is a Homebrew formula (`brew install tonym128/tap/dashd`) and a Scoop manifest when the tap and bucket are set up, and a source [PKGBUILD](packaging/aur/PKGBUILD).

## Upgrading & keeping up to date

### Central server (Docker)

Back up the database before major/schema-migrating upgrades, pull the latest image, and recreate the container (never run `docker compose down -v`, which deletes the database volume):

```bash
# Optional pre-upgrade backup (safe while serving)
docker exec dashd-server dashd backup -db /var/lib/dashd/dashboard.db -out /var/lib/dashd/pre-upgrade.bak

docker pull ghcr.io/tonym128/dashd:latest
docker compose up -d
```

To pin or roll back to a specific release, set `image: ghcr.io/tonym128/dashd:v0.11.0` in `docker-compose.yml`.

### Host push agents & CLI binaries

```bash
dashd self-update
# Then restart your service manager:
systemctl --user restart dashd-push.service   # Linux (systemd)
# or pass --auto-update 24h to install.sh so push agents update themselves
```

### Keeping pricing current & verifying health

Run the server with `-models-refresh 24h` (enabled in [docker-compose.yml](docker-compose.yml)) so `models.json` refreshes daily and pushes updates to clients automatically. For local-only installs, run `dashd update-models && dashd reprice`.

Verify everything after an upgrade:

```bash
dashd -version
dashd status    # shows push agent health and central server version
dashd doctor    # checks logs, pricing, database and remote reachability
```

## Read-only API, SQL queries & MCP

Give LLMs, scripts and external services a `-read-token` (`DASHD_READ_TOKEN` / `-read-token-file`): it grants read-only access to the dashboard, read APIs, `/api/query` and `/mcp`, and refuses `/api/ingest`, `/api/reprice` and `/api/admin/*`.

### Read-only HTTP JSON endpoints

All endpoints accept `Authorization: Bearer <token>` and the standard filter query parameters (`range`, `from`, `to`, `project`, `agent`, `model`, `machine`, `tag`):

| Endpoint | Returns |
|---|---|
| `GET /api/summary` | Full rollup payload (totals, burn windows, daily series, models, providers, projects, machines, tools, tags, facets, insights) |
| `GET /api/insights` | Ordered cost, efficiency, cache, budget and spike insights |
| `GET /api/digest?days=7` | Usage digest (`format=markdown` for raw Markdown, JSON by default) |
| `GET /api/stats` | Database totals, machines and per-source scan status |
| `GET /api/sessions` | Paginated sessions (`limit`, `offset`, `sort`) |
| `GET /api/export?kind=calls&format=json` | Streaming CSV/JSON export (`kind=calls\|sessions\|projects\|models\|machines\|daily\|tools\|tags`) |
| `GET /api/schema` | SQLite tables, views and columns |
| `POST /api/query` (`{"sql":"...","limit":500}`) | Single read-only SQL query (`SELECT`, `WITH`, `EXPLAIN`) enforced with `PRAGMA query_only = ON`, 10s timeout and row cap (also `GET /api/query?sql=...`) |

### Remote CLI & `query`

`stats`, `insights`, `digest`, `export` and `query` work against a remote server with `-remote` + `-read-token-file` (or `-auth-token-file`) without needing a local database:

```bash
dashd stats    -remote https://dashd.example.com -read-token-file ~/.config/dashd/read.token
dashd insights -remote https://dashd.example.com -read-token-file ~/.config/dashd/read.token
dashd query    -remote https://dashd.example.com -read-token-file ~/.config/dashd/read.token -schema
dashd query    -remote https://dashd.example.com -read-token-file ~/.config/dashd/read.token \
  -sql "SELECT model, COUNT(*) AS calls, ROUND(SUM(cost_usd),2) AS usd FROM call GROUP BY model ORDER BY usd DESC LIMIT 10"
```

### Model Context Protocol (MCP)

`dashd` exposes six read-only MCP tools (`get_summary`, `list_sessions`, `get_insights`, `get_digest`, `get_schema`, `query_sql`) over both **stdio** and **Streamable HTTP** (`POST /mcp`):

```json
{
  "mcpServers": {
    "dashd": {
      "command": "dashd",
      "args": [
        "mcp",
        "-remote", "https://dashd.example.com",
        "-read-token-file", "/home/you/.config/dashd/read.token"
      ]
    }
  }
}
```

Omit `-remote` to run `dashd mcp` directly against the local SQLite database. For clients that support remote **Streamable HTTP MCP**, point directly at `POST https://dashd.example.com/mcp` with `Authorization: Bearer <read-token>` (no local binary required):

```json
{
  "mcpServers": {
    "dashd": {
      "url": "https://dashd.example.com/mcp",
      "headers": {
        "Authorization": "Bearer YOUR_READ_TOKEN"
      }
    }
  }
}
```

## Proxy for Ollama, `llama-server` & direct APIs

Use `dashd proxy` to capture token usage and timing for local inference engines (`ollama`, `llama-server`) and direct OpenAI/Anthropic API clients that do not write session logs to disk:

```bash
# Proxy local Ollama (:11434) on :8754, mark calls $0 (-free), and push to your central server:
dashd proxy \
  -upstream http://127.0.0.1:11434 \
  -listen 127.0.0.1:8754 \
  -free \
  -remote https://dashd.example.com \
  -ingest-token-file ~/.config/dashd/ingest.token
```

- **Streaming & protocols:** transparently passes through SSE (`text/event-stream`), NDJSON (`application/x-ndjson`), and JSON for OpenAI (`/v1/chat/completions`, `/v1/completions`, `/v1/responses`), Ollama (`/api/chat`, `/api/generate`), `llama-server` (`timings`), and Anthropic (`/v1/messages`).
- **No double-counting:** requests carrying `X-Dashd-Skip: 1` or a scanned coding-harness `User-Agent` (`-skip-agents`, default: `opencode,claude-code,claude-cli,codex,gemini-cli,antigravity,pi-agent`) are forwarded without recording.
- **Per-request attribution:** optional headers `X-Dashd-Project`, `X-Dashd-Session`, and `X-Dashd-Agent` tag calls and are stripped before forwarding upstream.

## Security

By default, `dashd` binds to `127.0.0.1` with no authentication, refuses cross-site writes and DNS-rebinding requests, and does not accept pushes. A non-loopback address needs a token: browsers sign in at `/login`, API clients send `Authorization: Bearer <token>`. See [SECURITY.md](SECURITY.md) for the roles, token files and threat model.
