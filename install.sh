#!/usr/bin/env sh
set -eu

REPO="${SOROQ_INSTALL_REPO:-soroq/install}"
VERSION="${SOROQ_INSTALL_VERSION:-latest}"
INSTALL_DIR="${SOROQ_INSTALL_DIR:-$HOME/.soroq/bin}"
BINARY_NAME="soroq"
GITHUB_TOKEN_VALUE="${SOROQ_GITHUB_TOKEN:-${GITHUB_TOKEN:-}}"

if [ -t 1 ] && [ "${NO_COLOR:-}" = "" ]; then
  BOLD="$(printf '\033[1m')"
  DIM="$(printf '\033[2m')"
  RED="$(printf '\033[31m')"
  GREEN="$(printf '\033[32m')"
  YELLOW="$(printf '\033[33m')"
  BLUE="$(printf '\033[34m')"
  RESET="$(printf '\033[0m')"
else
  BOLD=""
  DIM=""
  RED=""
  GREEN=""
  YELLOW=""
  BLUE=""
  RESET=""
fi

say() {
  printf '%s\n' "$*"
}

banner() {
  say ""
  say "${BOLD}${BLUE}Soroq CLI Installer${RESET}"
  say "${DIM}Fast Android OTA release tooling, installed globally.${RESET}"
  say ""
}

info() {
  say "${BLUE}i${RESET} $*"
}

step() {
  say "${BLUE}>${RESET} $*"
}

success() {
  say "${GREEN}OK${RESET} $*"
}

warn() {
  say "${YELLOW}WARN${RESET} $*"
}

fail() {
  say "" >&2
  say "${RED}ERROR${RESET} $*" >&2
  say "" >&2
  say "${BOLD}What to try next${RESET}" >&2
  say "  - Re-run with verbose curl output: curl -fsSL <install-url> -o install.sh && sh install.sh" >&2
  say "  - Private repo? set SOROQ_GITHUB_TOKEN to a GitHub token that can read ${REPO}" >&2
  say "  - Pin a version: SOROQ_INSTALL_VERSION=v0.1.16 sh install.sh" >&2
  say "  - Change install path: SOROQ_INSTALL_DIR=/usr/local/bin sh install.sh" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "Missing required command: $1"
}

detect_os() {
  case "$(uname -s)" in
    Darwin) echo "darwin" ;;
    Linux) echo "linux" ;;
    *) fail "Unsupported operating system: $(uname -s). Soroq CLI releases currently support macOS and Linux." ;;
  esac
}

detect_arch() {
  case "$(uname -m)" in
    arm64 | aarch64) echo "arm64" ;;
    x86_64 | amd64) echo "amd64" ;;
    *) fail "Unsupported CPU architecture: $(uname -m). Soroq CLI releases currently support arm64 and amd64." ;;
  esac
}

download() {
  url="$1"
  output="$2"
  label="$3"

  if command -v curl >/dev/null 2>&1; then
    if [ -n "$GITHUB_TOKEN_VALUE" ]; then
      if ! curl --proto '=https' --tlsv1.2 -fsSL \
        -H "Authorization: Bearer $GITHUB_TOKEN_VALUE" \
        -H "X-GitHub-Api-Version: 2022-11-28" \
        "$url" -o "$output"; then
        fail "Could not download $label from $url. Check that the release exists and your GitHub token can read ${REPO}."
      fi
    elif ! curl --proto '=https' --tlsv1.2 -fsSL "$url" -o "$output"; then
      fail "Could not download $label from $url. Check that the release exists and your network can reach GitHub."
    fi
  elif command -v wget >/dev/null 2>&1; then
    if [ -n "$GITHUB_TOKEN_VALUE" ]; then
      if ! wget -q --header="Authorization: Bearer $GITHUB_TOKEN_VALUE" --header="X-GitHub-Api-Version: 2022-11-28" "$url" -O "$output"; then
        fail "Could not download $label from $url. Check that the release exists and your GitHub token can read ${REPO}."
      fi
    elif ! wget -q "$url" -O "$output"; then
      fail "Could not download $label from $url. Check that the release exists and your network can reach GitHub."
    fi
  else
    fail "Missing curl or wget. Install one of them and run the installer again."
  fi
}

checksum_cmd() {
  if command -v shasum >/dev/null 2>&1; then
    echo "shasum -a 256"
  elif command -v sha256sum >/dev/null 2>&1; then
    echo "sha256sum"
  else
    fail "Missing shasum or sha256sum. A checksum tool is required before installing downloaded binaries."
  fi
}

