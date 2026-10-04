# Archivist CLI installer for Windows (PowerShell 5.1 or 7).
#
#   & ([scriptblock]::Create((irm https://github.com/mosaicss/archivist/releases/latest/download/install.ps1))) -Pair CODE
#
# Installs or updates archivist.exe in %LOCALAPPDATA%\Programs\archivist,
# adds that folder to the user PATH, and with -Pair redeems the pairing code
# from the Mosaic workspace (archivist connect --pair). The download is
# checked against the release's archivist_v<ver>_SHA256SUMS before anything
# is installed. Background connect (the daemon) is not available on Windows
# yet; this installs and pairs only.
#
# Environment:
#   ARCHIVIST_INSTALL_VERSION   install this tag (e.g. v0.2.26)
#   ARCHIVIST_RELEASE_BASE_URL  release base URL (default
#                               https://github.com/mosaicss/archivist/releases)
#
# Errors throw rather than exit: this runs inside the caller's session, and
# exit would close it.

param(
    [string]$Pair = ""
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3.0
$ProgressPreference = 'SilentlyContinue'

# Stamped with the release tag by the release workflow. An unstamped copy
# installs the latest release.
$StampedVersion = '__ARCHIVIST_VERSION__'

$ReleaseBase = 'https://github.com/mosaicss/archivist/releases'
if ($env:ARCHIVIST_RELEASE_BASE_URL) { $ReleaseBase = $env:ARCHIVIST_RELEASE_BASE_URL.TrimEnd('/') }

# Windows PowerShell 5.1 may default to TLS 1.0; GitHub needs 1.2.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

function Resolve-ArchivistVersion {
    if ($env:ARCHIVIST_INSTALL_VERSION) { $v = $env:ARCHIVIST_INSTALL_VERSION }
    elseif ($StampedVersion -match '^v[0-9]') { $v = $StampedVersion }
    else {
        # The latest release redirect names the tag; no GitHub API call.
        $req = [System.Net.HttpWebRequest]::Create("$ReleaseBase/latest")
        $req.Method = 'HEAD'
        $req.AllowAutoRedirect = $false
        $resp = $req.GetResponse()
        try { $location = $resp.Headers['Location'] } finally { $resp.Close() }
        if (-not $location) { throw "archivist install: could not resolve the latest release from $ReleaseBase/latest" }
        $v = ($location -split '/')[-1]
    }
    if ($v -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+[A-Za-z0-9._-]*$') { throw "archivist install: invalid release version '$v'" }
    return $v
}

function Get-ExpectedHash([string]$SumsPath, [string]$Name) {
    foreach ($line in Get-Content -LiteralPath $SumsPath) {
        $parts = $line -split '\s+', 2
        if ($parts.Count -eq 2 -and ($parts[1].TrimStart('*') -eq $Name)) {
            if ($parts[0] -match '^[0-9a-f]{64}$') { return $parts[0] }
        }
    }
    throw "archivist install: no checksum for $Name in the release SHA256SUMS"
}

function Assert-Hash([string]$Path, [string]$SumsPath, [string]$Name) {
    $expected = Get-ExpectedHash $SumsPath $Name
    $actual = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) {
        throw "archivist install: checksum mismatch for $Name (expected $expected, got $actual). Nothing was installed."
    }
}

$version = Resolve-ArchivistVersion
$ver = $version.Substring(1)
# Only windows_amd64 is released; Windows on ARM runs it under emulation.
$archive = "archivist_v${ver}_windows_amd64.zip"
$sums = "archivist_v${ver}_SHA256SUMS"
$base = "$ReleaseBase/download/$version"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("archivist-install-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Downloading archivist $version (windows_amd64)..."
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$sums" -OutFile (Join-Path $tmp $sums)
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive)
    Assert-Hash (Join-Path $tmp $archive) (Join-Path $tmp $sums) $archive
    Write-Host 'Checksum verified.'

    $extract = Join-Path $tmp 'x'
    Expand-Archive -LiteralPath (Join-Path $tmp $archive) -DestinationPath $extract -Force
    $newExe = Join-Path $extract 'archivist.exe'
    if (-not (Test-Path -LiteralPath $newExe)) { throw 'archivist install: the archive has no archivist.exe' }
    & $newExe version | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'archivist install: the downloaded binary does not run on this machine' }

    $installDir = Join-Path $env:LOCALAPPDATA 'Programs\archivist'
    New-Item -ItemType Directory -Force -Path $installDir | Out-Null
    $exe = Join-Path $installDir 'archivist.exe'
    if (Test-Path -LiteralPath $exe) {
        # A running exe cannot be overwritten but can be renamed.
        $old = "$exe.old"
        Remove-Item -LiteralPath $old -Force -ErrorAction SilentlyContinue
        Move-Item -LiteralPath $exe -Destination $old -Force
    }
    Copy-Item -LiteralPath $newExe -Destination $exe -Force
    Write-Host "Installed $exe"

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $entries = @()
    if ($userPath) { $entries = $userPath -split ';' | Where-Object { $_ -ne '' } }
    if ($entries -notcontains $installDir) {
        [Environment]::SetEnvironmentVariable('Path', (($entries + $installDir) -join ';'), 'User')
        Write-Host "Added $installDir to your user PATH (new terminals pick it up)."
    }
    if (($env:Path -split ';') -notcontains $installDir) { $env:Path = "$env:Path;$installDir" }

    $channelDir = Join-Path $HOME '.archivist'
    New-Item -ItemType Directory -Force -Path $channelDir | Out-Null
    Set-Content -LiteralPath (Join-Path $channelDir 'install-channel') -Value 'powershell' -NoNewline

    if ($Pair) {
        & $exe connect --pair $Pair
        if ($LASTEXITCODE -ne 0) { throw "archivist install: pairing failed (exit $LASTEXITCODE)" }
        Write-Host ''
        Write-Host 'Background connect is not available on Windows yet. Your key is saved, so archivist search and read work here;'
        Write-Host 'run the macOS or Linux command from Mosaic on a Mac or Linux machine to connect your own agent.'
    } else {
        Write-Host 'Next: copy the connect command from Mosaic, or run: archivist connect --pair CODE'
    }
}
finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
