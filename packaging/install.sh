#!/usr/bin/env bash
set -euo pipefail

# Interactive installer for the dashd push agent on Linux and macOS.
# Downloads the latest release from GitHub (or uses --local <path-to-binary>),
# prompts for configuration, and installs a user-level background service.

REPO="tonym128/dashd"
LOCAL_BIN=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --local)
      LOCAL_BIN="${2:-}"
      shift 2
      ;;
    *)
      echo "Unknown option: $1" >&2
      exit 1
      ;;
  esac
done

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$OS" in
  linux|darwin) ;;
  *) echo "Unsupported OS: $OS (use install.ps1 on Windows)" >&2; exit 1 ;;
esac
case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;;
esac

INSTALL_DIR="${HOME}/.local/bin"
DATA_DIR="${HOME}/.local/share/dashd"
CONFIG_DIR="${HOME}/.config/dashd"
mkdir -p "$INSTALL_DIR" "$DATA_DIR" "$CONFIG_DIR"

if [[ -n "$LOCAL_BIN" ]]; then
  cp "$LOCAL_BIN" "$INSTALL_DIR/dashd"
  chmod 0755 "$INSTALL_DIR/dashd"
  echo "Installed local binary to $INSTALL_DIR/dashd"
else
  echo "Fetching latest release metadata from GitHub ($REPO)..."
  TAG="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1)"
  if [[ -z "$TAG" ]]; then
    echo "Could not determine latest release tag." >&2
    exit 1
  fi
  VER="${TAG#v}"
  ARCHIVE="dashd_${VER}_${OS}_${ARCH}.tar.gz"
  URL="https://github.com/$REPO/releases/download/${TAG}/${ARCHIVE}"
  TMP_DIR="$(mktemp -d)"
  trap 'rm -rf "$TMP_DIR"' EXIT

  echo "Downloading $URL..."
  curl -fsSL "$URL" -o "$TMP_DIR/$ARCHIVE"
  tar -xzf "$TMP_DIR/$ARCHIVE" -C "$TMP_DIR"
  EXTRACTED_DIR="$TMP_DIR/dashd_${VER}_${OS}_${ARCH}"
  install -m 0755 "$EXTRACTED_DIR/dashd" "$INSTALL_DIR/dashd"
  if [[ -f "$EXTRACTED_DIR/models.json" ]]; then
    install -m 0644 "$EXTRACTED_DIR/models.json" "$DATA_DIR/models.json"
  fi
  echo "Installed dashd $TAG to $INSTALL_DIR/dashd"
fi

echo ""
echo "=== dashd Agent Configuration ==="
read -rp "Remote dashd server URL (e.g. https://dashd.example.com): " REMOTE_URL
while [[ -z "$REMOTE_URL" ]]; do
  read -rp "Remote URL cannot be empty: " REMOTE_URL
done

read -rsp "Auth token (leave empty if server has no -auth-token): " AUTH_TOKEN
echo ""

DEFAULT_MACHINE="$(hostname -s 2>/dev/null || hostname)"
read -rp "Machine name [$DEFAULT_MACHINE]: " MACHINE_NAME
MACHINE_NAME="${MACHINE_NAME:-$DEFAULT_MACHINE}"

read -rp "Push interval [30s]: " PUSH_INTERVAL
PUSH_INTERVAL="${PUSH_INTERVAL:-30s}"

read -rp "Auto-update models.json interval (e.g. 24h, or 0 to disable) [24h]: " MODELS_REFRESH
MODELS_REFRESH="${MODELS_REFRESH:-24h}"

# Fetch latest models.json now if auto-update is enabled or file is missing.
if [[ "$MODELS_REFRESH" != "0" || ! -f "$DATA_DIR/models.json" ]]; then
  echo "Updating models.json..."
  "$INSTALL_DIR/dashd" update-models -models "$DATA_DIR/models.json" || echo "Warning: initial models update failed; using bundled fallback."
fi

ENV_FILE="$CONFIG_DIR/push.env"
umask 077
cat > "$ENV_FILE" <<EOF
DASHD_REMOTE=$REMOTE_URL
DASHD_AUTH_TOKEN=$AUTH_TOKEN
EOF
chmod 0600 "$ENV_FILE"

if [[ "$OS" == "linux" ]]; then
  UNIT_DIR="${HOME}/.config/systemd/user"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_DIR/dashd-push.service" <<EOF
[Unit]
Description=dashd remote log pusher
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=$ENV_FILE
ExecStart=$INSTALL_DIR/dashd push -machine "$MACHINE_NAME" -interval $PUSH_INTERVAL -models "$DATA_DIR/models.json" -models-refresh $MODELS_REFRESH -db "$DATA_DIR/push-state.db"
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now dashd-push
  echo ""
  echo "dashd-push service installed and started!"
  echo "View logs: journalctl --user -u dashd-push -f"
elif [[ "$OS" == "darwin" ]]; then
  PLIST_DIR="${HOME}/Library/LaunchAgents"
  PLIST_FILE="$PLIST_DIR/com.dashd.push.plist"
  mkdir -p "$PLIST_DIR"
  cat > "$PLIST_FILE" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.dashd.push</string>
  <key>ProgramArguments</key>
  <array>
    <string>$INSTALL_DIR/dashd</string>
    <string>push</string>
    <string>-machine</string>
    <string>$MACHINE_NAME</string>
    <string>-interval</string>
    <string>$PUSH_INTERVAL</string>
    <string>-models</string>
    <string>$DATA_DIR/models.json</string>
    <string>-models-refresh</string>
    <string>$MODELS_REFRESH</string>
    <string>-db</string>
    <string>$DATA_DIR/push-state.db</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>DASHD_REMOTE</key>
    <string>$REMOTE_URL</string>
    <key>DASHD_AUTH_TOKEN</key>
    <string>$AUTH_TOKEN</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/tmp/dashd-push.log</string>
  <key>StandardErrorPath</key>
  <string>/tmp/dashd-push.err</string>
</dict>
</plist>
EOF
  chmod 0600 "$PLIST_FILE"
  launchctl unload "$PLIST_FILE" 2>/dev/null || true
  launchctl load -w "$PLIST_FILE"
  echo ""
  echo "com.dashd.push LaunchAgent installed and started!"
  echo "View logs: tail -f /tmp/dashd-push.log /tmp/dashd-push.err"
fi
