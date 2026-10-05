# Archivist CLI installer for Windows (Windows PowerShell 5.1 or PowerShell 7).
#
#   & ([scriptblock]::Create((irm https://github.com/mosaicss/archivist/releases/latest/download/install.ps1))) -Pair CODE
#
# Installs or updates archivist.exe and archivistw.exe (the console-less
# companion background connect runs) in %LOCALAPPDATA%\Programs\archivist,
# adds that folder to the user PATH, and with -Pair redeems the pairing code
# from the Mosaic workspace (archivist connect --pair), installs background
# connect (archivist connect --install: a per-user scheduled task that
# starts at every login, designed to need no admin rights; it requests no
# elevation) and reports it (archivist connect
# --status). Every download is checked against the release's
# archivist_v<ver>_SHA256SUMS before anything is installed.
#
# Options:
#   -Pair CODE    pair this computer with the code shown in Mosaic
#   -NoService    pair only; do not install background connect (one already
#                 installed is still restarted on the new binary and key)
#
# Environment:
#   ARCHIVIST_INSTALL_DIR       install directory (default
#                               %LOCALAPPDATA%\Programs\archivist)
#   ARCHIVIST_INSTALL_VERSION   install this tag (e.g. v0.2.29) instead of the
#                               version this script was released with
#   ARCHIVIST_RELEASE_BASE_URL  release base URL (default
#                               https://github.com/mosaicss/archivist/releases);
#                               for local testing against a fake release
#   ARCHIVIST_INSTALL_NO_PATH   1 leaves the user PATH unchanged (tests)
#
# archivist is not code signed yet: SmartScreen or Smart App Control may
# warn about or block it.
#
# Errors throw rather than exit: this runs inside the caller's session, and
# exit would close it.

