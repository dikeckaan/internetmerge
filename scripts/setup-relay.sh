#!/usr/bin/env bash
# One-command InternetMerge relay setup for Linux and macOS.
# Prefers Docker when available; otherwise installs the native binary as a
# systemd service (Linux) or launchd agent (macOS).
#
#   curl -fsSL <raw-url>/scripts/setup-relay.sh | bash
#   ./scripts/setup-relay.sh --native   # force native install (skip Docker)
#   ./scripts/setup-relay.sh --docker   # force Docker
set -euo pipefail

REPO="dikeckaan/internetmerge"
PORT="7000"
MODE="auto"
for a in "$@"; do
  case "$a" in
    --docker) MODE="docker" ;;
    --native) MODE="native" ;;
    *) echo "unknown arg: $a" >&2; exit 2 ;;
  esac
done

OS="$(uname -s)"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) ASSET_ARCH="amd64" ;;
  aarch64|arm64) ASSET_ARCH="arm64" ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

have() { command -v "$1" >/dev/null 2>&1; }

gen_key() {
  if have openssl; then openssl rand -base64 32
  else head -c 32 /dev/urandom | base64; fi
}

print_conn() {
  local key="$1"
  local ip
  ip="$(curl -fsSL https://api.ipify.org 2>/dev/null || echo YOUR_SERVER_IP)"
  echo
  echo "Relay is up. Paste this into InternetMerge:"
  echo "  Address: ${ip}:${PORT}"
  echo "  Key:     ${key}"
  echo
  echo "Open TCP port ${PORT} in your firewall / security group."
}

setup_docker() {
  echo "Using Docker."
  local key="${INTERNETMERGE_RELAY_KEY:-$(gen_key)}"
  docker rm -f internetmerge-relay >/dev/null 2>&1 || true
  docker run -d --name internetmerge-relay --restart unless-stopped \
    -p "${PORT}:7000" -e "INTERNETMERGE_RELAY_KEY=${key}" \
    ghcr.io/dikeckaan/internetmerge:latest
  print_conn "$key"
}

os_asset() {
  case "$OS" in
    Linux) echo "internetmerge-linux-${ASSET_ARCH}" ;;
    Darwin) echo "internetmerge-darwin-${ASSET_ARCH}" ;;
    *) echo "unsupported OS for native install: $OS" >&2; exit 1 ;;
  esac
}

install_binary() {
  local asset url dest="/usr/local/bin/internetmerge"
  asset="$(os_asset)"
  url="https://github.com/${REPO}/releases/latest/download/${asset}"
  echo "Downloading ${asset}"
  if [ -w "$(dirname "$dest")" ] || [ "$(id -u)" = "0" ]; then
    curl -fsSL "$url" -o "$dest" && chmod +x "$dest"
  else
    sudo curl -fsSL "$url" -o "$dest" && sudo chmod +x "$dest"
  fi
}

setup_linux_native() {
  install_binary
  local key="${INTERNETMERGE_RELAY_KEY:-$(gen_key)}"
  echo "INTERNETMERGE_RELAY_KEY=${key}" | sudo tee /etc/internetmerge-relay.env >/dev/null
  sudo chmod 600 /etc/internetmerge-relay.env
  sudo tee /etc/systemd/system/internetmerge-relay.service >/dev/null <<EOF
[Unit]
Description=InternetMerge bonding relay
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/internetmerge relay --listen :${PORT}
EnvironmentFile=/etc/internetmerge-relay.env
Restart=on-failure
RestartSec=2
DynamicUser=yes
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
EOF
  sudo systemctl daemon-reload
  sudo systemctl enable --now internetmerge-relay
  print_conn "$key"
}

setup_macos_native() {
  install_binary
  local key="${INTERNETMERGE_RELAY_KEY:-$(gen_key)}"
  local plist="$HOME/Library/LaunchAgents/com.internetmerge.relay.plist"
  mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
  # The plist embeds the secret key — create it private (not world-readable).
  umask 077
  cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.internetmerge.relay</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/internetmerge</string>
    <string>relay</string>
    <string>--listen</string>
    <string>:${PORT}</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>INTERNETMERGE_RELAY_KEY</key><string>${key}</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>${HOME}/Library/Logs/internetmerge-relay.log</string>
  <key>StandardErrorPath</key><string>${HOME}/Library/Logs/internetmerge-relay.log</string>
</dict>
</plist>
EOF
  chmod 600 "$plist"
  launchctl unload "$plist" 2>/dev/null || true
  launchctl load "$plist"
  print_conn "$key"
}

case "$MODE" in
  docker) setup_docker ;;
  native)
    case "$OS" in
      Linux) setup_linux_native ;;
      Darwin) setup_macos_native ;;
      *) echo "unsupported OS: $OS" >&2; exit 1 ;;
    esac ;;
  auto)
    if have docker; then setup_docker
    else
      case "$OS" in
        Linux) setup_linux_native ;;
        Darwin) setup_macos_native ;;
        *) echo "no Docker and unsupported OS: $OS" >&2; exit 1 ;;
      esac
    fi ;;
esac
