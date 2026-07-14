#!/usr/bin/env sh
set -eu

REPO="${SOROQ_INSTALL_REPO:-soroq/install}"
VERSION="${SOROQ_INSTALL_VERSION:-latest}"
INSTALL_DIR="${SOROQ_INSTALL_DIR:-$HOME/.soroq/bin}"
BINARY_NAME="soroq"
GITHUB_TOKEN_VALUE="${SOROQ_GITHUB_TOKEN:-${GITHUB_TOKEN:-}}"

# Managed-block markers for the idempotent PATH entry the installer appends to the
# active shell profile. Do not change these once shipped — re-runs match on them.
SOROQ_PATH_BLOCK_BEGIN="# >>> soroq PATH >>>"
SOROQ_PATH_BLOCK_END="# <<< soroq PATH <<<"

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
  say "${DIM}Soroq hard-OTA release tooling (Android + iOS), installed globally.${RESET}"
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
  say "  - Pin a version: SOROQ_INSTALL_VERSION=<version> sh install.sh   (e.g. v0.2.2)" >&2
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

# Profile file to auto-manage for the active shell. Only zsh and bash are
# configured automatically (the two supported interactive shells); anything else
# (fish, unknown) returns empty so the caller prints a manual hint instead.
# bash reads a different login file per OS: macOS Terminal starts a login shell
# (~/.bash_profile), Linux terminals start interactive non-login shells (~/.bashrc).
target_profile() {
  shell_name="$(basename "${SHELL:-sh}")"
  case "$shell_name" in
    zsh) echo "$HOME/.zshrc" ;;
    bash)
      if [ "${os:-}" = "darwin" ]; then
        echo "$HOME/.bash_profile"
      else
        echo "$HOME/.bashrc"
      fi
      ;;
    *) echo "" ;;
  esac
}

