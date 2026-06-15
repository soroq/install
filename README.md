# Soroq CLI

Public home for the Soroq CLI source, installers, and binary releases.

## Source

The Go CLI lives in:

```text
backend/cmd/soroq
```

Build from source:

```bash
cd backend
go test ./cmd/soroq ./internal/...
go build -o soroq ./cmd/soroq
./soroq --help
```

This repository intentionally contains the CLI slice only: the `soroq` command plus the internal Go packages it needs to build Android release/patch artifacts.

## Install on macOS or Linux

```bash
curl --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/soroq/install/main/install.sh -sSf | bash
```

Then restart your shell or add Soroq to your current session:

```bash
export PATH="$HOME/.soroq/bin:$PATH"
soroq --help
```

## Install on Windows

Run this in PowerShell:

```powershell
iwr https://raw.githubusercontent.com/soroq/install/main/install.ps1 -UseBasicParsing | iex
```

Then open a new PowerShell window and run:

```powershell
soroq --help
```

## What This Installs

The installer detects your OS and CPU, downloads the matching Soroq CLI archive from this repository's GitHub Releases, verifies `checksums.txt`, and installs the binary to:

```text
~/.soroq/bin/soroq
```

On Windows, the default path is:

```text
%USERPROFILE%\.soroq\bin\soroq.exe
```

Supported release assets:

- `soroq_darwin_arm64.tar.gz`
- `soroq_darwin_amd64.tar.gz`
- `soroq_linux_arm64.tar.gz`
- `soroq_linux_amd64.tar.gz`
- `soroq_windows_arm64.zip`
- `soroq_windows_amd64.zip`
- `checksums.txt`

## Options

Install a specific version:

```bash
SOROQ_INSTALL_VERSION=v0.1.16 sh install.sh
```

Install somewhere else:

```bash
SOROQ_INSTALL_DIR=/usr/local/bin sh install.sh
```

Install somewhere else on Windows:

```powershell
$env:SOROQ_INSTALL_DIR = "C:\Tools\soroq"
iwr https://raw.githubusercontent.com/soroq/install/main/install.ps1 -UseBasicParsing | iex
```

Use a different release repository:

```bash
SOROQ_INSTALL_REPO=soroq/install sh install.sh
```

Use private release assets when testing from a private repo:

```bash
SOROQ_INSTALL_REPO=soroq/soroq \
SOROQ_GITHUB_TOKEN="$(gh auth token)" \
sh install.sh
```

## Internal Maintainer Note

Tag a release such as `v0.1.1` to build and publish macOS, Linux, and Windows CLI archives from this public source tree.
