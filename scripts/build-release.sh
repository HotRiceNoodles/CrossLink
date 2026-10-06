#!/usr/bin/env bash
# Local release helper for CrossLink.
#
# Real releases are performed by CI: pushing a vX.Y.Z tag triggers
# .github/workflows/release.yml, which runs goreleaser and publishes
# GitHub Release binaries plus Docker images.
#
# This script only exercises the pipeline locally (snapshot mode —
# nothing is published). Works in git bash on Windows.
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v goreleaser >/dev/null 2>&1; then
  echo "ERROR: goreleaser is not installed." >&2
  echo "  Install: https://goreleaser.com/install/ (prebuilt Windows binaries available)" >&2
  exit 1
fi

echo ">> Running goreleaser in snapshot mode (no publishing)..."
goreleaser release --snapshot --clean

echo ""
echo "Done. Artifacts are under dist/."
echo "To cut a real release: review CHANGELOG.md, commit it, then push a tag:"
echo "  git tag vX.Y.Z && git push origin vX.Y.Z   # CI does the rest"
