#!/usr/bin/env bash
# Run MultiSpeed's Go tests inside WSL for Linux-specific checks.
# Usage: bash scripts/wsl-test.sh [-race] [packages...]
set -u

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO" || exit 1
export PATH="$HOME/sdk/go/bin:$PATH"
export GOCACHE="$HOME/.cache/go-build"
export GOFLAGS="-p=1"
export GOMAXPROCS=2

RACE=0
if [[ "${1:-}" == "-race" ]]; then
  RACE=1
  shift
fi

PKGS=("$@")
if [[ ${#PKGS[@]} -eq 0 ]]; then
  PKGS=(./...)
fi

echo "repo=$REPO"
go version
free -m | head -3

if ((RACE)); then
  go test -race -count=1 "${PKGS[@]}"
else
  go test -count=1 "${PKGS[@]}"
fi
status=$?
echo "go test exit=$status"
exit "$status"
