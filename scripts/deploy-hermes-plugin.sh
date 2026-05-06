#!/usr/bin/env bash
# Deploy the Hermes Noema memory provider plugin to one or more remote hosts.
#
# Usage:
#   scripts/deploy-hermes-plugin.sh <host> [<host> ...]
#
# Each host must be SSH-reachable. The script rsyncs plugins/hermes/ to
# <hermes-home>/plugins/memory/noema/ on each host (default hermes-home is
# ~/.hermes/hermes-agent — override with HERMES_HOME on the remote env).
# Then it restarts every hermes-gateway-*.service systemd --user unit so the
# new code is picked up.
#
# Idempotent. Safe to re-run. No-ops on hosts that don't have Hermes installed.
#
# Until `noema hermes install` (Tier 2) lands, this is the pull-mechanism
# replacement for the manual `cp -r` step in plugins/hermes/README.md.

set -euo pipefail

if [[ $# -lt 1 ]]; then
    echo "usage: $0 <host> [<host> ...]" >&2
    exit 64
fi

repo_root=$(cd "$(dirname "$0")/.." && pwd)
src="$repo_root/plugins/hermes/"

if [[ ! -f "$src/__init__.py" ]]; then
    echo "error: plugin source not found at $src" >&2
    exit 1
fi

for host in "$@"; do
    echo "==> $host"

    remote_home=$(ssh "$host" 'echo "${HERMES_HOME:-$HOME/.hermes/hermes-agent}"')
    remote_dest="$remote_home/plugins/memory/noema"

    if ! ssh "$host" "test -d $remote_home"; then
        echo "    Hermes not installed at $remote_home — skipping"
        continue
    fi

    ssh "$host" "mkdir -p $remote_dest"

    # --delete removes stale files (e.g. an old transport_legacy.py); excludes
    # __pycache__ so we don't ship local bytecode.
    rsync -az --delete \
        --exclude='__pycache__' \
        --exclude='.pytest_cache' \
        --exclude='.ruff_cache' \
        "$src" "$host:$remote_dest/"

    echo "    plugin synced to $remote_dest"

    # Restart every Hermes gateway profile so the new code loads. Prefer
    # the host's `hermes-profiles` utility if present — it owns the
    # canonical restart convention (sequencing, profile discovery, etc.)
    # and we should not re-implement it. Fall back to a direct systemctl
    # glob restart on hosts that don't have the utility installed.
    if ssh -n "$host" 'command -v hermes-profiles >/dev/null 2>&1'; then
        echo "    restarting via hermes-profiles gateways restart"
        ssh -n "$host" 'hermes-profiles gateways restart'
        continue
    fi

    units=$(ssh -n "$host" "systemctl --user list-unit-files 'hermes-gateway*.service' --no-legend 2>/dev/null | awk '{print \$1}'" || true)
    if [[ -z "$units" ]]; then
        echo "    no hermes-gateway-*.service units found — skipping restart"
        continue
    fi
    while IFS= read -r unit; do
        [[ -z "$unit" ]] && continue
        echo "    restarting $unit"
        ssh -n "$host" "systemctl --user restart $unit"
    done <<<"$units"
done

echo
echo "deploy complete."
