#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${TV_COVERAGE_BASE:?Set TV_COVERAGE_BASE to the reviewed integration base}"
mkdir -p coverage/tvos
revision=$(python3 scripts/tvos-changed-coverage.py --digest)
packages=./pkg/driver/wda,./pkg/cli,./pkg/simulator
case "${1:-}" in
  unit)
    rm -rf coverage/tvos/unit
    mkdir -p coverage/tvos/unit
    printf '%s' "$revision" > coverage/tvos/unit.revision
    go test -cover -coverpkg="$packages" ./pkg/driver/wda/... ./pkg/cli/... ./pkg/simulator/... -args -test.gocoverdir="$PWD/coverage/tvos/unit"
    go tool covdata textfmt -i=coverage/tvos/unit -o=coverage/tvos/unit.txt
    python3 scripts/tvos-changed-coverage.py --base "$TV_COVERAGE_BASE" --profile coverage/tvos/unit.txt --minimum 90
    ;;
  build)
    rm -rf coverage/tvos/e2e
    mkdir -p coverage/tvos/e2e coverage/tvos/bin
    printf '%s' "$revision" > coverage/tvos/e2e.revision
    go build -cover -coverpkg="$packages,." -o coverage/tvos/bin/maestro-d .
    ;;
  combined)
    test "$revision" = "$(cat coverage/tvos/unit.revision)"
    test "$revision" = "$(cat coverage/tvos/e2e.revision)"
    test -n "$(find coverage/tvos/e2e -name 'covcounters.*' -print -quit)"
    rm -rf coverage/tvos/combined
    mkdir -p coverage/tvos/combined
    go tool covdata merge -i=coverage/tvos/unit,coverage/tvos/e2e -o=coverage/tvos/combined
    go tool covdata textfmt -i=coverage/tvos/combined -o=coverage/tvos/combined.txt
    python3 scripts/tvos-changed-coverage.py --base "$TV_COVERAGE_BASE" --profile coverage/tvos/combined.txt --minimum 95
    ;;
  *) echo 'Usage: tvos-coverage.sh unit|build|combined' >&2; exit 2 ;;
esac
