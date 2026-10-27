# Archivist CLI installer for Windows (Windows PowerShell 5.1 or PowerShell 7).
#
#   & ([scriptblock]::Create((irm https://github.com/mosaicss/archivist/releases/latest/download/install.ps1))) -Pair CODE
#
# Installs or updates archivist.exe and archivistw.exe (the console-less
# companion background connect runs) in ~\.archivist\bin, adds that folder
# to the user PATH, and with -Pair redeems the pairing code from the Mosaic
# workspace (archivist connect --pair), installs background connect
# (archivist connect --install: a per-user scheduled task that starts at
# every login, designed to need no admin rights; it requests no elevation)
# and reports it (archivist connect --status). A first install asks "Start
# Archivist when your computer starts? (recommended)" (yes is the default;
# with no console to answer it the answer is yes, said in one line); no
# keeps the foreground mode.
# Every download is checked before anything is installed: the
# release's archivist_v<ver>_SHA256SUMS must carry a valid signature
# (archivist_v<ver>_SHA256SUMS.sig, checked with .NET RSA against the
# release key below), and each archive must match its line in it.
#
# Why ~\.archivist\bin and not AppData (Story 94.14): a shell started by a
# packaged (MSIX) app, such as Claude Code inside the Claude desktop app,
# writes new files under %LOCALAPPDATA% into the app's private view, which
# Task Scheduler cannot see. The user profile outside AppData is not
# redirected. An earlier install in %LOCALAPPDATA%\Programs\archivist is
# moved here (PATH entry, task and files), unless ARCHIVIST_INSTALL_DIR is
# set. Before a pairing code is spent, whenever the task will be
# (re)installed, archivist connect --preflight starts the installed
# archivistw.exe once through a one-shot task; when Windows cannot start
# it, the install stops there and the code stays unused. The archivist
# calls run with ARCHIVIST_INSTALLER=1, removed again afterwards.
#
# Options:
#   -Pair CODE    pair this computer with the code shown in Mosaic
#   -NoService    pair only; do not install background connect (one already
#                 installed is still restarted on the new binary and key)
#
# Environment:
#   ARCHIVIST_INSTALL_DIR       install directory (default ~\.archivist\bin;
#                               when set, it is used as is and nothing is
#                               moved)
#   ARCHIVIST_INSTALL_VERSION   install this tag (v0.2.35 or later, e.g.
#                               v0.2.35) instead of the version this script
#                               was released with; earlier tags have no
#                               SHA256SUMS.sig, so use that release's own
#                               .../releases/download/<tag>/install.ps1
#   ARCHIVIST_RELEASE_BASE_URL  release base URL (default
#                               https://github.com/mosaicss/archivist/releases);
#                               for local testing against a fake release. It
#                               must be https://, or http:// to 127.0.0.1,
#                               ::1 or localhost
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

# The release signing public key (internal/release/keys/ in the archivist
# repository) as .NET RSAParameters: big endian modulus and exponent,
# base64. archivist_v<ver>_SHA256SUMS.sig must verify with it.
# BEGIN ARCHIVIST RELEASE KEY
$ReleaseModulus = '24JUQPGXSMAjvWXAA1FXl6AbLkIKZm37+4d0UFV8mnaqmA7RH0fTwljB+lGJnQH9IuTpX78vcGzklcUAtwnuj0AbGpSm0X9Tt9FdTXddhcaDqLVgczGY+yvDAgGnTGZJ8P8X3gdDprhMmRgPhkLMpky+9Q97wO+/gnlP56CzEvmy/8fWBGa2UyqL5r7WDdPiRcQky0DL1igzfn1unfRpt2AuWx+ESToLmYcOrJv8pJVtHkj+BIbLg5/fJxtyRcDyYMpFLNoZIPMst70/lhYZctj9fsiUIxqq1mxh2VW7XwqpJKUbT1m8JiYngfnXf3yVKrRE/YerMMhOZcTO8DvrMSrfxpEvSVIeCfirl/UFh3WyUbX103CgWxeSraFDrTqW9Rx/gM6PEz4dS4AiLE3eCh7+fuw//hJITDtZCXSYJwXs7KFzf0mMLholKMb6h5tOU5uidlHX0jU35MhxFNaDBWv83CrGWMUq/XESPMS7ck5qkHO8er6ASNKAtN88prwS1FVYyipdNOYypOBWrrO3i1ndsweLQUbZWTAJQIb33XkMmxNKRRZwuby8TubCkx3SJplUWHJBpkmdJMgBxxHkV3qHbZdexrqVN1QudVPQaLhPC1wrrlA48fqY4snH8swyq1RlOunPUfotZhp51SiU79cGp1UbOABChqsxhL4DthM='
$ReleaseExponent = 'AQAB'
# END ARCHIVIST RELEASE KEY