param(
    [string]$Pair = "",
    [switch]$NoService
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

$UnsignedHint = 'archivist is not code signed yet. If Windows blocked it (SmartScreen or Smart App Control), allow it in Windows Security, or install on a computer where Smart App Control is off.'

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

# Install-Exe copies $Source to $Target. A running exe cannot be overwritten
# but can be renamed: an existing one moves to a unique .old-<id> name
# first (removed on a later install, once nothing runs it).
function Install-Exe([string]$Source, [string]$Target) {
    $old = $null
    if (Test-Path -LiteralPath $Target) {
        $old = "$Target.old-" + [guid]::NewGuid().ToString('N')
        Move-Item -LiteralPath $Target -Destination $old -Force
    }
    try {
        Copy-Item -LiteralPath $Source -Destination $Target -Force
    } catch {
        # Never leave the target without an exe: put the renamed one back.
        if ($old) {
            Remove-Item -LiteralPath $Target -Force -ErrorAction SilentlyContinue
            Move-Item -LiteralPath $old -Destination $Target -Force
        }
        throw
    }
}

function Test-ServiceInstalled {
    Test-Path -LiteralPath (Join-Path $HOME '.archivist\connect\MosaicArchivistConnect.xml')
}

$version = Resolve-ArchivistVersion
$ver = $version.Substring(1)
# Only windows_amd64 is released; Windows on ARM runs it under emulation.
$archive = "archivist_v${ver}_windows_amd64.zip"
$sums = "archivist_v${ver}_SHA256SUMS"
$skillArchive = "archivist_v${ver}_skill-bundle.tar.gz"
$base = "$ReleaseBase/download/$version"
$skillsDir = Join-Path $HOME '.claude\skills'

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("archivist-install-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Downloading archivist $version (windows_amd64)..."
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$sums" -OutFile (Join-Path $tmp $sums)
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive)
    Assert-Hash (Join-Path $tmp $archive) (Join-Path $tmp $sums) $archive

    $haveSkill = $false
    if (Test-Path -LiteralPath $skillsDir) {
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$base/$skillArchive" -OutFile (Join-Path $tmp $skillArchive)
            $haveSkill = $true
        } catch {
            Write-Host 'Note: the Claude Code skill bundle could not be downloaded; skipped.'
        }
        if ($haveSkill) { Assert-Hash (Join-Path $tmp $skillArchive) (Join-Path $tmp $sums) $skillArchive }
    }
    Write-Host 'Checksums verified.'

    $extract = Join-Path $tmp 'x'
    Expand-Archive -LiteralPath (Join-Path $tmp $archive) -DestinationPath $extract -Force
    $newExe = Join-Path $extract 'archivist.exe'
    $newExeW = Join-Path $extract 'archivistw.exe'
    if (-not (Test-Path -LiteralPath $newExe)) { throw 'archivist install: the archive has no archivist.exe' }
    if (-not (Test-Path -LiteralPath $newExeW)) { throw 'archivist install: the archive has no archivistw.exe' }
    $runs = $false
    try {
        & $newExe version | Out-Null
        $runs = ($LASTEXITCODE -eq 0)
    } catch {
        $runs = $false
    }
    if (-not $runs) {
        Write-Host $UnsignedHint
        throw 'archivist install: the downloaded binary does not run on this computer. Nothing was installed.'
    }

    $installDir = Join-Path $env:LOCALAPPDATA 'Programs\archivist'
    if ($env:ARCHIVIST_INSTALL_DIR) { $installDir = $env:ARCHIVIST_INSTALL_DIR }
    New-Item -ItemType Directory -Force -Path $installDir | Out-Null
    # Earlier installs' renamed executables (archivist.exe.old-<id>,
    # archivistw.exe.old-<id>, the legacy archivist.exe.old) and the legacy
    # archivist.exe.new of the pre-78.34 archivist update; one still running
    # stays. Nothing else is touched: ARCHIVIST_INSTALL_DIR may be a shared
    # folder.
    foreach ($pattern in 'archivist.exe.old-*', 'archivistw.exe.old-*', 'archivist.exe.old', 'archivist.exe.new') {
        Get-ChildItem -LiteralPath $installDir -Filter $pattern -File -ErrorAction SilentlyContinue |
            Where-Object { $_.Name -like $pattern } |
            ForEach-Object { Remove-Item -LiteralPath $_.FullName -Force -ErrorAction SilentlyContinue }
    }
    $exe = Join-Path $installDir 'archivist.exe'
    Install-Exe $newExe $exe
    Install-Exe $newExeW (Join-Path $installDir 'archivistw.exe')
    Write-Host "Installed $exe"

    if ($haveSkill) {
        $skillDir = Join-Path $skillsDir 'archivist'
        New-Item -ItemType Directory -Force -Path $skillDir | Out-Null
        & tar.exe -xzf (Join-Path $tmp $skillArchive) -C $skillDir
        if ($LASTEXITCODE -eq 0) { Write-Host "Installed the Claude Code skill in $skillDir" }
        else { Write-Host 'Note: the Claude Code skill bundle could not be unpacked; skipped.' }
    }

    if ($env:ARCHIVIST_INSTALL_NO_PATH -ne '1') {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $entries = @()
        if ($userPath) { $entries = @($userPath -split ';' | Where-Object { $_ -ne '' }) }
        if ($entries -notcontains $installDir) {
            [Environment]::SetEnvironmentVariable('Path', (($entries + $installDir) -join ';'), 'User')
            Write-Host "Added $installDir to your user PATH (new terminals pick it up)."
        }
    }
    if (($env:Path -split ';') -notcontains $installDir) { $env:Path = "$installDir;$env:Path" }

    $channelDir = Join-Path $HOME '.archivist'
    New-Item -ItemType Directory -Force -Path $channelDir | Out-Null
    Set-Content -LiteralPath (Join-Path $channelDir 'install-channel') -Value 'powershell' -NoNewline
}
finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

if ($Pair) {
    & $exe connect --pair $Pair
    if ($LASTEXITCODE -ne 0) { throw "archivist install: pairing failed (exit $LASTEXITCODE)" }
    # An installed task is restarted on the new binary and key even with -NoService.
    if ((-not $NoService) -or (Test-ServiceInstalled)) {
        & $exe connect --install
        if ($LASTEXITCODE -ne 0) {
            Write-Host ''
            Write-Host 'This computer is paired, but background connect could not be installed.'
            Write-Host "Fix the error above, then run: & '$exe' connect --install (no new code is needed)"
            throw "archivist install: background connect was not installed (exit $LASTEXITCODE)"
        }
    }
} elseif (Test-ServiceInstalled) {
    # An update restarts an installed task on the new binary.
    & $exe connect --install
    if ($LASTEXITCODE -ne 0) { throw "archivist install: restarting background connect failed (exit $LASTEXITCODE)" }
}

if ((-not $NoService) -and (Test-ServiceInstalled)) {
    Write-Host ''
    & $exe connect --status
    if ($LASTEXITCODE -ne 0) {
        Write-Host ''
        Write-Host 'Background connect is installed but not running. The last log line above says why.'
        throw 'archivist install: background connect is not running'
    }
}

if ((-not $Pair) -and (-not (Test-ServiceInstalled))) {
    Write-Host ''
    Write-Host 'Next: copy the connect command from Mosaic, or run: archivist connect --pair CODE'
}
