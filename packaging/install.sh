#!/usr/bin/env bash
# Installer for dashd on Linux and macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/tonym128/dashd/main/packaging/install.sh | bash
#
# By default it installs the push agent: a user-level service that scans this
# machine's agent logs and pushes them to a central dashd server. With
# --server it installs a dashboard server instead.
#
# Every prompt reads from the terminal (/dev/tty), so the curl | bash form
# works; every prompt also has a flag, so it can run unattended:
#
#   install.sh --remote https://dashd.example.com --token-file ~/ingest.token --yes
#
# Options:
#   --remote URL          server URL for the push agent
#   --token-file PATH     file holding the server's write-only ingest token
#   --token-env VAR       read the token from environment variable VAR
#   --machine NAME        machine name (default: short hostname)
#   --interval DUR        push interval (default 30s)
#   --models-refresh DUR  models.json auto-update interval, 0 disables (default 24h)
#   --redact-paths MODE   none|home|basename|hash (default none)
#   --auto-update DUR     check for new releases on this interval, 0 disables (default 0)
#   --server              install a dashboard server (serve-and-scan) instead
#   --addr ADDR           server listen address (default 127.0.0.1:8753)
#   --version vX.Y.Z      install this release instead of the latest
#   --local PATH          install a local binary instead of downloading
#   --upgrade             replace the binary and restart, keeping configuration
#   --uninstall           remove the service and binary, keep config and data
#   --yes                 accept defaults for anything not given as a flag
set -euo pipefail

REPO="tonym128/dashd"
LOCAL_BIN=""
UPGRADE=0
UNINSTALL=0
SERVER=0
ASSUME_YES=0
VERSION_TAG=""
REMOTE_URL=""
TOKEN=""
TOKEN_FILE_IN=""
MACHINE_NAME=""
PUSH_INTERVAL=""
MODELS_REFRESH=""
REDACT_PATHS=""
AUTO_UPDATE=""
ADDR=""

die() { echo "error: $*" >&2; exit 1; }
need_arg() { [[ $# -ge 2 && -n "$2" ]] || die "$1 needs a value"; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --local) need_arg "$@"; LOCAL_BIN="$2"; shift 2 ;;
    --upgrade) UPGRADE=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    --server) SERVER=1; shift ;;
    --yes|-y) ASSUME_YES=1; shift ;;
    --version) need_arg "$@"; VERSION_TAG="$2"; shift 2 ;;
    --remote) need_arg "$@"; REMOTE_URL="$2"; shift 2 ;;
    --token-file) need_arg "$@"; TOKEN_FILE_IN="$2"; shift 2 ;;
    --token-env) need_arg "$@"; TOKEN="${!2:-}"; [[ -n "$TOKEN" ]] || die "environment variable $2 is empty"; shift 2 ;;
    --machine) need_arg "$@"; MACHINE_NAME="$2"; shift 2 ;;
    --interval) need_arg "$@"; PUSH_INTERVAL="$2"; shift 2 ;;
    --models-refresh) need_arg "$@"; MODELS_REFRESH="$2"; shift 2 ;;
    --redact-paths) need_arg "$@"; REDACT_PATHS="$2"; shift 2 ;;
    --auto-update) need_arg "$@"; AUTO_UPDATE="$2"; shift 2 ;;
    --addr) need_arg "$@"; ADDR="$2"; shift 2 ;;
    -h|--help) sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown option: $1 (see --help)" ;;
  esac
done

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$OS" in
  linux|darwin) ;;
  *) die "unsupported OS: $OS (use install.ps1 on Windows)" ;;
esac
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) die "unsupported architecture: $ARCH" ;;
esac

INSTALL_DIR="${HOME}/.local/bin"
DATA_DIR="${HOME}/.local/share/dashd"
CONFIG_DIR="${HOME}/.config/dashd"
LOG_DIR="${HOME}/Library/Logs/dashd"
TOKEN_PATH="$CONFIG_DIR/ingest.token"
SERVICE="dashd-push"
LABEL="com.dashd.push"
if [[ "$SERVER" -eq 1 ]]; then
  SERVICE="dashd"
  LABEL="com.dashd.server"
  TOKEN_PATH="$CONFIG_DIR/auth.token"
