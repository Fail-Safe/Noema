#!/usr/bin/env bash
# sync-brand-assets.sh — copy the published brand SVGs from the
# private Noema-design repo into this repo's assets/brand/ so the
# README can reference them from a committed path.
#
# Noema-design is a separate git checkout. By default this script
# expects it as a sibling directory (../Noema-design); override with
# NOEMA_DESIGN_REPO=/path/to/Noema-design if you keep it elsewhere.
#
# Source of truth for the SVGs themselves is scripts/compose_brand_text.py
# inside Noema-design — when the brand changes, regenerate there first,
# then run this script to republish into the public repo.

set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
DESIGN_REPO_RAW="${NOEMA_DESIGN_REPO:-$REPO_ROOT/../Noema-design}"

if [[ ! -d "$DESIGN_REPO_RAW" ]]; then
  echo "error: Noema-design checkout not found at $DESIGN_REPO_RAW" >&2
  echo "       set NOEMA_DESIGN_REPO to your Noema-design checkout, or" >&2
  echo "       clone it as a sibling of this repo at $REPO_ROOT/../Noema-design" >&2
  exit 1
fi

DESIGN_REPO="$(cd -- "$DESIGN_REPO_RAW" && pwd)"
SRC="$DESIGN_REPO/graphics/name"
DST="$REPO_ROOT/assets/brand"

if [[ ! -d "$SRC" ]]; then
  echo "error: source dir $SRC not found — regenerate via scripts/compose_brand_text.py first" >&2
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

echo "done. assets/brand/ now matches $SRC"
