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
| `serve` | Serve only; the database is maintained elsewhere |
| `scan` | One pass, then exit — for cron or a systemd timer |
| `reprice` | Recompute every stored cost from the current price table |
| `stats` | Print a summary of the database and the last scan of each source |

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-db` | `~/.local/share/dashd/dashboard.db` | Database path |
| `-addr` | `127.0.0.1:8753` | Listen address; `0.0.0.0` exposes it to the network |
| `-auth-token` | none | Require `Authorization: Bearer <token>`; use it if you expose the dashboard |
| `-interval` | `30s` | How often to scan |
| `-home` | `$HOME` | Where to look for agent logs |
| `-models` | next to the binary, one level above it, the working directory, or one level above that; else `./models.json` | OpenRouter price dump |
| `-opencode` | inside `-home` | OpenCode database path |
| `-verbose` | off | Log every request and scan detail |

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

[`models.json`](models.json) (refreshed with `python3 update_models.py`) is the source of truth for model rates, backed by an embedded fallback table ([`internal/source/manual_pricing.json`](internal/source/manual_pricing.json)) and provider overrides ([`internal/source/pricing_overrides.json`](internal/source/pricing_overrides.json)).

Models without a known rate are recorded as **unpriced** rather than `$0.00` so unknown costs are never confused with free usage. Running `dashd reprice` recomputes all stored calls against the current price table.

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

To expose the dashboard, pass both `-addr 0.0.0.0:8753` and `-auth-token`:

```bash
docker run -d -p 127.0.0.1:8753:8753 \
  -v "$HOME/.claude:/home/dashd/.claude:ro" \
  -v dashd-data:/var/lib/dashd \
  ghcr.io/tonym128/dashd \
  serve-and-scan \
    -addr 0.0.0.0:8753 \
    -auth-token "$(openssl rand -hex 32)" \
    -models /usr/local/lib/dashd/models.json \
    -db /var/lib/dashd/dashboard.db
```

Note that the container UID must match your local user to read `0700` agent directories, and the named volume must be owned by that UID.

## Security

By default, `dashd` binds to `127.0.0.1` with no authentication. If you expose it on a non-loopback address (`-addr 0.0.0.0`), pass `-auth-token` and send `Authorization: Bearer <token>`. See [SECURITY.md](SECURITY.md) for the threat model and safer ways to supply the token.
