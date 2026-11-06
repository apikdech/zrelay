#!/bin/sh
# Build and start the zrelay containers (design §8.3).
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

docker compose -f deploy/compose.yaml up -d --build
