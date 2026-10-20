#!/usr/bin/env bash
# Installs the delamain binary of a GitHub release into ~/.local/bin.
#
#   curl -fsSL https://raw.githubusercontent.com/paraizofelipe/delamain/main/install.sh | bash
#
# DELAMAIN_VERSION      release tag to install (default: latest), e.g. v0.2.0
# DELAMAIN_INSTALL_DIR  destination directory (default: ~/.local/bin)
set -euo pipefail

version="${DELAMAIN_VERSION:-latest}"
dir="${DELAMAIN_INSTALL_DIR:-$HOME/.local/bin}"
base="${DELAMAIN_BASE_URL:-https://github.com/paraizofelipe/delamain/releases}" # overridden by tests

die() { echo "delamain install: $*" >&2; exit 1; }

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) die "sistema não suportado: $(uname -s) (só linux e darwin)" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "arquitetura não suportada: $(uname -m) (só amd64 e arm64)" ;;
esac
asset="delamain_${os}_${arch}"

if [ "$version" = latest ]; then
  url="$base/latest/download"
else
  [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "DELAMAIN_VERSION deve ser vX.Y.Z ou latest, recebido '$version'"
  url="$base/download/$version"
fi

command -v curl >/dev/null || die "curl não encontrado"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "baixando $asset ($version)..."
curl -fsSL --retry 3 -o "$tmp/$asset" "$url/$asset" || die "download falhou: $url/$asset"
curl -fsSL --retry 3 -o "$tmp/checksums.txt" "$url/checksums.txt" || die "download falhou: $url/checksums.txt"

grep -E "^[0-9a-f]{64}  \*?$asset\$" "$tmp/checksums.txt" >"$tmp/expected" || die "checksums.txt não lista $asset"
if command -v sha256sum >/dev/null; then
  (cd "$tmp" && sha256sum -c --status expected) || die "sha256 de $asset não confere"
else
  (cd "$tmp" && shasum -a 256 -c -s expected) || die "sha256 de $asset não confere"
fi

mkdir -p "$dir"
install -m 0755 "$tmp/$asset" "$dir/delamain"
echo "instalado: $dir/delamain $("$dir/delamain" version)"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "aviso: $dir não está no PATH; adicione ao ~/.zshrc ou ~/.bashrc:" >&2
     echo "  export PATH=\"$dir:\$PATH\"" >&2 ;;
esac

echo "gh/glab são opcionais: basta GH_TOKEN (GitHub) ou GITLAB_TOKEN (GitLab); sem eles, o delamain só lê o token do login do gh/glab."
