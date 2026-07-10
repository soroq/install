# Windows acceptance kit (Soroq CLI)

**Status: Windows is PENDING.** The Soroq CLI is **supported** on macOS and Linux only. `soroq.exe`
and `soroqctl.exe` build on Windows and pass a non-interactive CI job (see `.github/workflows/ci.yml`,
job `windows (informational)`), but automated CI cannot cover the interactive, production paths a real
operator depends on: a browser login against the live hosted surface, credential storage/revocation,
and real frontend/toolchain installs.

Windows stays PENDING until a human completes **every** gate below on a real Windows machine, against
production (`https://soroq.dev` / `https://api.soroq.dev`), and records a pass. Until then:

- `install.sh` does not offer Windows.
- `install.ps1` refuses to run unless `SOROQ_INSTALL_ALLOW_WINDOWS=1` is set (opt-in for testers).
- No Windows asset is advertised as supported (the release zip is labeled pending/experimental).

Honest boundaries still apply: hard-OTA is an experimental tier; iOS is device-only; no App
Store / Play production approval, no Shorebird parity, no arbitrary-Dart/Flutter parity.

---

## Environment

- Windows 10/11 x64, PowerShell 5.1 or PowerShell 7+.
- A real browser (Edge/Chrome) for the loopback login.
- Network access to `https://soroq.dev` and `https://api.soroq.dev`.

Note on credential storage: **there is no Windows Credential Manager integration.** The macOS
Keychain path is macOS-only; on Windows the browser-login token falls back to `config.json`. That
file must therefore be protected by NTFS ACLs (owner-only), which one of the gates below verifies.

---

## Setup (opt-in install)

```powershell
# From a clone of soroq/install (Set-Location, not cd, if a path has spaces):
$env:SOROQ_INSTALL_ALLOW_WINDOWS = "1"
$env:SOROQ_INSTALL_DIR = "$HOME\.soroq\bin"
.\install.ps1

# Put the bin dir on PATH for this session:
$env:PATH = "$HOME\.soroq\bin;$env:PATH"
soroq version   # expect: soroq v0.2.1
```

---

## Interactive acceptance gates

Run these **in order**. Mark each PASS/FAIL in the checklist at the bottom. A single FAIL keeps
Windows PENDING.

### G1 — Production browser login (loopback)

```powershell
soroq login --hosted-surface https://soroq.dev --api https://api.soroq.dev
```

- A browser opens to the hosted login surface; complete the real sign-in.
- The browser is redirected to a `http://127.0.0.1:<port>/callback` page that says login is complete.
- The terminal prints `Logged in to https://api.soroq.dev as <you>` and a config path.
- **PASS if** login completes and the token is **never printed** to the terminal.

### G2 — whoami

```powershell
soroq whoami --api https://api.soroq.dev
```

- **PASS if** it prints your operator email + scopes (identity verified server-side).

### G3 — Credential storage + ACL (no plaintext exposure)

```powershell
$cfg = "$HOME\.soroq\config.json"
Test-Path $cfg
# Confirm the token is NOT world-readable: only the owner (and admins/SYSTEM) may have access.
(Get-Acl $cfg).Access | Format-Table IdentityReference, FileSystemRights, AccessControlType -Auto
```

- **PASS if** `config.json` exists and its ACL grants access only to your user (plus the built-in
  `SYSTEM`/`Administrators`) — no `Everyone`, `Authenticated Users`, or `Users` read grant.
- (There is no Credential Manager entry on Windows by design; the file IS the credential store.)

### G4 — Logout / revocation

```powershell
soroq logout --api https://api.soroq.dev
soroq whoami --api https://api.soroq.dev   # expect: an unauthenticated / no-credential error
```

- **PASS if** logout succeeds, the stored credential is removed, and a subsequent `whoami` fails
  because there is no longer a valid credential (server-side revoke best-effort + local removal).

### G5 — Frontend install

