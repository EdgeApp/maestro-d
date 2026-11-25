#!/usr/bin/env bash
# Build the maestro-d npm packages.
#
# Usage: VERSION=0.1.0 ./npm/build-maestro-d-npm.sh [--targets "darwin/arm64 linux/amd64"]
#
# Unlike npm/build-npm.sh (which repackages maestro-runner's signed release
# tarballs) this cross-compiles the daemon binary itself: the fork has no
# separate release pipeline to consume. Output:
#
#   npm/platforms/maestro-d-<os>-<arch>/   one package per target with
#       bin/maestro-d, drivers/ and LICENSE — the layout the binary
#       resolves its home from (<home>/bin/maestro-d → <home>/drivers)
#   npm/maestro-d/                         version stamped into
#       package.json and its optionalDependencies, dist/ built
#   npm/dist-maestro-d/<VERSION>/*.tgz             `npm pack` of every package
#
# It does not publish; the release workflow (or a person) does that after
# looking at the result.
set -euo pipefail

VERSION="${VERSION:-}"
if [ -z "$VERSION" ]; then
    echo "Error: VERSION is required — VERSION=0.1.0 ./npm/build-maestro-d-npm.sh" >&2
    exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NPM_DIR="$REPO_ROOT/npm"
PKG_DIR="$NPM_DIR/maestro-d"
PLATFORMS_DIR="$NPM_DIR/platforms"
OUT_DIR="$NPM_DIR/dist-maestro-d/$VERSION"

# Go's GOARCH and Node's process.arch disagree on one name (amd64 vs x64),
# so the mapping is explicit: "<go-os>/<go-arch>" → "<node-os>-<node-arch>".
ALL_TARGETS="darwin/arm64 darwin/amd64 linux/arm64 linux/amd64"
TARGETS="$ALL_TARGETS"
if [ "${1:-}" = "--targets" ]; then
    TARGETS="$2"
fi

node_target() {
    case "$1" in
        darwin/arm64) echo "darwin-arm64" ;;
        darwin/amd64) echo "darwin-x64" ;;
        linux/arm64) echo "linux-arm64" ;;
        linux/amd64) echo "linux-x64" ;;
        *) echo "Error: unsupported target $1" >&2; exit 1 ;;
    esac
}

COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo none)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w \
 -X github.com/devicelab-dev/maestro-runner/pkg/version.Version=${VERSION} \
 -X github.com/devicelab-dev/maestro-runner/pkg/version.Commit=${COMMIT} \
 -X github.com/devicelab-dev/maestro-runner/pkg/version.BuildDate=${BUILD_DATE}"

rm -rf "$PLATFORMS_DIR" "$OUT_DIR"
mkdir -p "$PLATFORMS_DIR" "$OUT_DIR"

for target in $TARGETS; do
    go_os="${target%/*}"
    go_arch="${target#*/}"
    node_os_arch="$(node_target "$target")"
    node_os="${node_os_arch%-*}"
    node_arch="${node_os_arch#*-}"
    pkg="maestro-d-${node_os_arch}"
    dest="$PLATFORMS_DIR/$pkg"

    echo "Building $pkg"
    mkdir -p "$dest/bin"
    (cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS="$go_os" GOARCH="$go_arch" \
        go build -trimpath -ldflags "$LDFLAGS" -o "$dest/bin/maestro-d" .)

    # The binary finds drivers at <home>/drivers, and its home is the parent
    # of its bin/ directory — the package itself.
    cp -R "$REPO_ROOT/drivers" "$dest/drivers"
    rm -f "$dest/drivers/embed.go"
    cp "$REPO_ROOT/LICENSE" "$dest/LICENSE"

    cat > "$dest/package.json" <<EOF
{
  "name": "$pkg",
  "version": "$VERSION",
  "description": "maestro-d binary for ${node_os} ${node_arch}. Installed automatically as an optional dependency of maestro-d.",
  "homepage": "https://github.com/EdgeApp/maestro-d",
  "repository": {
    "type": "git",
    "url": "git+https://github.com/EdgeApp/maestro-d.git"
  },
  "license": "Apache-2.0",
  "os": ["$node_os"],
  "cpu": ["$node_arch"],
  "files": ["bin/", "drivers/", "LICENSE"],
  "preferUnplugged": true
}
EOF
    (cd "$dest" && npm pack --silent --pack-destination "$OUT_DIR" >/dev/null)
done

# Stamp the version into the main package and the optional dependencies that
# must resolve to the exact same build, then build and pack it.
node - "$PKG_DIR/package.json" "$VERSION" <<'NODE'
const fs = require('node:fs');
const [file, version] = process.argv.slice(2);
const pkg = JSON.parse(fs.readFileSync(file, 'utf8'));
pkg.version = version;
for (const dep of Object.keys(pkg.optionalDependencies)) {
  pkg.optionalDependencies[dep] = version;
}
fs.writeFileSync(file, JSON.stringify(pkg, null, 2) + '\n');
NODE

echo "Building maestro-d"
(cd "$PKG_DIR" && npm ci --no-audit --no-fund --silent && npm run --silent build \
    && npm pack --silent --pack-destination "$OUT_DIR" >/dev/null)

echo
echo "Built npm packages for $VERSION in $OUT_DIR:"
ls -1 "$OUT_DIR"
echo
echo "Try one locally:"
echo "  npm install --no-save $OUT_DIR/maestro-d-$VERSION.tgz $OUT_DIR/maestro-d-$(node_target "$(go env GOOS)/$(go env GOARCH)")-$VERSION.tgz"
echo "  npx maestro-d --version"
echo
echo "Publish — platform packages first, so the main package never resolves to a version that does not exist:"
for target in $TARGETS; do
    echo "  npm publish $OUT_DIR/maestro-d-$(node_target "$target")-$VERSION.tgz --access public"
done
echo "  npm publish $OUT_DIR/maestro-d-$VERSION.tgz --access public"
