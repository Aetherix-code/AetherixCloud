#!/usr/bin/env bash
# Build the AetherixCloud Caddy: stock Caddy + the replicated-store storage
# module + the Cloudflare DNS-01 provider (wildcard certificates).
#
#   ./build.sh [output]            # needs Go and network access
#   CADDY_VERSION=v2.9.1 ./build.sh /usr/local/bin/caddy
#
# The Docker node image does the same in a build stage (deploy/dev/Dockerfile.node).
set -euo pipefail
cd "$(dirname "$0")"
OUT="${1:-./caddy}"
CADDY_VERSION="${CADDY_VERSION:-v2.9.1}"
command -v xcaddy >/dev/null || go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest
xcaddy build "$CADDY_VERSION" \
  --with github.com/aetherix/aetherixcloud/deploy/caddy="$(pwd)" \
  --with github.com/caddy-dns/cloudflare \
  --output "$OUT"
"$OUT" list-modules | grep -q caddy.storage.aetherix
echo "built $OUT"
