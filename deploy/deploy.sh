#!/bin/sh
# Build and start the zrelay and zview-server containers (design §8.3).
# TAILSCALE_IP is taken from `tailscale ip -4` when it is not set.
set -eu
cd "$(dirname "$0")/.."

if [ -z "${TAILSCALE_IP:-}" ]; then
	TAILSCALE_IP=$(tailscale ip -4 | head -n 1)
fi
if [ -z "$TAILSCALE_IP" ]; then
	echo "deploy.sh: no Tailscale IPv4 address; set TAILSCALE_IP" >&2
	exit 1
fi
export TAILSCALE_IP

# Relay build version, stamped into sidecars and the status object.
if [ -z "${ZRELAY_VERSION:-}" ]; then
	ZRELAY_VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
fi
export ZRELAY_VERSION

# Both services need their config and secrets before they can start.
for f in .env zrelay.yaml zview.yaml secrets/zrelay_master secrets/s3_secret_key \
	secrets/api_token_sha256 secrets/s3_ro_secret_key; do
	if [ ! -e "deploy/$f" ]; then
		echo "deploy.sh: deploy/$f is missing (see the header of deploy/compose.yaml)" >&2
		exit 1
	fi
done

# Builds and starts zrelay and zview-server.
docker compose -f deploy/compose.yaml up -d --build
