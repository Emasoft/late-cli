#!/usr/bin/env bash
# install-dev.sh — replace a brew-managed `late` with a symlink into this
# repository's locally built binary, Homebrew-style: the symlink lives in
# brew's bin dir on PATH, but points at bin/late in this repo, so every
# `make build` updates the command in place. Nothing is copied.
#
# Usage:
#   brew uninstall late
#   ./install-dev.sh              # optionally: LATE_DEV_VERSION=x ./install-dev.sh
#
# Go back to brew:
#   rm "$(brew --prefix)/bin/late" && brew install late
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$SCRIPT_DIR"
BREW_BIN="$(brew --prefix)/bin"

if ! command -v brew >/dev/null 2>&1; then
    echo "Error: brew not found — this script links into Homebrew's bin dir." >&2
    exit 1
fi

# Refuse to fight an active brew install: replacing brew's symlink out from
# under it would leave brew's link tracking desynced.
if brew list --formula 2>/dev/null | grep -qx 'late' || brew list --cask 2>/dev/null | grep -qx 'late'; then
    echo "Error: late is still installed via brew — run: brew uninstall late" >&2
    exit 1
fi

echo "=> Building late from $REPO..."
if [[ -n "${LATE_DEV_VERSION:-}" ]]; then
    make -C "$REPO" build VERSION="$LATE_DEV_VERSION"
else
    make -C "$REPO" build
fi

echo "=> Symlinking $BREW_BIN/late -> $REPO/bin/late"
ln -sfn "$REPO/bin/late" "$BREW_BIN/late"

# Friction check: brew's bin must be on PATH for the command to resolve.
if ! command -v late >/dev/null 2>&1; then
    echo "" >&2
    echo "⚠️  $BREW_BIN is not in your PATH — add it to your ~/.zshrc or ~/.bashrc:" >&2
    echo "    export PATH=\"\$PATH:$BREW_BIN\"" >&2
    exit 1
fi

echo "=> Success! 'late' now resolves to:"
command -v late
late -version
