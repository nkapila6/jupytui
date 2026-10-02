#!/bin/sh
# Install jupytui from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/nkapila6/jupytui/main/install.sh | sh
#
# INSTALL_DIR (default ~/.local/bin) and JUPYTUI_VERSION (default: latest)
# change where it goes and which release.
set -eu

repo="nkapila6/jupytui"
dir="${INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*"; }
die() { say "jupytui install: $*" >&2; exit 1; }

command -v curl >/dev/null || die "needs curl"
command -v tar >/dev/null || die "needs tar"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) die "no build for $(uname -s), try: go install github.com/$repo/cmd/jupytui@latest" ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) die "no build for $(uname -m), try: go install github.com/$repo/cmd/jupytui@latest" ;;
esac

version="${JUPYTUI_VERSION:-}"
if [ -z "$version" ]; then
  # the latest-release page redirects to .../tag/vX.Y.Z
  url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest") ||
    die "couldn't reach GitHub"
  version="${url##*/}"
fi
case "$version" in v*) ;; *) die "couldn't work out the latest version" ;; esac

name="jupytui_${version}_${os}_${arch}"
base="https://github.com/$repo/releases/download/$version"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "downloading jupytui $version ($os/$arch)"
curl -fsSL "$base/$name.tar.gz" -o "$tmp/$name.tar.gz" || die "download failed: $base/$name.tar.gz"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" || die "couldn't get checksums"

want=$(grep " $name.tar.gz\$" "$tmp/checksums.txt" | cut -d' ' -f1)
if command -v sha256sum >/dev/null; then
  got=$(sha256sum "$tmp/$name.tar.gz" | cut -d' ' -f1)
else
  got=$(shasum -a 256 "$tmp/$name.tar.gz" | cut -d' ' -f1)
fi
[ -n "$want" ] && [ "$want" = "$got" ] || die "checksum mismatch, not installing"

tar -xzf "$tmp/$name.tar.gz" -C "$tmp"
mkdir -p "$dir"
mv "$tmp/$name/jupytui" "$dir/jupytui"
chmod +x "$dir/jupytui"
say "installed $("$dir/jupytui" --version) to $dir/jupytui"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "note: $dir isn't on your PATH, add it to your shell config" ;;
esac
command -v uv >/dev/null || say "note: jupytui runs kernels with uv, get it from https://docs.astral.sh/uv/"