profile_hint() {
  shell_name="$(basename "${SHELL:-sh}")"
  case "$shell_name" in
    zsh) echo "$HOME/.zshrc" ;;
    bash) echo "$HOME/.bashrc" ;;
    fish) echo "$HOME/.config/fish/config.fish" ;;
    *) echo "your shell profile" ;;
  esac
}

banner

need_cmd tar
need_cmd uname
need_cmd mkdir
need_cmd mktemp
need_cmd mv
need_cmd chmod
need_cmd grep
need_cmd awk

os="$(detect_os)"
arch="$(detect_arch)"
asset="soroq_${os}_${arch}.tar.gz"

if [ "$VERSION" = "latest" ]; then
  base_url="https://github.com/${REPO}/releases/latest/download"
else
  base_url="https://github.com/${REPO}/releases/download/${VERSION}"
fi

tmp_dir="$(mktemp -d 2>/dev/null || mktemp -d -t soroq-install)"
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

archive="$tmp_dir/$asset"
checksums="$tmp_dir/checksums.txt"

info "Repository: ${BOLD}${REPO}${RESET}"
info "Version:    ${BOLD}${VERSION}${RESET}"
info "Target:     ${BOLD}${os}/${arch}${RESET}"
info "Install:    ${BOLD}${INSTALL_DIR}/${BINARY_NAME}${RESET}"
if [ -n "$GITHUB_TOKEN_VALUE" ]; then
  info "Auth:       ${BOLD}GitHub token detected${RESET}"
else
  info "Auth:       public GitHub release"
fi
say ""

step "Downloading ${asset}"
download "$base_url/$asset" "$archive" "$asset"
success "Downloaded CLI archive"

step "Downloading checksums"
download "$base_url/checksums.txt" "$checksums" "checksums.txt"
success "Downloaded checksum manifest"

step "Verifying checksum"
expected="$(grep "  $asset\$" "$checksums" | awk '{print $1}' || true)"
[ -n "$expected" ] || fail "checksums.txt does not contain an entry for $asset."

actual="$($(checksum_cmd) "$archive" | awk '{print $1}')"
if [ "$expected" != "$actual" ]; then
  say "${RED}Expected:${RESET} $expected" >&2
  say "${RED}Actual:  ${RESET} $actual" >&2
  fail "Checksum mismatch for $asset. The download may be corrupted or the release asset changed."
fi
success "Checksum verified"

step "Unpacking CLI"
if ! tar -xzf "$archive" -C "$tmp_dir"; then
  fail "Could not unpack $asset. The archive may be incomplete or corrupted."
fi
[ -x "$tmp_dir/$BINARY_NAME" ] || fail "Archive did not contain an executable named $BINARY_NAME."
success "Archive unpacked"

step "Installing binary"
if ! mkdir -p "$INSTALL_DIR"; then
  fail "Could not create install directory: $INSTALL_DIR"
fi
if ! mv "$tmp_dir/$BINARY_NAME" "$INSTALL_DIR/$BINARY_NAME"; then
  fail "Could not move $BINARY_NAME into $INSTALL_DIR. Check directory permissions."
fi
chmod 0755 "$INSTALL_DIR/$BINARY_NAME"
success "Installed ${BOLD}${INSTALL_DIR}/${BINARY_NAME}${RESET}"

step "Checking installation"
if ! "$INSTALL_DIR/$BINARY_NAME" --help >/dev/null 2>&1; then
  fail "Installed binary did not run successfully."
fi
success "Soroq CLI is ready"

say ""
say "${BOLD}${GREEN}Installation complete.${RESET}"

case ":$PATH:" in
  *":$INSTALL_DIR:"*)
    say "Run ${BOLD}soroq --help${RESET} to get started."
    ;;
  *)
    warn "$INSTALL_DIR is not currently on PATH."
    say "Add this to $(profile_hint):"
    say ""
    if [ "$(basename "${SHELL:-sh}")" = "fish" ]; then
      say "  fish_add_path $INSTALL_DIR"
    else
      say "  export PATH=\"$INSTALL_DIR:\$PATH\""
    fi
    say ""
    say "For this terminal session, you can run:"
    say "  export PATH=\"$INSTALL_DIR:\$PATH\""
    ;;
esac

say ""
say "${DIM}Tip: run 'soroq init' inside a Flutter app to prepare Android OTA release tooling.${RESET}"