```powershell
soroq frontend install soroq-flutter-frontend-f74781f6-6903c161 --api https://api.soroq.dev
soroq frontend list
soroq frontend doctor
```

- **PASS if** the frontend downloads, verifies (signature + archive sha256/size), installs under
  `%USERPROFILE%\.soroq\frontends\`, and `doctor` reports it healthy.

### G6 — Toolchain install + doctor

```powershell
soroq toolchain install soroq-android-3.44.2-release-12d3315131f5 --api https://api.soroq.dev
soroq toolchain list
soroq toolchain doctor
```

- **PASS if** the toolchain downloads, verifies (signature + archive hash + `verifyEngineBundle`),
  caches under `%USERPROFILE%\.soroq\toolchains\`, and `doctor` reports availability + compatibility.

### G7 — Normal command execution

```powershell
soroq --help
soroq doctor
soroqctl   # prints usage
```

- **PASS if** help/doctor run cleanly and `soroqctl` prints its usage without crashing.

### G8 — Credential fallback (file store, owner-only)

This confirms the ONLY supported Windows credential store (config.json) behaves correctly on a
fresh/sandboxed profile.

```powershell
$sandbox = Join-Path $env:TEMP "soroq-accept-home"
Remove-Item -Recurse -Force $sandbox -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path $sandbox | Out-Null
$cfg = Join-Path $sandbox "config.json"
soroq login --hosted-surface https://soroq.dev --api https://api.soroq.dev --config $cfg
Test-Path $cfg
(Get-Acl $cfg).Access | Format-Table IdentityReference, FileSystemRights, AccessControlType -Auto
soroq whoami --api https://api.soroq.dev --config $cfg
```

- **PASS if** the token is written to the specified `--config` file, the ACL is owner-only (as in
  G3), and `whoami` against that config verifies. Then log out: `soroq logout --api https://api.soroq.dev --config $cfg`.

### G9 — Paths with spaces

```powershell
$spaced = "C:\Program Files Test\Soroq CLI\bin"
$env:SOROQ_INSTALL_ALLOW_WINDOWS = "1"
$env:SOROQ_INSTALL_DIR = $spaced
.\install.ps1
& (Join-Path $spaced "soroq.exe") version
# And a frontend install into a spaced HOME-like path:
$spacedHome = "C:\Users\Public\Soroq Work Dir"
New-Item -ItemType Directory -Force -Path $spacedHome | Out-Null
$env:SOROQ_CONFIG_HOME = $spacedHome   # if supported by your build; otherwise use --config on each command
```

- **PASS if** install into a spaced directory works and the installed `soroq.exe` runs. Prefer
  `Set-Location -LiteralPath` (not bare `cd`) whenever a working directory contains spaces.

---

## Pass/fail checklist

Record the CLI build under test (`soroq version`) and the date. Windows is called **supported** only
when all gates are PASS on a real machine against production.

| Gate | What it proves | Result |
|---|---|---|
| G1 | Production browser login (loopback), token not printed | ☐ PASS ☐ FAIL |
| G2 | whoami verifies identity server-side | ☐ PASS ☐ FAIL |
| G3 | config.json exists + owner-only ACL (no plaintext exposure) | ☐ PASS ☐ FAIL |
| G4 | logout removes + revokes the credential | ☐ PASS ☐ FAIL |
| G5 | `frontend install` verifies + installs, `doctor` healthy | ☐ PASS ☐ FAIL |
| G6 | `toolchain install` verifies + caches, `doctor` healthy | ☐ PASS ☐ FAIL |
| G7 | help/doctor/soroqctl run cleanly | ☐ PASS ☐ FAIL |
| G8 | file credential fallback works on a fresh profile, owner-only ACL | ☐ PASS ☐ FAIL |
| G9 | install + run under paths containing spaces | ☐ PASS ☐ FAIL |

**Until every row is PASS, Windows remains PENDING and is not offered as a supported platform.**
