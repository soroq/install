param(
    [string]$Repo = $(if ($env:SOROQ_INSTALL_REPO) { $env:SOROQ_INSTALL_REPO } else { "soroq/install" }),
    [string]$Version = $(if ($env:SOROQ_INSTALL_VERSION) { $env:SOROQ_INSTALL_VERSION } else { "latest" }),
    [string]$InstallDir = $(if ($env:SOROQ_INSTALL_DIR) { $env:SOROQ_INSTALL_DIR } else { Join-Path $HOME ".soroq\bin" })
)

$ErrorActionPreference = "Stop"
$BinaryNames = @("soroq.exe", "soroqctl.exe")
$Token = if ($env:SOROQ_GITHUB_TOKEN) { $env:SOROQ_GITHUB_TOKEN } else { $env:GITHUB_TOKEN }

# Windows is pending for the Soroq hard-OTA beta. A Windows CLI candidate is published for
# explicit testers, but Windows-host frontend/toolchain artifacts have not passed acceptance.
# Refuse by default so a runnable CLI is not mistaken for complete Windows build support.
if (-not $env:SOROQ_INSTALL_ALLOW_WINDOWS) {
    Write-Host ""
    Write-Host "Soroq CLI: a native Windows installer is not published yet (Windows is pending for the hard-OTA beta)."
    Write-Host "Supported today: macOS and Linux. Track Windows status: https://github.com/soroq/install"
    exit 1
}

function Supports-Color {
    if ($env:NO_COLOR) { return $false }
    return $Host.Name -ne "Default Host" -or $env:WT_SESSION -or $env:TERM_PROGRAM
}

$UseColor = Supports-Color

function Paint([string]$Color, [string]$Text) {
    if (-not $UseColor) { return $Text }
    $codes = @{
        Red = "31"
        Green = "32"
        Yellow = "33"
        Blue = "34"
        Bold = "1"
        Dim = "2"
    }
    return "$([char]27)[$($codes[$Color])m$Text$([char]27)[0m"
}

function Say([string]$Text = "") {
    Write-Host $Text
}

function Step([string]$Text) {
    Say "$(Paint Blue '>') $Text"
}

function Success([string]$Text) {
    Say "$(Paint Green 'OK') $Text"
}

function Fail([string]$Text) {
    Write-Host ""
    Write-Host "$(Paint Red 'ERROR') $Text" -ForegroundColor Red
    Write-Host ""
    Write-Host "$(Paint Bold 'What to try next')"
    Write-Host "  - Run PowerShell as a normal user unless installing to a protected directory."
    Write-Host "  - Pin a version: `$env:SOROQ_INSTALL_VERSION='<version>'; .\install.ps1   (e.g. v0.2.4)"
    Write-Host "  - Change install path: `$env:SOROQ_INSTALL_DIR='C:\Tools\soroq'; .\install.ps1"
    Write-Host "  - Private repo? set SOROQ_GITHUB_TOKEN to a GitHub token that can read $Repo"
    exit 1
}

function Detect-Arch {
    switch ($env:PROCESSOR_ARCHITECTURE.ToLowerInvariant()) {
        "amd64" { return "amd64" }
        "arm64" { Fail "Windows arm64 is not published yet. Use an x64 PowerShell under Windows emulation, or wait for a native arm64 release." }
        default { Fail "Unsupported CPU architecture: $env:PROCESSOR_ARCHITECTURE. The Windows CLI candidate currently supports amd64." }
    }
}