fi
UNIT_FILE="${HOME}/.config/systemd/user/${SERVICE}.service"
PLIST_FILE="${HOME}/Library/LaunchAgents/${LABEL}.plist"

# Prompts read from the terminal, not stdin: under curl | bash, stdin is the
# script itself, and `read` would swallow its next lines.
TTY=""
if [[ -r /dev/tty ]] && { : < /dev/tty; } 2>/dev/null; then TTY=/dev/tty; fi

ask() { # ask VAR "prompt" default [secret]
  local var="$1" prompt="$2" def="${3:-}" secret="${4:-}" reply=""
  if [[ -n "${!var}" ]]; then return; fi
  if [[ "$ASSUME_YES" -eq 1 || -z "$TTY" ]]; then
    printf -v "$var" '%s' "$def"
    return
  fi
  if [[ -n "$secret" ]]; then
    read -rsp "$prompt" reply < "$TTY"; echo > "$TTY"
  else
    read -rp "$prompt${def:+ [$def]}: " reply < "$TTY"
  fi
  printf -v "$var" '%s' "${reply:-$def}"
}

stop_service() {
  if [[ "$OS" == "linux" ]]; then
    systemctl --user disable --now "$SERVICE" 2>/dev/null || true
  else
    launchctl bootout "gui/$(id -u)" "$PLIST_FILE" 2>/dev/null || launchctl unload "$PLIST_FILE" 2>/dev/null || true
  fi
}

start_service() {
  if [[ "$OS" == "linux" ]]; then
    systemctl --user daemon-reload
    systemctl --user enable --now "$SERVICE"
    systemctl --user restart "$SERVICE"
  else
    launchctl bootout "gui/$(id -u)" "$PLIST_FILE" 2>/dev/null || launchctl unload "$PLIST_FILE" 2>/dev/null || true
    launchctl bootstrap "gui/$(id -u)" "$PLIST_FILE" 2>/dev/null || launchctl load -w "$PLIST_FILE"
  fi
}

if [[ "$UNINSTALL" -eq 1 ]]; then
  stop_service
  rm -f "$UNIT_FILE" "$PLIST_FILE"
  [[ "$OS" == "linux" ]] && systemctl --user daemon-reload 2>/dev/null || true
  rm -f "$INSTALL_DIR/dashd"
  echo "Uninstalled the $SERVICE service and $INSTALL_DIR/dashd."
  echo "Kept: configuration in $CONFIG_DIR and data in $DATA_DIR."
  exit 0
fi

mkdir -p "$INSTALL_DIR" "$DATA_DIR" "$CONFIG_DIR"
chmod 700 "$CONFIG_DIR" "$DATA_DIR"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

if [[ -n "$LOCAL_BIN" ]]; then
  [[ -f "$LOCAL_BIN" ]] || die "$LOCAL_BIN does not exist"
  install -m 0755 "$LOCAL_BIN" "$INSTALL_DIR/dashd"
  SIBLING_MODELS="$(dirname "$LOCAL_BIN")/models.json"
  if [[ -f "$SIBLING_MODELS" ]]; then
    install -m 0644 "$SIBLING_MODELS" "$DATA_DIR/models.json"
  fi
  echo "Installed local binary to $INSTALL_DIR/dashd"