# The PATH value to write. Keep it $HOME-relative when INSTALL_DIR is under $HOME
# so the profile line is portable (matches the documented export in the README).
managed_path_spec() {
  case "$INSTALL_DIR" in
    "$HOME") printf '$HOME' ;;
    "$HOME"/*) printf '$HOME%s' "${INSTALL_DIR#"$HOME"}" ;;
    *) printf '%s' "$INSTALL_DIR" ;;
  esac
}

# Emit the profile's contents with any existing managed block removed (inclusive
# of the marker lines). Used to replace a stale block instead of appending a new one.
strip_managed_block() {
  awk -v b="$SOROQ_PATH_BLOCK_BEGIN" -v e="$SOROQ_PATH_BLOCK_END" '
    $0 == b { inblock = 1; next }
    $0 == e { inblock = 0; next }
    !inblock { print }
  ' "$1"
}

# Idempotently ensure the profile contains exactly one managed PATH block.
# Sets CONFIG_STATUS to "already" | "updated" | "added". Non-destructive: existing
# content is preserved and the block is appended (the profile is created if absent).
CONFIG_STATUS=""
configure_path() {
  profile="$1"
  path_spec="$2"
  line="export PATH=\"$path_spec:\$PATH\""

  if [ ! -e "$profile" ]; then
    if ! : > "$profile" 2>/dev/null; then
      return 1
    fi
  fi
  [ -w "$profile" ] || return 1

  if grep -qF "$SOROQ_PATH_BLOCK_BEGIN" "$profile" 2>/dev/null; then
    if grep -qF "$line" "$profile" 2>/dev/null; then
      CONFIG_STATUS="already"
      return 0
    fi
    # Stale managed block (e.g. install dir changed): replace it, keeping one block.
    stripped="$(mktemp 2>/dev/null || mktemp -t soroq-profile)" || return 1
    if ! strip_managed_block "$profile" > "$stripped"; then
      rm -f "$stripped"
      return 1
    fi
    cat "$stripped" > "$profile"
    rm -f "$stripped"
    CONFIG_STATUS="updated"
  else
    CONFIG_STATUS="added"
  fi

  {
    printf '\n%s\n' "$SOROQ_PATH_BLOCK_BEGIN"
    printf '%s\n' "# Added by the Soroq CLI installer. Managed block; safe to remove as one unit."
    printf '%s\n' "$line"
    printf '%s\n' "$SOROQ_PATH_BLOCK_END"
  } >> "$profile"
}

# Persist PATH for the active shell. Returns non-zero (so the caller falls back to
# a manual hint) for unsupported shells, install dirs outside $HOME, or write errors.
persist_path() {
  profile="$(target_profile)"
  [ -n "$profile" ] || return 2
  case "$INSTALL_DIR" in
    "$HOME" | "$HOME"/*) : ;;
    *) return 2 ;;
  esac
  configure_path "$profile" "$(managed_path_spec)"
}

# Preference-ordered list of "global" bin directories to expose the CLI from.
# Selection (see install_global_symlinks) prefers a candidate that is BOTH writable
# AND already on the CURRENT shell's PATH, then falls back to any writable one.
# Rationale per entry:
#   /usr/local/bin   - on the macOS default PATH (/etc/paths), so it works in
#                      ordinary shells whose inherited/default PATH includes it;
#                      usually needs sudo to write. (No installer can reach a
#                      deliberately restricted PATH like PATH=/usr/bin:/bin.)
#   /opt/homebrew/bin - Apple Silicon Homebrew prefix: writable + on the
#                      interactive PATH (fixes the current terminal immediately).
#   $HOME/.local/bin - per-user fallback; created if missing; on many PATHs.
# Test-only seam: SOROQ_GLOBAL_BIN_DIRS (a ':'-separated list) overrides the list
# so tests can point exclusively at temp dirs (same spirit as SOROQ_INSTALL_LIB_ONLY).
global_bin_candidates() {
  if [ -n "${SOROQ_GLOBAL_BIN_DIRS:-}" ]; then
    printf '%s\n' "$SOROQ_GLOBAL_BIN_DIRS" | tr ':' '\n'
    return 0
  fi
  printf '%s\n' \
    "/usr/local/bin" \
    "/opt/homebrew/bin" \
    "$HOME/.local/bin"
}

# Binary names to expose globally. soroqctl is only linked when it was installed
# (it ships in the same archive but may be absent in older releases).
global_link_names() {
  echo "$BINARY_NAME"
  [ -x "$INSTALL_DIR/soroqctl" ] && echo "soroqctl"
  return 0
}

# True only if $1 is a symlink that WE own, i.e. it points at a file inside
# $INSTALL_DIR. Used to distinguish a refreshable Soroq symlink from a real file
# or another tool's binary (which must never be clobbered).
is_our_symlink() {
  target="$1"
  [ -L "$target" ] || return 1
  dest="$(readlink "$target" 2>/dev/null)" || return 1
  case "$dest" in
    "$INSTALL_DIR"/*) return 0 ;;
    *) return 1 ;;
  esac
}

# Try to symlink every managed name into $1. Non-destructive and idempotent:
#   - target missing              -> create the symlink
#   - target is our symlink       -> refresh it (points back into $INSTALL_DIR)
#   - target is a foreign file    -> touch NOTHING; set SKIP_REASON and return 1
#                                    so the caller tries the next candidate dir.
SKIP_REASON=""
link_into_global_dir() {
  dir="$1"
  SKIP_REASON=""

  # Pre-flight: bail before creating ANY link if a foreign file would be clobbered,
  # so we never leave a partial (soroq linked, soroqctl blocked) state behind.
  # Read names line-by-line (IFS= read -r) so a path/name with spaces is safe.
  names="$(global_link_names)"
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    target="$dir/$name"
    if [ -e "$target" ] || [ -L "$target" ]; then
      if ! is_our_symlink "$target"; then
        SKIP_REASON="$target exists and is not a Soroq symlink"
        return 1
      fi
    fi
  done <<EOF
$names
EOF

  while IFS= read -r name; do
    [ -n "$name" ] || continue
    ln -sf "$INSTALL_DIR/$name" "$dir/$name" || return 1
  done <<EOF
$names
EOF
  return 0
}

# Expose the CLI globally by symlinking into a candidate dir so `soroq` resolves
# immediately (current shell + others) instead of only after a profile re-source.
# Two passes so a candidate already on the CURRENT PATH wins even if an earlier-
# ranked one is only writable: pass 1 = writable AND on $PATH (usable right now);
# pass 2 = any writable (works in ordinary new shells whose default PATH includes
# it). Never uses sudo; never clobbers a non-Soroq file (skips the dir + records
# why). Sets GLOBAL_LINK_DIR on success and accumulates GLOBAL_LINK_SKIPPED.
# Candidates are read line-by-line (IFS= read -r) so paths with spaces are safe.
GLOBAL_LINK_DIR=""
GLOBAL_LINK_SKIPPED=""
dir_on_path() {
  case ":$PATH:" in *":$1:"*) return 0 ;; *) return 1 ;; esac
}
install_global_symlinks() {
  GLOBAL_LINK_DIR=""
  GLOBAL_LINK_SKIPPED=""
  candidates="$(global_bin_candidates)"
  for pass in on_path any; do
    while IFS= read -r dir; do
      [ -n "$dir" ] || continue
      if [ ! -d "$dir" ]; then
        # Only create the per-user fallback; never mkdir a system dir (needs sudo /
        # would be surprising). Skip missing system dirs.
        case "$dir" in
          "$HOME"/*) mkdir -p "$dir" 2>/dev/null || continue ;;
          *) continue ;;
        esac
      fi
      [ -w "$dir" ] || continue
      # Pass 1 only considers dirs already on the live PATH (immediately usable).
      if [ "$pass" = "on_path" ] && ! dir_on_path "$dir"; then
        continue
      fi
      if link_into_global_dir "$dir"; then
        GLOBAL_LINK_DIR="$dir"
        return 0
      fi
      # Record a foreign-file skip once (a dir rejected in pass 1 is re-seen in
      # pass 2; only add it if we haven't noted it already).
      case ";$GLOBAL_LINK_SKIPPED;" in
        *";$dir ($SKIP_REASON);"*) : ;;
        *) GLOBAL_LINK_SKIPPED="${GLOBAL_LINK_SKIPPED}${GLOBAL_LINK_SKIPPED:+; }$dir ($SKIP_REASON)" ;;
      esac
    done <<EOF
$candidates
EOF
  done
  return 1
}

# Allow the helper functions above to be sourced (for tests) without running the
# installer: `SOROQ_INSTALL_LIB_ONLY=1 . ./install.sh`.
if [ "${SOROQ_INSTALL_LIB_ONLY:-}" = "1" ]; then
  return 0 2>/dev/null || exit 0
fi

banner

need_cmd tar
need_cmd uname
need_cmd mkdir
need_cmd mktemp
need_cmd mv
need_cmd chmod
need_cmd grep
need_cmd awk
need_cmd ln
need_cmd readlink

os="$(detect_os)"
arch="$(detect_arch)"
asset="soroq_${os}_${arch}.tar.gz"

# Hard-OTA beta: macOS (darwin) and Linux release assets are published and smoke-tested.
# Windows is pending (a native installer is not published yet) — fail clearly instead of a
# confusing 404 for any other OS.
case "$os" in
  darwin | linux) : ;;
  *) fail "The Soroq CLI ships macOS (darwin) and Linux binaries for the hard-OTA beta. '$os' is not supported yet (Windows is pending). See https://github.com/soroq/install for current status." ;;
esac

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

# soroqctl ships in the same archive and is required by the iOS engine lane
# (`soroq release/patch ios --engine` delegates to soroqctl). Install it when present.
if [ -x "$tmp_dir/soroqctl" ]; then
  if mv "$tmp_dir/soroqctl" "$INSTALL_DIR/soroqctl"; then
    chmod 0755 "$INSTALL_DIR/soroqctl"
    success "Installed ${BOLD}${INSTALL_DIR}/soroqctl${RESET}"
  fi
fi

step "Checking installation"
if ! "$INSTALL_DIR/$BINARY_NAME" --help >/dev/null 2>&1; then
  fail "Installed binary did not run successfully."
fi
success "Soroq CLI is ready"

say ""
say "${BOLD}${GREEN}Installation complete.${RESET}"

# Primary global-availability mechanism: symlink the CLI into a bin dir that is
# already on PATH, so `soroq` resolves NOW without re-sourcing a profile. The
# canonical binaries stay in $INSTALL_DIR (the self-updater's target); the
# symlink resolves through to them and keeps working after `soroq update`.
step "Linking ${BINARY_NAME} into a global bin directory"
linked_globally=0
if install_global_symlinks; then
  linked_globally=1
  linked_names="$BINARY_NAME"
  [ -x "$INSTALL_DIR/soroqctl" ] && linked_names="$linked_names, soroqctl"
  success "Linked ${BOLD}${linked_names}${RESET} into ${BOLD}${GLOBAL_LINK_DIR}${RESET}"
else
  warn "No writable global bin directory found; relying on your shell profile below."
fi
if [ -n "$GLOBAL_LINK_SKIPPED" ]; then
  info "Left existing files untouched (not Soroq symlinks): $GLOBAL_LINK_SKIPPED"
fi

# Fallback so `soroq` still resolves in shells where the symlink dir is not on
# PATH (kept regardless of the symlink outcome).
step "Configuring PATH"
if persist_path; then
  updated_profile="$(target_profile)"
  case "$CONFIG_STATUS" in
    already) success "PATH already configured in ${BOLD}${updated_profile}${RESET}" ;;
    *) success "Added ${BOLD}${INSTALL_DIR}${RESET} to your PATH in ${BOLD}${updated_profile}${RESET}" ;;
  esac
else
  # Unsupported shell (e.g. fish), a non-$HOME install dir, or a non-writable
  # profile: don't touch any file — print manual instructions instead.
  warn "Could not configure PATH automatically for this shell/install dir."
  say "Add this to $(profile_hint):"
  say ""
  if [ "$(basename "${SHELL:-sh}")" = "fish" ]; then
    say "  fish_add_path $INSTALL_DIR"
  else
    say "  export PATH=\"$INSTALL_DIR:\$PATH\""
  fi
fi

# Single authoritative "is it usable now?" summary. `soroq` resolves in THIS
# shell if the dir we linked into (or the install dir) is already on the live PATH.
usable_now=0
on_path=""
if [ "$linked_globally" = "1" ]; then
  case ":$PATH:" in *":$GLOBAL_LINK_DIR:"*) usable_now=1; on_path="$GLOBAL_LINK_DIR" ;; esac
fi
if [ "$usable_now" = "0" ]; then
  case ":$PATH:" in *":$INSTALL_DIR:"*) usable_now=1; on_path="$INSTALL_DIR" ;; esac
fi

say ""
if [ "$usable_now" = "1" ]; then
  success "${BOLD}soroq${RESET} is available now (via ${on_path})."
  say "Run ${BOLD}soroq --help${RESET} to get started."
else
  # Only the profile was updated (or nothing was on the current PATH): the
  # current shell needs to pick it up.
  say "Open a new terminal, or reload this one, to use ${BOLD}soroq${RESET}:"
  say "  ${BOLD}exec \$SHELL${RESET}"
fi

# /usr/local/bin is on the macOS default PATH (/etc/paths), so linking there makes
# soroq resolve in ordinary shells whose inherited/default PATH includes it (it
# cannot reach a deliberately restricted PATH like /usr/bin:/bin). Writing it
# usually needs sudo, so offer it as OPTIONAL guidance only; never run sudo
# ourselves. Skip if we already landed there.
if [ "$GLOBAL_LINK_DIR" != "/usr/local/bin" ]; then
  say ""
  say "${DIM}Optional — link into /usr/local/bin (on the default PATH of ordinary shells) with sudo:${RESET}"
  say "  sudo ln -sf \"$INSTALL_DIR/soroq\" /usr/local/bin/soroq && sudo ln -sf \"$INSTALL_DIR/soroqctl\" /usr/local/bin/soroqctl"
fi

say ""
say "${DIM}Next: soroq frontend install soroq-flutter-frontend-f74781f6-6903c161 --api https://api.soroq.dev  then  soroq toolchain doctor${RESET}"
