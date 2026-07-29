#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
unset NOEMA_DESIGN_REPO
source "$repo_root/.github/assets/brand/sync-brand-assets.sh"

if [[ "$REPO_ROOT" != "$repo_root" ]]; then
  echo "REPO_ROOT resolved to $REPO_ROOT, want $repo_root" >&2
  exit 1
fi
if [[ "$DESIGN_REPO_RAW" != "$repo_root/../Noema-design" ]]; then
  echo "default design repository resolved incorrectly" >&2
  exit 1
fi
if [[ "$DST" != "$repo_root/.github/assets/brand" ]]; then
  echo "brand destination resolved to $DST" >&2
  exit 1
fi
