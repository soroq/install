# Soroq CLI

Public home for the Soroq CLI **source, installers, and signed binary releases**.

## Build from source

`backend/` is the public Soroq CLI source — the same client-side code shipped in the binary
releases, exported deterministically from the main repo (operator-only publishing and all
private control-plane/store/S3 code are excluded). From a clean checkout:

```bash
cd backend
make build        # stamps ./VERSION -> ./soroq + ./soroqctl
./soroq version   # -> soroq v0.2.2
# or plainly:
go build ./cmd/soroq
go build ./cmd/soroqctl
go test ./...
```

No private module replacement, private Git dependency, or local path is required. The two
operator-only commands (`frontend publish`, `toolchain publish`) are intentionally not in this
build; every normal developer command (install/doctor, login/whoami/logout, init, release,
patch, rollback, …) is present.

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
soroq version   # -> soroq v0.2.2
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
SOROQ_INSTALL_VERSION=v0.2.2 sh install.sh
```

Install somewhere else:

```bash
SOROQ_INSTALL_DIR=/usr/local/bin sh install.sh
```

## Maintainer note

The public CLI module has been split out: `backend/` is a deterministic export of the client-side
CLI from the main Soroq repo (`scripts/export-public-cli.sh`), enforced by a drift-check CI in the
main repo. Releases are automated here by `.github/workflows/release.yml` (on `v*` tags): it builds
the macOS + Linux archives (and an experimental Windows candidate) from `backend/`, and emits
`checksums.txt`, `release-manifest.json`, an SBOM, and build-provenance attestations. `-rc`/`-beta`/
`-alpha` tags publish as prereleases and are not marked latest. `.github/workflows/ci.yml` gates
pushes/PRs (native Linux build+test+scans; informational Windows checks).