function PathContains([string]$PathValue, [string]$Entry) {
    if (-not $PathValue) { return $false }
    $needle = $Entry.Trim().TrimEnd('\')
    foreach ($part in ($PathValue -split [System.IO.Path]::PathSeparator)) {
        if ($part.Trim().TrimEnd('\').Equals($needle, [System.StringComparison]::OrdinalIgnoreCase)) {
            return $true
        }
    }
    return $false
}

function Ensure-UserPath([string]$Entry) {
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (-not (PathContains $userPath $Entry)) {
        $updated = if ([string]::IsNullOrWhiteSpace($userPath)) {
            $Entry
        } else {
            $userPath.TrimEnd(';') + ';' + $Entry
        }
        [Environment]::SetEnvironmentVariable("Path", $updated, "User")
        Success "Added $Entry to the current-user PATH"
    } else {
        Success "Current-user PATH already contains $Entry"
    }

    # Make the command available to the remainder of this PowerShell session too.
    if (-not (PathContains $env:PATH $Entry)) {
        $env:PATH = $Entry + [System.IO.Path]::PathSeparator + $env:PATH
    }
}

function Install-BinariesAtomically([string]$ExtractDir, [string]$DestinationDir) {
    New-Item -ItemType Directory -Force -Path $DestinationDir | Out-Null
    $id = [System.Guid]::NewGuid().ToString("N")
    $staged = @{}
    $backups = @{}
    $installed = New-Object System.Collections.Generic.List[string]

    try {
        foreach ($name in $BinaryNames) {
            $source = Join-Path $ExtractDir $name
            if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
                throw "Archive did not contain $name."
            }
            $stage = Join-Path $DestinationDir (".$name.$id.new")
            Copy-Item -LiteralPath $source -Destination $stage -Force
            $staged[$name] = $stage
        }

        # Verify staged bytes before touching a working installation.
        & $staged["soroq.exe"] version *> $null
        if ($LASTEXITCODE -ne 0) { throw "Staged soroq.exe did not run successfully." }
        & $staged["soroqctl.exe"] --help *> $null
        if ($LASTEXITCODE -ne 0) { throw "Staged soroqctl.exe did not run successfully." }

        foreach ($name in $BinaryNames) {
            $destination = Join-Path $DestinationDir $name
            if (Test-Path -LiteralPath $destination) {
                $backup = Join-Path $DestinationDir (".$name.$id.bak")
                Move-Item -LiteralPath $destination -Destination $backup -Force
                $backups[$name] = $backup
            }
        }

        foreach ($name in $BinaryNames) {
            $destination = Join-Path $DestinationDir $name
            Move-Item -LiteralPath $staged[$name] -Destination $destination -Force
            $installed.Add($name)
        }

        & (Join-Path $DestinationDir "soroq.exe") version *> $null
        if ($LASTEXITCODE -ne 0) { throw "Installed soroq.exe did not run successfully." }
        & (Join-Path $DestinationDir "soroqctl.exe") --help *> $null
        if ($LASTEXITCODE -ne 0) { throw "Installed soroqctl.exe did not run successfully." }

        foreach ($backup in $backups.Values) {
            Remove-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue
        }
    } catch {
        foreach ($name in $installed) {
            Remove-Item -LiteralPath (Join-Path $DestinationDir $name) -Force -ErrorAction SilentlyContinue
        }
        foreach ($name in $backups.Keys) {
            Move-Item -LiteralPath $backups[$name] -Destination (Join-Path $DestinationDir $name) -Force -ErrorAction SilentlyContinue
        }
        foreach ($stage in $staged.Values) {
            Remove-Item -LiteralPath $stage -Force -ErrorAction SilentlyContinue
        }
        throw
    }
}

function Download-File([string]$Url, [string]$Output, [string]$Label) {
    $headers = @{}
    if ($Token) {
        $headers["Authorization"] = "Bearer $Token"
        $headers["X-GitHub-Api-Version"] = "2022-11-28"
    }
    try {
        Invoke-WebRequest -Uri $Url -OutFile $Output -Headers $headers -UseBasicParsing
    } catch {
        Fail "Could not download $Label from $Url. $($_.Exception.Message)"
    }
}

Say ""
Say "$(Paint Bold (Paint Blue 'Soroq CLI Installer'))"
Say "$(Paint Dim 'Fast Android OTA release tooling, installed globally.')"
Say ""

$Arch = Detect-Arch
$Asset = "soroq_windows_$Arch.zip"
# SOROQ_INSTALL_BASE_URL overrides the release download base (scheme+host+path). It exists for
# offline/CI verification against a local file:// or http://127.0.0.1 server; unset in normal use,
# so default behavior (public GitHub Releases) is unchanged.
if ($env:SOROQ_INSTALL_BASE_URL) {
    $BaseUrl = $env:SOROQ_INSTALL_BASE_URL.TrimEnd("/")
} elseif ($Version -eq "latest") {
    $BaseUrl = "https://github.com/$Repo/releases/latest/download"
} else {
    $BaseUrl = "https://github.com/$Repo/releases/download/$Version"
}

$TmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ("soroq-install-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $TmpDir | Out-Null

try {
    $Archive = Join-Path $TmpDir $Asset
    $Checksums = Join-Path $TmpDir "checksums.txt"

    Say "$(Paint Blue 'i') Repository: $(Paint Bold $Repo)"
    Say "$(Paint Blue 'i') Version:    $(Paint Bold $Version)"
    Say "$(Paint Blue 'i') Target:     $(Paint Bold "windows/$Arch")"
    Say "$(Paint Blue 'i') Install:    $(Paint Bold $InstallDir)"
    if ($Token) {
        Say "$(Paint Blue 'i') Auth:       $(Paint Bold 'GitHub token detected')"
    } else {
        Say "$(Paint Blue 'i') Auth:       public GitHub release"
    }
    Say ""

    Step "Downloading $Asset"
    Download-File "$BaseUrl/$Asset" $Archive $Asset
    Success "Downloaded CLI archive"

    Step "Downloading checksums"
    Download-File "$BaseUrl/checksums.txt" $Checksums "checksums.txt"
    Success "Downloaded checksum manifest"

    Step "Verifying checksum"
    $ExpectedLine = Get-Content $Checksums | Where-Object { $_ -match "  $([regex]::Escape($Asset))$" } | Select-Object -First 1
    if (-not $ExpectedLine) {
        Fail "checksums.txt does not contain an entry for $Asset."
    }
    $Expected = ($ExpectedLine -split "\s+")[0]
    $Actual = (Get-FileHash -Algorithm SHA256 $Archive).Hash.ToLowerInvariant()
    if ($Expected.ToLowerInvariant() -ne $Actual) {
        Write-Host "Expected: $Expected"
        Write-Host "Actual:   $Actual"
        Fail "Checksum mismatch for $Asset. The download may be corrupted or the release asset changed."
    }
    Success "Checksum verified"

    Step "Unpacking CLI"
    Expand-Archive -Path $Archive -DestinationPath $TmpDir -Force
    foreach ($name in $BinaryNames) {
        if (-not (Test-Path -LiteralPath (Join-Path $TmpDir $name) -PathType Leaf)) {
            Fail "Archive did not contain $name."
        }
    }
    Success "Archive unpacked"

    Step "Installing soroq + soroqctl atomically"
    try {
        Install-BinariesAtomically $TmpDir $InstallDir
    } catch {
        Fail "Could not install both binaries safely. The previous installation was restored. $($_.Exception.Message)"
    }
    Success "Installed $(Paint Bold (Join-Path $InstallDir 'soroq.exe'))"
    Success "Installed $(Paint Bold (Join-Path $InstallDir 'soroqctl.exe'))"

    Step "Checking installation"
    & (Join-Path $InstallDir "soroq.exe") version *> $null
    if ($LASTEXITCODE -ne 0) { Fail "Installed soroq.exe did not run successfully." }
    & (Join-Path $InstallDir "soroqctl.exe") --help *> $null
    if ($LASTEXITCODE -ne 0) { Fail "Installed soroqctl.exe did not run successfully." }
    Success "Both Soroq CLI binaries are ready"

    Step "Configuring PATH"
    try {
        Ensure-UserPath $InstallDir
    } catch {
        Fail "Binaries were installed, but the current-user PATH could not be updated safely. $($_.Exception.Message)"
    }

    Say ""
    Say "$(Paint Bold (Paint Green 'Installation complete.'))"
    Say "Open a new PowerShell window, then run:"
    Say "  soroq version"
    Say "  soroq --help"
    Say ""
} finally {
    Remove-Item -Recurse -Force $TmpDir -ErrorAction SilentlyContinue
}
