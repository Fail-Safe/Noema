#!/usr/bin/env bash
# sync-brand-assets.sh — copy the published brand SVGs from the
# Noema-design working tree (docs/design/graphics/name/, gitignored
# in the main repo) into assets/brand/ so the README can reference
# them from a committed path.
#
# Source of truth for the SVGs themselves is docs/design/scripts/
# compose_brand_text.py — when the brand changes, regenerate there
# first, then run this script to republish into the public repo.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
SRC="$REPO_ROOT/docs/design/graphics/name"
DST="$REPO_ROOT/assets/brand"

if [[ ! -d "$SRC" ]]; then
  echo "error: source dir $SRC not found — is Noema-design checked out here?" >&2
  exit 1
fi

mkdir -p "$DST"

# Published variants (add more here if the README ever references them).
FILES=(
  "noema-dark.svg"
  "noema-light.svg"
)

for f in "${FILES[@]}"; do
  if [[ ! -f "$SRC/$f" ]]; then
    echo "error: $SRC/$f missing — regenerate via compose_brand_text.py first" >&2
    exit 1
  fi
  cp "$SRC/$f" "$DST/$f"
  echo "synced $f"
done

echo "done. assets/brand/ now matches docs/design/graphics/name/"
