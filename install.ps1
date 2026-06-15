param(
    [string]$Repo = $(if ($env:SOROQ_INSTALL_REPO) { $env:SOROQ_INSTALL_REPO } else { "soroq/install" }),
    [string]$Version = $(if ($env:SOROQ_INSTALL_VERSION) { $env:SOROQ_INSTALL_VERSION } else { "latest" }),
    [string]$InstallDir = $(if ($env:SOROQ_INSTALL_DIR) { $env:SOROQ_INSTALL_DIR } else { Join-Path $HOME ".soroq\bin" })
)

$ErrorActionPreference = "Stop"
$BinaryName = "soroq.exe"
$Token = if ($env:SOROQ_GITHUB_TOKEN) { $env:SOROQ_GITHUB_TOKEN } else { $env:GITHUB_TOKEN }

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
    Write-Host "  - Pin a version: `$env:SOROQ_INSTALL_VERSION='v0.1.0'; .\install.ps1"
    Write-Host "  - Change install path: `$env:SOROQ_INSTALL_DIR='C:\Tools\soroq'; .\install.ps1"
    Write-Host "  - Private repo? set SOROQ_GITHUB_TOKEN to a GitHub token that can read $Repo"
    exit 1
}

function Detect-Arch {
    switch ($env:PROCESSOR_ARCHITECTURE.ToLowerInvariant()) {
        "amd64" { return "amd64" }
        "arm64" { return "arm64" }
        default { Fail "Unsupported CPU architecture: $env:PROCESSOR_ARCHITECTURE. Soroq CLI releases support amd64 and arm64." }
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
if ($Version -eq "latest") {
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
    Say "$(Paint Blue 'i') Install:    $(Paint Bold (Join-Path $InstallDir $BinaryName))"
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
    $ExtractedBinary = Join-Path $TmpDir $BinaryName
    if (-not (Test-Path $ExtractedBinary)) {
        Fail "Archive did not contain $BinaryName."
    }
    Success "Archive unpacked"

    Step "Installing binary"
    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Copy-Item -Path $ExtractedBinary -Destination (Join-Path $InstallDir $BinaryName) -Force
    Success "Installed $(Paint Bold (Join-Path $InstallDir $BinaryName))"

    Step "Checking installation"
    & (Join-Path $InstallDir $BinaryName) --help *> $null
    if ($LASTEXITCODE -ne 0) {
        Fail "Installed binary did not run successfully."
    }
    Success "Soroq CLI is ready"

    Say ""
    Say "$(Paint Bold (Paint Green 'Installation complete.'))"
    $PathEntries = $env:PATH -split [System.IO.Path]::PathSeparator
    if ($PathEntries -contains $InstallDir) {
        Say "Run $(Paint Bold 'soroq --help') to get started."
    } else {
        Say "$(Paint Yellow 'WARN') $InstallDir is not currently on PATH."
        Say "Add it for the current user:"
        Say ""
        Say "  [Environment]::SetEnvironmentVariable('Path', `$env:Path + ';$InstallDir', 'User')"
        Say ""
        Say "Then open a new PowerShell window and run:"
        Say "  soroq --help"
    }
    Say ""
} finally {
    Remove-Item -Recurse -Force $TmpDir -ErrorAction SilentlyContinue
}
