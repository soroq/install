# Soroq CLI

Public home for the Soroq CLI **installers and signed binary releases**.

> **Source-build status.** This repo currently distributes released binaries. A full
> **build-from-source** path for the current CLI is **pending**: the CLI links private
> control-plane packages, so a clean public CLI module has not been split out yet. The
> `backend/` tree here is an older slice and does **not** build the current tested CLI — do
> not rely on it. Use the binary installer below.

Hard-OTA is an experimental tier. No App Store / Play **production** approval is claimed, no
Shorebird parity, and no arbitrary-Dart/Flutter parity. iOS hard-OTA is device-only.

## Supported platforms

| Platform | Status |
|---|---|
| macOS arm64 / amd64 | **Supported** (smoke-tested) |
| Linux amd64 / arm64 | **Supported** (smoke-tested in a container) |
| Windows | **Pending** (builds, not runtime-smoked; no published installer yet) |

## Install (macOS or Linux)

```bash
curl --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/soroq/install/main/install.sh -sSf | bash
```

Then add Soroq to your current shell (the installer also prints your profile file):

```bash
export PATH="$HOME/.soroq/bin:$PATH"
soroq version   # -> soroq v0.2.1
```

The installer detects your OS/CPU, downloads the matching archive from this repo's GitHub
Releases, verifies `checksums.txt`, and installs both `soroq` and `soroqctl` to
`~/.soroq/bin/`. `soroqctl` is required by the iOS engine lane (`soroq release/patch ios --engine`).

macOS Gatekeeper (only if a download is quarantined):

```bash
xattr -dr com.apple.quarantine "$HOME/.soroq/bin/soroq" "$HOME/.soroq/bin/soroqctl"
```

### Next steps

```bash
soroq frontend install soroq-flutter-frontend-f74781f6-6903c161 --api https://api.soroq.dev
soroq toolchain doctor
```

See the docs: <https://soroq.dev/getting-started>.

## Windows (pending)

A native Windows installer is not published yet. `soroq.exe` builds, but it has not been
runtime-smoke-tested, so it is not offered as supported. Track status on this repo.

## Published release assets

- `soroq_darwin_arm64.tar.gz`
- `soroq_darwin_amd64.tar.gz`
- `soroq_linux_amd64.tar.gz`
- `soroq_linux_arm64.tar.gz`
- `checksums.txt`

Each archive contains `soroq` + `soroqctl`, no secrets, no local paths, no private keys.

## Options

Pin a version:

```bash
SOROQ_INSTALL_VERSION=v0.2.1 sh install.sh
```

Install somewhere else:

```bash
SOROQ_INSTALL_DIR=/usr/local/bin sh install.sh
```

## Maintainer note

Release binaries are built from the main Soroq repository and uploaded to this repo's Releases;
the in-repo `cli-release` workflow is disabled (it rebuilt from this repo's stale source and
clobbered uploads). Re-enabling automated builds is gated on splitting a clean public CLI module.
