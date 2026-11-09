#!/usr/bin/env bash
# Build the release tarballs that install.sh downloads.
#
# Usage: VERSION=2.0.0 ./npm/build-maestro-d-tarballs.sh
#
# Run it after npm/build-maestro-d-npm.sh, whose per-platform directories
# (npm/platforms/maestro-d-<os>-<arch>/: bin/maestro-d, drivers/, LICENSE)
# are already the home the binary expects. Each becomes
#
#   npm/dist-maestro-d/<VERSION>/maestro-d-<VERSION>-<os>-<arch>.tar.gz
#
# holding one directory, maestro-d-<VERSION>-<os>-<arch>/, with bin/,
# drivers/ and LICENSE. install.sh is copied next to them, and SHA256SUMS
# covers every tarball and install.sh. It does not publish; the release
# workflow attaches the directory's files to the GitHub release.
set -euo pipefail

VERSION="${VERSION:-}"
if [ -z "$VERSION" ]; then
    echo "Error: VERSION is required — VERSION=2.0.0 ./npm/build-maestro-d-tarballs.sh" >&2
    exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PLATFORMS_DIR="$REPO_ROOT/npm/platforms"
OUT_DIR="$REPO_ROOT/npm/dist-maestro-d/$VERSION"
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

# Owner and group 0 in the archive, so an extraction does not carry the
# build machine's account into the tarball.
if tar --version 2>/dev/null | grep -q 'GNU tar'; then
    TAR_OWNER=(--owner=0 --group=0 --numeric-owner)
else
    TAR_OWNER=(--uid 0 --gid 0)
fi

mkdir -p "$OUT_DIR"
found=0
for dir in "$PLATFORMS_DIR"/maestro-d-*/; do
    [ -x "$dir/bin/maestro-d" ] || continue
    platform="$(basename "$dir")"
    platform="${platform#maestro-d-}"
    name="maestro-d-$VERSION-$platform"

    mkdir -p "$STAGE/$name"
    cp -R "$dir/bin" "$dir/drivers" "$dir/LICENSE" "$STAGE/$name/"
    tar -czf "$OUT_DIR/$name.tar.gz" "${TAR_OWNER[@]}" -C "$STAGE" "$name"
    echo "Built $name.tar.gz"
    found=$((found + 1))
done
if [ "$found" -eq 0 ]; then
    echo "Error: no platform builds in $PLATFORMS_DIR — run npm/build-maestro-d-npm.sh first" >&2
    exit 1
fi

cp "$REPO_ROOT/install.sh" "$OUT_DIR/install.sh"

if command -v sha256sum >/dev/null; then
    sum() { sha256sum "$@"; }
else
    sum() { shasum -a 256 "$@"; }
fi
(cd "$OUT_DIR" && sum maestro-d-"$VERSION"-*.tar.gz install.sh > SHA256SUMS)

echo
echo "Release files for $VERSION in $OUT_DIR:"
cat "$OUT_DIR/SHA256SUMS"
