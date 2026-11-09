#!/bin/bash
# Installs maestro-d from a GitHub release: no Go, Node.js or checkout needed.
#
#   curl -fsSL https://github.com/EdgeApp/maestro-d/releases/latest/download/install.sh | bash
#   curl -fsSL …/install.sh | bash -s -- --version 2.0.0   # a release other than the latest
#   curl -fsSL …/install.sh | bash -s -- uninstall
#
# maestro-d goes into ~/.maestro-d (bin/maestro-d and drivers/, the home the
# binary resolves from its own path, the same one `make build` installs to),
# and a maestro-d link goes into the first of /usr/local/bin and
# /opt/homebrew/bin that is writable, else ~/.local/bin. No sudo. An upgrade
# replaces bin/ and drivers/ and keeps cache/ (WebDriverAgent builds) and the
# daemons' run files.
#
# Options: --version <x.y.z>  a release other than the latest
#          --prefix <dir>     install somewhere other than ~/.maestro-d
#          --bin-dir <dir>    put the maestro-d link there
#          uninstall          remove the program and its link, keep cache/
#
# MAESTRO_D_REPO picks another repository (default EdgeApp/maestro-d), and
# MAESTRO_D_RELEASE_URL a directory holding SHA256SUMS and the tarballs.
set -euo pipefail

repo=${MAESTRO_D_REPO:-EdgeApp/maestro-d}
prefix=${MAESTRO_D_PREFIX:-$HOME/.maestro-d}
bindir=${MAESTRO_D_BIN:-}
version=''
uninstall=0
while [[ $# -gt 0 ]]; do
  case $1 in
    --version) version=${2:?--version needs a version}; version=${version#v}; shift 2 ;;
    --prefix) prefix=${2:?--prefix needs a directory}; shift 2 ;;
    --bin-dir) bindir=${2:?--bin-dir needs a directory}; shift 2 ;;
    uninstall) uninstall=1; shift ;;
    *) echo "install.sh: unknown option $1" >&2; exit 2 ;;
  esac
done

fail() { echo "install.sh: $*" >&2; exit 1; }

# The link's directory: the one asked for, else the first writable one of
# the usual places on PATH, else ~/.local/bin.
pick_bindir() {
  if [[ -n $bindir ]]; then echo "$bindir"; return; fi
  local d
  for d in /usr/local/bin /opt/homebrew/bin; do
    if [[ -d $d && -w $d ]]; then echo "$d"; return; fi
  done
  echo "$HOME/.local/bin"
}

if [[ $uninstall == 1 ]]; then
  for d in "$bindir" /usr/local/bin /opt/homebrew/bin "$HOME/.local/bin"; do
    [[ -n $d && -L $d/maestro-d && $(readlink "$d/maestro-d") == "$prefix/bin/maestro-d" ]] || continue
    rm -f "$d/maestro-d"
    echo "Removed $d/maestro-d"
  done
  rm -rf "$prefix/bin" "$prefix/drivers" "$prefix/LICENSE"
  echo "Removed maestro-d from $prefix (kept its cache/ and run files; delete $prefix to remove them too)"
  exit 0
fi

case $(uname -s) in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "maestro-d runs on macOS and Linux, not $(uname -s)" ;;
esac
case $(uname -m) in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=x64 ;;
  *) fail "unsupported CPU $(uname -m)" ;;
esac
platform=$os-$arch

if [[ -n ${MAESTRO_D_RELEASE_URL:-} ]]; then
  base=$MAESTRO_D_RELEASE_URL
elif [[ -n $version ]]; then
  base=https://github.com/$repo/releases/download/maestro-d-v$version
else
  base=https://github.com/$repo/releases/latest/download
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fetch() { # fetch <name>
  curl -fsSL --retry 3 -o "$work/$1" "$base/$1" || fail "could not download $base/$1"
}

fetch SHA256SUMS
tarball=$(awk '{print $2}' "$work/SHA256SUMS" | grep -E "^maestro-d-[0-9][^ ]*-$platform\\.tar\\.gz$" | head -1 || true)
[[ -n $tarball ]] || fail "the release has no build for $platform"
fetch "$tarball"

# The tarball must match the release's SHA256SUMS.
expected=$(awk -v f="$tarball" '$2 == f {print $1}' "$work/SHA256SUMS")
if command -v sha256sum >/dev/null; then
  actual=$(sha256sum "$work/$tarball" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$work/$tarball" | awk '{print $1}')
fi
[[ $actual == "$expected" ]] || fail "$tarball does not match SHA256SUMS"

tar -xzf "$work/$tarball" -C "$work"
src=$work/${tarball%.tar.gz}
[[ -x $src/bin/maestro-d && -d $src/drivers ]] || fail "$tarball is not a maestro-d build"

# Replace bin/maestro-d by a rename, never by writing over it: a daemon
# running the old binary keeps its file, and macOS kills a process whose
# signed executable changes under it. drivers/ is swapped the same way.
mkdir -p "$prefix/bin"
cp "$src/bin/maestro-d" "$prefix/bin/.maestro-d.new"
mv -f "$prefix/bin/.maestro-d.new" "$prefix/bin/maestro-d"
rm -rf "$prefix/.drivers.new" "$prefix/.drivers.old"
cp -R "$src/drivers" "$prefix/.drivers.new"
if [[ -d $prefix/drivers ]]; then mv "$prefix/drivers" "$prefix/.drivers.old"; fi
mv "$prefix/.drivers.new" "$prefix/drivers"
rm -rf "$prefix/.drivers.old"
cp "$src/LICENSE" "$prefix/LICENSE"

link_dir=$(pick_bindir)
mkdir -p "$link_dir"
ln -sfn "$prefix/bin/maestro-d" "$link_dir/maestro-d"

echo "Installed $("$prefix/bin/maestro-d" --version | head -1) in $prefix"
echo "Linked $link_dir/maestro-d"
case ":$PATH:" in
  *":$link_dir:"*) ;;
  *) echo "Add $link_dir to your PATH:  export PATH=\"$link_dir:\$PATH\"" ;;
esac
if pgrep -f "$prefix/bin/maestro-d" >/dev/null 2>&1; then
  echo "maestro-d daemons started before this install keep running the old version until stopped: maestro-d stop --all"
fi
echo "Next: maestro-d doctor"
