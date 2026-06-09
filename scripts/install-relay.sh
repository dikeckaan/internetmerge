#!/usr/bin/env bash
# Deprecated: relay setup is now handled by setup-relay.sh (one binary, Docker or
# native, Linux/macOS/Windows). This shim forwards to it.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
echo "install-relay.sh is deprecated; using setup-relay.sh instead." >&2
exec "$DIR/setup-relay.sh" "$@"
