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
  if [ ! -f .env ]; then echo "INTERNETMERGE_RELAY_KEY=${key}" > .env; fi
  docker compose up -d
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
  curl -fsSL "https://raw.githubusercontent.com/${REPO}/main/build/relay/internetmerge-relay.service" \
    | sudo tee /etc/systemd/system/internetmerge-relay.service >/dev/null
  sudo systemctl daemon-reload
  sudo systemctl enable --now internetmerge-relay
  print_conn "$key"
}

setup_macos_native() {
  install_binary
  local key="${INTERNETMERGE_RELAY_KEY:-$(gen_key)}"
  local plist="$HOME/Library/LaunchAgents/com.internetmerge.relay.plist"
  mkdir -p "$HOME/Library/LaunchAgents"
  curl -fsSL "https://raw.githubusercontent.com/${REPO}/main/build/relay/com.internetmerge.relay.plist" \
    | sed "s|__RELAY_KEY__|${key}|" > "$plist"
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