else
  if [[ -z "$VERSION_TAG" ]]; then
    echo "Fetching latest release metadata from GitHub ($REPO)..."
    VERSION_TAG="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)"
    [[ -n "$VERSION_TAG" ]] || die "could not determine the latest release tag"
  fi
  VER="${VERSION_TAG#v}"
  ARCHIVE="dashd_${VER}_${OS}_${ARCH}.tar.gz"
  BASE="https://github.com/$REPO/releases/download/${VERSION_TAG}"
  TMP_DIR="$(mktemp -d)"
  trap 'rm -rf "$TMP_DIR"' EXIT

  echo "Downloading $BASE/$ARCHIVE..."
  curl -fsSL "$BASE/$ARCHIVE" -o "$TMP_DIR/$ARCHIVE"
  curl -fsSL "$BASE/checksums.txt" -o "$TMP_DIR/checksums.txt"
  WANT="$(awk -v f="$ARCHIVE" '$2 == f || $2 == "*"f {print $1}' "$TMP_DIR/checksums.txt")"
  [[ -n "$WANT" ]] || die "$ARCHIVE is not listed in checksums.txt; not installing"
  GOT="$(sha256_of "$TMP_DIR/$ARCHIVE")"
  [[ "$WANT" == "$GOT" ]] || die "checksum mismatch for $ARCHIVE (got $GOT, release says $WANT); not installing"
  echo "Checksum verified."

  tar -xzf "$TMP_DIR/$ARCHIVE" -C "$TMP_DIR"
  EXTRACTED_DIR="$TMP_DIR/dashd_${VER}_${OS}_${ARCH}"
  install -m 0755 "$EXTRACTED_DIR/dashd" "$INSTALL_DIR/dashd"
  if [[ -f "$EXTRACTED_DIR/models.json" ]]; then
    install -m 0644 "$EXTRACTED_DIR/models.json" "$DATA_DIR/models.json"
  fi
  echo "Installed dashd $VERSION_TAG to $INSTALL_DIR/dashd"
fi

if [[ "$UPGRADE" -eq 1 ]]; then
  if [[ -f "$UNIT_FILE" || -f "$PLIST_FILE" ]]; then
    start_service
    echo "Upgraded the binary and restarted $SERVICE (configuration kept)."
  else
    echo "Upgraded the binary. No $SERVICE service is installed; run without --upgrade to configure one."
  fi
  exit 0
fi

# ---------------------------------------------------------------- configuration

if [[ -n "$TOKEN_FILE_IN" ]]; then
  [[ -r "$TOKEN_FILE_IN" ]] || die "cannot read $TOKEN_FILE_IN"
  TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE_IN")"
fi

echo ""
if [[ "$SERVER" -eq 1 ]]; then
  echo "=== dashd server configuration ==="
  ask ADDR "Listen address (127.0.0.1 keeps it local; 0.0.0.0 needs a token)" "127.0.0.1:8753"
  if [[ "$ADDR" != 127.0.0.1:* && "$ADDR" != localhost:* && -z "$TOKEN" ]]; then
    TOKEN="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
    echo "Generated a dashboard token (saved to $TOKEN_PATH)."
  fi
else
  echo "=== dashd push agent configuration ==="
  ask REMOTE_URL "Remote dashd server URL (e.g. https://dashd.example.com)" ""
  [[ -n "$REMOTE_URL" ]] || die "a server URL is required (--remote)"
  case "$REMOTE_URL" in
    https://*) ;;
    http://127.0.0.1*|http://localhost*) ;;
    http://*) echo "warning: $REMOTE_URL is plain HTTP; the token and every session travel unencrypted." >&2 ;;
    *) die "the server URL must start with http:// or https://" ;;
  esac
  ask TOKEN "Ingest token (the server's -ingest-token; leave empty if it has none): " "" secret
  ask MACHINE_NAME "Machine name" "$(hostname -s 2>/dev/null || hostname)"
  ask PUSH_INTERVAL "Push interval" "30s"
  ask REDACT_PATHS "Send project paths as (none, home, basename, hash)" "none"
  ask AUTO_UPDATE "Auto-update dashd itself (e.g. 24h, 0 to disable)" "0"
fi
ask MODELS_REFRESH "Auto-update models.json interval (e.g. 24h, 0 to disable)" "24h"

if [[ "$MODELS_REFRESH" != "0" || ! -f "$DATA_DIR/models.json" ]]; then
  echo "Updating models.json..."
  "$INSTALL_DIR/dashd" update-models -models "$DATA_DIR/models.json" || echo "warning: initial models update failed; using the bundled fallback." >&2
