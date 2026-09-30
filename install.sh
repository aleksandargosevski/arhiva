#!/usr/bin/env bash
set -euo pipefail

REPO="aleksandargosevski/arhiva"
BINARY="arhiva"
INSTALL_DIR="${ARHIVA_INSTALL_DIR:-/usr/local/bin}"

main() {
  local arch version url

  if [ "$(uname -s)" != "Darwin" ]; then
    echo "Error: ${BINARY} only supports macOS" >&2
    exit 1
  fi

  arch=$(detect_arch)
  version=$(latest_version)

  if [ -z "$version" ]; then
    echo "Error: could not determine latest version" >&2
    exit 1
  fi

  echo "Installing ${BINARY} ${version} (darwin/${arch})..."

  url="https://github.com/${REPO}/releases/download/${version}/${BINARY}_${version#v}_darwin_${arch}.tar.gz"

  local tmpdir
  tmpdir=$(mktemp -d)
  trap 'rm -rf "$tmpdir"' EXIT

  echo "Downloading ${url}..."
  curl -fsSL "$url" -o "${tmpdir}/${BINARY}.tar.gz"

  tar -xzf "${tmpdir}/${BINARY}.tar.gz" -C "$tmpdir"

  if [ ! -f "${tmpdir}/${BINARY}" ]; then
    echo "Error: binary not found in archive" >&2
    exit 1
  fi

  install_binary "${tmpdir}/${BINARY}"

  echo ""
  echo "✓ ${BINARY} ${version} installed to ${INSTALL_DIR}/${BINARY}"
  echo ""
  echo "For cd on quit, see: https://github.com/${REPO}#shell-integration-cd-on-quit"
}

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)  echo "amd64" ;;
    arm64|aarch64) echo "arm64" ;;
    *)
      echo "Error: unsupported architecture: $(uname -m)" >&2
      exit 1
      ;;
  esac
}

latest_version() {
  curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
    | grep '"tag_name"' \
    | head -1 \
    | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/'
}

install_binary() {
  local src="$1"

  if [ -w "$INSTALL_DIR" ]; then
    cp "$src" "${INSTALL_DIR}/${BINARY}"
    chmod +x "${INSTALL_DIR}/${BINARY}"
  else
    echo "Need sudo to install to ${INSTALL_DIR}"
    sudo cp "$src" "${INSTALL_DIR}/${BINARY}"
    sudo chmod +x "${INSTALL_DIR}/${BINARY}"
  fi
}

main
