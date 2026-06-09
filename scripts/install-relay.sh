#!/usr/bin/env bash
# Installs the InternetMerge bonding relay on a Linux VPS (systemd). Run as root.
# Usage: curl -fsSL <raw-url>/install-relay.sh | sudo bash -s -- <version>
set -euo pipefail

VERSION="${1:-latest}"
REPO="dikeckaan/internetmerge"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ASSET_ARCH="amd64" ;;
  aarch64|arm64) ASSET_ARCH="arm64" ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

if [ "$VERSION" = "latest" ]; then
  URL="https://github.com/${REPO}/releases/latest/download/internetmerge-relay-linux-${ASSET_ARCH}"
else
  URL="https://github.com/${REPO}/releases/download/${VERSION}/internetmerge-relay-linux-${ASSET_ARCH}"
fi

echo "Downloading relay ($ASSET_ARCH) from $URL"
curl -fsSL "$URL" -o /usr/local/bin/internetmerge-relay
chmod +x /usr/local/bin/internetmerge-relay

if [ ! -f /etc/internetmerge-relay.env ]; then
  KEY="$(head -c 32 /dev/urandom | base64)"
  echo "INTERNETMERGE_RELAY_KEY=${KEY}" > /etc/internetmerge-relay.env
  chmod 600 /etc/internetmerge-relay.env
else
  KEY="$(grep -oP '(?<=INTERNETMERGE_RELAY_KEY=).*' /etc/internetmerge-relay.env)"
fi

curl -fsSL "https://raw.githubusercontent.com/${REPO}/main/build/relay/internetmerge-relay.service" \
  -o /etc/systemd/system/internetmerge-relay.service
systemctl daemon-reload
systemctl enable --now internetmerge-relay

PUBIP="$(curl -fsSL https://api.ipify.org || echo YOUR_SERVER_IP)"
echo
echo "Relay running. Paste this connection string into InternetMerge:"
echo "  Address: ${PUBIP}:7000"
echo "  Key:     ${KEY}"
echo
echo "Make sure TCP port 7000 is open in your firewall/security group."