fi

# The token lives in a 0600 file read with -*-token-file: not in argv (which
# every local user can read with ps) and not in a plist or unit file.
TOKEN_ARGS=()
if [[ -n "$TOKEN" ]]; then
  (umask 077; printf '%s\n' "$TOKEN" > "$TOKEN_PATH")
  chmod 600 "$TOKEN_PATH"
  if [[ "$SERVER" -eq 1 ]]; then
    TOKEN_ARGS=(-auth-token-file "$TOKEN_PATH")
  else
    TOKEN_ARGS=(-ingest-token-file "$TOKEN_PATH")
  fi
fi

if [[ "$SERVER" -eq 1 ]]; then
  ARGS=(serve-and-scan -addr "$ADDR" -models "$DATA_DIR/models.json" -models-refresh "$MODELS_REFRESH"
        -db "$DATA_DIR/dashboard.db" "${TOKEN_ARGS[@]}")
  DESC="dashd dashboard server"
else
  ARGS=(push -remote "$REMOTE_URL" -machine "$MACHINE_NAME" -interval "$PUSH_INTERVAL"
        -models "$DATA_DIR/models.json" -models-refresh "$MODELS_REFRESH"
        -state-db "$DATA_DIR/push-state.db" -redact-paths "$REDACT_PATHS" -auto-update "$AUTO_UPDATE"
        "${TOKEN_ARGS[@]}")
  DESC="dashd push agent"
fi

# systemd quoting: each argument double-quoted, with \ and " escaped.
systemd_quote() {
  local out="" a
  for a in "$@"; do
    a="${a//\\/\\\\}"; a="${a//\"/\\\"}"; a="${a//%/%%}"
    out+=" \"$a\""
  done
  printf '%s' "$out"
}
xml_escape() {
  local s="$1"
  s="${s//&/&amp;}"; s="${s//</&lt;}"; s="${s//>/&gt;}"; s="${s//\"/&quot;}"
  printf '%s' "$s"
}

if [[ "$OS" == "linux" ]]; then
  mkdir -p "$(dirname "$UNIT_FILE")"
  cat > "$UNIT_FILE" <<EOF
[Unit]
Description=$DESC
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$(systemd_quote "$INSTALL_DIR/dashd")$(systemd_quote "${ARGS[@]}")
# Always: dashd exits with status 3 after installing an update so that the
# supervisor restarts it into the new binary.
Restart=always
RestartSec=10
NoNewPrivileges=true

[Install]
WantedBy=default.target
EOF
  start_service
  echo ""
  echo "$SERVICE installed and started."
  echo "Logs:   journalctl --user -u $SERVICE -f"
  [[ "$SERVER" -eq 0 ]] && echo "Status: $INSTALL_DIR/dashd status"
  echo "Tip: 'loginctl enable-linger $USER' keeps it running while you are logged out."
else
  mkdir -p "$(dirname "$PLIST_FILE")" "$LOG_DIR"
  {
    cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$(xml_escape "$INSTALL_DIR/dashd")</string>
EOF
    for a in "${ARGS[@]}"; do
      printf '    <string>%s</string>\n' "$(xml_escape "$a")"
    done
    cat <<EOF
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>$(xml_escape "$LOG_DIR/$SERVICE.log")</string>
  <key>StandardErrorPath</key>
  <string>$(xml_escape "$LOG_DIR/$SERVICE.log")</string>
</dict>
</plist>
EOF
  } > "$PLIST_FILE"
  chmod 600 "$PLIST_FILE"
  start_service
  echo ""
  echo "$LABEL LaunchAgent installed and started."
  echo "Logs:   tail -f $LOG_DIR/$SERVICE.log"
  [[ "$SERVER" -eq 0 ]] && echo "Status: $INSTALL_DIR/dashd status"
fi

if [[ "$SERVER" -eq 1 ]]; then
  echo "Dashboard: http://$ADDR/"
  [[ -n "$TOKEN" ]] && echo "Sign in with the token in $TOKEN_PATH."
fi