$ReleaseBase = 'https://github.com/mosaicss/archivist/releases'
if ($env:ARCHIVIST_RELEASE_BASE_URL) { $ReleaseBase = $env:ARCHIVIST_RELEASE_BASE_URL.TrimEnd('/') }

# The release base must be https:// with a host and no user info, or
# http:// to a loopback host (127.0.0.1, ::1, localhost). Checked before any
# download.
function Assert-ReleaseBase([string]$Base) {
    $u = $null
    if (-not [Uri]::TryCreate($Base, [UriKind]::Absolute, [ref]$u) -or -not $u.Host -or $u.UserInfo) {
        throw "archivist install: ARCHIVIST_RELEASE_BASE_URL must be an https:// URL with a host, without user info"
    }
    if ($u.Scheme -eq 'https') { return }
    if ($u.Scheme -eq 'http' -and @('127.0.0.1', 'localhost', '[::1]', '::1') -contains $u.Host) { return }
    throw 'archivist install: ARCHIVIST_RELEASE_BASE_URL must start with https:// (http:// is only for 127.0.0.1, ::1 or localhost)'
}
Assert-ReleaseBase $ReleaseBase

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

# Assert-Signature: the SHA256SUMS file must carry a valid RSA PKCS#1 v1.5
# SHA-256 signature by the release key. Works on .NET Framework 4.6+
# (Windows PowerShell 5.1) and .NET (PowerShell 7).
function Assert-Signature([string]$SumsPath, [string]$SigPath, [string]$SigName) {
    $ok = $false
    try {
        $params = New-Object System.Security.Cryptography.RSAParameters
        $params.Modulus = [Convert]::FromBase64String($ReleaseModulus)
        $params.Exponent = [Convert]::FromBase64String($ReleaseExponent)
        $data = [System.IO.File]::ReadAllBytes($SumsPath)
        $sig = [System.IO.File]::ReadAllBytes($SigPath)
        if ($sig.Length -eq $params.Modulus.Length) {
            $rsa = [System.Security.Cryptography.RSA]::Create()
            try {
                $rsa.ImportParameters($params)
                $ok = $rsa.VerifyData($data, $sig, [System.Security.Cryptography.HashAlgorithmName]::SHA256,
                    [System.Security.Cryptography.RSASignaturePadding]::Pkcs1)
            } finally { $rsa.Dispose() }
        }
    } catch {
        $ok = $false
    }
    if ($ok -ne $true) {
        throw "archivist install: the release checksums could not be verified ($SigName). Nothing was installed."
    }
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

# Swap-PathEntry returns the ;-separated $PathValue with the $Old entry
# replaced by $New in place ($New kept once; added at the end when neither
# was there).
function Swap-PathEntry([string]$PathValue, [string]$Old, [string]$New) {
    $out = New-Object System.Collections.Generic.List[string]
    foreach ($e in @($PathValue -split ';')) {
        if ($e -eq '') { continue }
        if ($e.TrimEnd('\') -eq $Old.TrimEnd('\')) { $e = $New }
        if ($e -eq $New -and $out.Contains($New)) { continue }
        $out.Add($e)
    }
    if (-not $out.Contains($New)) { $out.Add($New) }
    return ($out -join ';')
}

# Remove-LegacyInstall removes archivist from the pre-94.14 location once the
# new one works: each exe is removed, or renamed to a unique .old-<id> while
# it still runs (removed by a later install), stale renamed copies go, and
# the folder goes when nothing else is left in it.
function Remove-LegacyInstall([string]$Dir) {
    foreach ($pattern in 'archivist.exe.old-*', 'archivistw.exe.old-*', 'archivist.exe.old', 'archivist.exe.new') {
        Get-ChildItem -LiteralPath $Dir -Filter $pattern -File -ErrorAction SilentlyContinue |
            Where-Object { $_.Name -like $pattern } |
            ForEach-Object { Remove-Item -LiteralPath $_.FullName -Force -ErrorAction SilentlyContinue }
    }
    foreach ($name in 'archivist.exe', 'archivistw.exe') {
        $p = Join-Path $Dir $name
        if (-not (Test-Path -LiteralPath $p)) { continue }
        try {
            Remove-Item -LiteralPath $p -Force -ErrorAction Stop
        } catch {
            Move-Item -LiteralPath $p -Destination ("$p.old-" + [guid]::NewGuid().ToString('N')) -Force -ErrorAction SilentlyContinue
        }
    }
    if (@(Get-ChildItem -LiteralPath $Dir -Force -ErrorAction SilentlyContinue).Count -eq 0) {
        Remove-Item -LiteralPath $Dir -Force -ErrorAction SilentlyContinue
    }
}

$version = Resolve-ArchivistVersion
$ver = $version.Substring(1)
# Only windows_amd64 is released; Windows on ARM runs it under emulation.
$archive = "archivist_v${ver}_windows_amd64.zip"
$sums = "archivist_v${ver}_SHA256SUMS"
$sumsSig = "$sums.sig"
$skillArchive = "archivist_v${ver}_skill-bundle.tar.gz"
$base = "$ReleaseBase/download/$version"
$skillsDir = Join-Path $HOME '.claude\skills'

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("archivist-install-" + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Downloading archivist $version (windows_amd64)..."
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$sums" -OutFile (Join-Path $tmp $sums)
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$sumsSig" -OutFile (Join-Path $tmp $sumsSig)
    Assert-Signature (Join-Path $tmp $sums) (Join-Path $tmp $sumsSig) $sumsSig
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
    Write-Host 'Download verified.'

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

    # Story 94.14: outside AppData, which a packaged app's shell redirects.
    $installDir = Join-Path $HOME '.archivist\bin'
    $legacyDir = $null
    if ($env:ARCHIVIST_INSTALL_DIR) {
        $installDir = $env:ARCHIVIST_INSTALL_DIR
    } elseif ($env:LOCALAPPDATA) {
        $candidate = Join-Path $env:LOCALAPPDATA 'Programs\archivist'
        # The exes, or copies an earlier move renamed aside while they ran.
        $leftovers = @(Get-ChildItem -LiteralPath $candidate -File -ErrorAction SilentlyContinue |
            Where-Object { @('archivist.exe', 'archivistw.exe') -contains $_.Name -or $_.Name -like 'archivist.exe.old-*' -or $_.Name -like 'archivistw.exe.old-*' })
        if ($leftovers.Count -gt 0) {
            $legacyDir = $candidate
        }
    }
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
        if ($legacyDir) {
            # The legacy entry is replaced by the new one.
            $newPath = Swap-PathEntry $userPath $legacyDir $installDir
            if ($newPath -ne $userPath) { [Environment]::SetEnvironmentVariable('Path', $newPath, 'User') }
        } elseif ($entries -notcontains $installDir) {
            [Environment]::SetEnvironmentVariable('Path', (($entries + $installDir) -join ';'), 'User')
            Write-Host "Added $installDir to your user PATH (new terminals pick it up)."
        }
    }
    if ($legacyDir) { $env:Path = Swap-PathEntry $env:Path $legacyDir $installDir }
    if (($env:Path -split ';') -notcontains $installDir) { $env:Path = "$installDir;$env:Path" }

    $channelDir = Join-Path $HOME '.archivist'
    New-Item -ItemType Directory -Force -Path $channelDir | Out-Null
    Set-Content -LiteralPath (Join-Path $channelDir 'install-channel') -Value 'powershell' -NoNewline
}
finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

# Story 94.14: the archivist calls below know they run under the installer
# (an environment variable, which binaries older than it ignore); the
# caller's session gets its earlier value back.
$prevInstaller = $env:ARCHIVIST_INSTALLER
$env:ARCHIVIST_INSTALLER = '1'
try {
    # Before a pairing code is spent, whenever the task will be (re)installed:
    # can Task Scheduler start the archivistw.exe just installed? A packaged
    # app's shell installs into a view Windows cannot see. Exit 2 is a binary
    # older than --preflight: no check, and its "unknown flag" line is not
    # shown. The output is captured with stderr (under 'Continue', since
    # Windows PowerShell 5.1 turns redirected native stderr into errors) and
    # shown for every other exit.
    $willInstallTask = if ($Pair) { (-not $NoService) -or (Test-ServiceInstalled) } else { Test-ServiceInstalled }
    if ($willInstallTask) {
        $eap = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        try {
            $preflightOut = @(& $exe connect --preflight 2>&1 | ForEach-Object { "$_" })
            $preflight = $LASTEXITCODE
        } finally { $ErrorActionPreference = $eap }
        if ($preflight -ne 2) { foreach ($l in $preflightOut) { Write-Host $l } }
        if ($preflight -ne 0 -and $preflight -ne 2) {
            if ($Pair) {
                Write-Host 'Your pairing code was not used.'
                throw "archivist install: stopped before pairing (exit $preflight)"
            }
            throw "archivist install: stopped before restarting background connect (exit $preflight)"
        }
    }

    if ($Pair) {
        & $exe connect --pair $Pair
        if ($LASTEXITCODE -ne 0) { throw "archivist install: pairing failed (exit $LASTEXITCODE)" }
        # An installed task is restarted on the new binary and key even with -NoService.
        if ((-not $NoService) -or (Test-ServiceInstalled)) {
            # Story 94.6: a first install asks whether to start at login (--ask);
            # an installed task is reinstalled without the question.
            if (Test-ServiceInstalled) {
                & $exe connect --install
            } else {
                & $exe connect --install --ask
            }
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
            Write-Host 'Background connect is installed but not running; the Service line above says why.'
            if ($Pair) {
                Write-Host 'This computer is paired. Once that is fixed, run this again without -Pair (no new code is needed):'
                Write-Host "  & ([scriptblock]::Create((irm $ReleaseBase/latest/download/install.ps1)))"
            } else {
                Write-Host 'Once that is fixed, run this command again.'
            }
            throw 'archivist install: background connect is not running'
        }
    }
} finally {
    if ($null -eq $prevInstaller) { Remove-Item Env:ARCHIVIST_INSTALLER -ErrorAction SilentlyContinue }
    else { $env:ARCHIVIST_INSTALLER = $prevInstaller }
}

# Story 94.14: the pre-94.14 install goes only now, once the new one is in
# place and any task runs from it; an earlier failure (thrown above) keeps it.
if ($legacyDir) {
    Remove-LegacyInstall $legacyDir
    Write-Host "Moved archivist from $legacyDir to $installDir. Terminals and apps that were already open find it there after you restart them."
}

if ((-not $Pair) -and (-not (Test-ServiceInstalled))) {
    Write-Host ''
    Write-Host 'Next: copy the connect command from Mosaic, or run: archivist connect --pair CODE'
}
