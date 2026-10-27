# Replays scripts/install.ps1 against a local fake release (Windows only).
#
#   .\scripts\test-install.ps1
#
# Builds archivist.exe and archivistw.exe from this checkout, lays out fake
# releases <base>/download/<tag>/ (zip, skill bundle, SHA256SUMS and its
# signature by a throwaway test key made with .NET), serves them on
# 127.0.0.1 with fake pairing routes (scripts/testdata/winreplay/server),
# and runs a copy of install.ps1 whose release key block holds the test key
# in a child PowerShell (the one running this script) under a throwaway
# USERPROFILE, with a stub schtasks.exe (scripts/testdata/winreplay/schtasks)
# first on PATH. Pairing takes the browser confirmation
# (ARCHIVIST_PAIR_CONFIRM=browser, with BROWSER the fake server binary, which
# confirms the pending pairing), so no console prompt waits. Nothing touches
# the real profile, the user PATH or Task Scheduler.
#
# Asserts: install + pair + task created and reported running; an update
# without -Pair recreates the task and calls no redeem; an update while the
# installed archivist.exe runs renames it aside; -NoService pairs without a
# task; a failed task registration after pairing prints the resume step and
# throws; an archive that does not match its checksum throws with nothing
# installed; the skill bundle
# is installed when ~\.claude\skills exists; the panel's scriptblock
# one-liner (install.ps1 served as application/octet-stream) works like the
# file form. Each of these throws with nothing installed: checksums without
# a valid signature; a release
# without the signature; one signed by another key; the unmodified
# install.ps1 (release key) against the test signed release; a plain http://
# release URL that is not loopback (before any download).
#
# Story 94.6: connect keeps its key in ~\.archivist\credentials, never in
# Credential Manager (real on Windows), so after pairing and install the file
# holds the key and no "archivist:<path>" credential exists; the
# first install takes the start question's default (no console to ask), and
# connect --uninstall revokes the key (DELETE /account/cli-tokens/current),
# deletes the task and removes the key, the task XML and ~\.archivist\connect.
# Credential Manager items the replay created are deleted at the end.
#
# Story 94.14 (the stub schtasks.exe records each task's last result): a
# probe launch failure with -Pair stops before pairing (no redeem, no
# credentials file, the packaged shell message and "Your pairing code was
# not used."); a task that does not start names Task Scheduler's result and
# the rerun without -Pair, never a log line; an install without
# ARCHIVIST_INSTALL_DIR goes to ~\.archivist\bin and moves a legacy
# %LOCALAPPDATA%\Programs\archivist install with its task; a re-pair under
# the installer prints no restart hint and leaves no ARCHIVIST_INSTALLER in
# the caller's session; an archivist.exe older than connect --preflight
# (scripts/testdata/winreplay/oldarchivist, released as v9.9.3: it answers
# --preflight with "unknown flag" and exit 2 and runs the real exe for the
# rest) is asked, the check is skipped without showing its error, and the
# install pairs; a move whose task then does not start keeps the legacy
# exes and prints no move line (13b). Before anything else, on any
# platform, Swap-PathEntry is loaded from install.ps1 and checked.

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3.0
$ProgressPreference = 'SilentlyContinue'

# --- 0. Swap-PathEntry (Story 94.14), taken from install.ps1 by its AST -------
# The replay cases leave the user PATH alone (ARCHIVIST_INSTALL_NO_PATH=1), so
# the PATH swap of a legacy move is checked here, on any platform.
$swapErrors = $null
$swapAst = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'install.ps1'), [ref]$null, [ref]$swapErrors)
$swapFn = @($swapAst.FindAll({ param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $n.Name -eq 'Swap-PathEntry' }, $true))
if ($swapFn.Count -ne 1) { throw 'FAIL: install.ps1 has no single Swap-PathEntry' }
. ([scriptblock]::Create($swapFn[0].Extent.Text))
$legacyEntry = 'C:\Users\a\AppData\Local\Programs\archivist'
$newEntry = 'C:\Users\a\.archivist\bin'
foreach ($c in @(
        @{ In = "C:\x;$legacyEntry;C:\y"; Want = "C:\x;$newEntry;C:\y"; What = 'the legacy entry is replaced in place' },
        @{ In = "C:\x;$legacyEntry\;C:\y"; Want = "C:\x;$newEntry;C:\y"; What = 'a trailing backslash' },
        @{ In = "C:\x;$($legacyEntry.ToUpperInvariant());C:\y"; Want = "C:\x;$newEntry;C:\y"; What = 'another case' },
        @{ In = "$newEntry;C:\x;$legacyEntry"; Want = "$newEntry;C:\x"; What = 'the new entry already there is kept once' },
        @{ In = 'C:\x;C:\y'; Want = "C:\x;C:\y;$newEntry"; What = 'no legacy entry: the new one is appended' },
        @{ In = ''; Want = $newEntry; What = 'an empty PATH' })) {
    $got = Swap-PathEntry $c.In $legacyEntry $newEntry
    if ($got -ne $c.Want) { throw "FAIL: Swap-PathEntry, $($c.What): got '$got', want '$($c.Want)'" }
}
Write-Host 'ok: Swap-PathEntry swaps the legacy PATH entry for the new one'

if ($env:OS -ne 'Windows_NT') { Write-Host 'test-install.ps1 runs on Windows only; skipped'; return }

$Root = Split-Path -Parent $PSScriptRoot
$Work = Join-Path ([System.IO.Path]::GetTempPath()) ('archivist-install-test-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $Work | Out-Null
$PSExe = (Get-Process -Id $PID).Path
$server = $null
$running = $null

function Fail([string]$Message) { throw "FAIL: $Message" }
function Pass([string]$Message) { Write-Host "ok: $Message" }

# Set below, read by the functions (script scope).
$Base = ''
$stubs = ''
$callLog = Join-Path $Work 'calls.log'
$redeemLog = Join-Path $Work 'redeem.log'

# The installer under test: install.ps1 with the test key (set below).
$Installer = ''

# Invoke-Install runs the installer under test in a child PowerShell with a
# clean profile environment; returns @{ Out; Code }.
# With -OneLiner it runs that command (the panel's scriptblock form)
# instead of the installer as a file; -Script runs another installer file.
function Invoke-Install([string]$UserHome, [string]$Version, [string[]]$InstallArgs, [hashtable]$Extra = @{}, [string]$OneLiner = '', [string]$Script = '') {
    if (-not $Script) { $Script = $Installer }
    New-Item -ItemType Directory -Force -Path $UserHome | Out-Null
    $vars = @{
        USERPROFILE = $UserHome; HOME = $UserHome; LOCALAPPDATA = (Join-Path $UserHome 'AppData\Local')
        ARCHIVIST_INSTALL_DIR = (Join-Path $UserHome 'bin'); ARCHIVIST_INSTALL_NO_PATH = '1'
        ARCHIVIST_RELEASE_BASE_URL = $Base; ARCHIVIST_INSTALL_VERSION = $Version; ARCHIVIST_BASE_URL = $Base
        ARCHIVIST_TOKEN = $null; STUB_LOG = $callLog; STUB_STATE = (Join-Path $UserHome 'task-registered')
        STUB_FAIL_CREATE = $null; STUB_PROBE_RESULT = $null; STUB_RUN_FAIL = $null; ARCHIVIST_INSTALLER = $null
        Path = "$stubs;$env:Path"
        ARCHIVIST_PAIR_CONFIRM = 'browser'; BROWSER = (Join-Path $Work 'server.exe')
    }
    foreach ($k in $Extra.Keys) { $vars[$k] = $Extra[$k] }
    $saved = @{}
    foreach ($k in $vars.Keys) {
        $saved[$k] = [Environment]::GetEnvironmentVariable($k, 'Process')
        [Environment]::SetEnvironmentVariable($k, $vars[$k], 'Process')
    }
    # Native stderr through 2>&1 must not stop this script (PowerShell 5.1).
    $eap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        if ($OneLiner) {
            $out = & $PSExe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command $OneLiner 2>&1 | Out-String
        } else {
            $out = & $PSExe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $Script @InstallArgs 2>&1 | Out-String
        }
        return @{ Out = $out; Code = $LASTEXITCODE }
    } finally {
        $ErrorActionPreference = $eap
        foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k], 'Process') }
    }
}

# Reset-CallLog starts a fresh call log for one case, keeping the old lines
# in calls.log.all so Stop-StandIns still finds every stand-in daemon.
function Reset-CallLog {
    if (Test-Path -LiteralPath $callLog) {
        Get-Content -LiteralPath $callLog | Add-Content -LiteralPath "$callLog.all"
    }
    Set-Content -LiteralPath $callLog -Value $null
}

# Assert-Refused: a refused install exited non-zero, said Message, and left
# nothing installed.
function Assert-Refused([string]$Name, [string]$UserHome, $Result, [string]$Message) {
    if ($Result.Code -eq 0) { Fail "$Name exited 0:`n$($Result.Out)" }
    if ($Result.Out -notmatch [regex]::Escape($Message)) { Fail "${Name}: no '$Message' message:`n$($Result.Out)" }
    if (Test-Path -LiteralPath (Join-Path $UserHome 'bin')) { Fail "$Name left the install directory" }
    if (Test-Path -LiteralPath (Join-Path $UserHome '.archivist')) { Fail "$Name left ~\.archivist" }
    if (Test-Path -LiteralPath (Join-Path $UserHome '.claude\skills\archivist')) { Fail "$Name installed the skill" }
}

# The saved key as the CLI could read it (Story 94.6): the credentials file
# when it exists, else the Credential Manager item go-keyring writes (generic
# credential "archivist:<absolute credentials path>", UTF-8 blob). Connect
# never writes that item; Assert-KeyInFile checks so.
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
public static class ArchivistReplayCred {
    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    struct CREDENTIAL {
        public uint Flags; public uint Type; public string TargetName; public string Comment;
        public uint LastWrittenLow; public uint LastWrittenHigh; public uint CredentialBlobSize;
        public IntPtr CredentialBlob; public uint Persist; public uint AttributeCount; public IntPtr Attributes;
        public string TargetAlias; public string UserName;
    }
    [DllImport("advapi32.dll", EntryPoint = "CredReadW", CharSet = CharSet.Unicode, SetLastError = true)]
    static extern bool CredRead(string target, uint type, uint flags, out IntPtr credential);
    [DllImport("advapi32.dll", EntryPoint = "CredDeleteW", CharSet = CharSet.Unicode, SetLastError = true)]
    static extern bool CredDelete(string target, uint type, uint flags);
    [DllImport("advapi32.dll")]
    static extern void CredFree(IntPtr credential);
    public static string Read(string target) {
        IntPtr p;
        if (!CredRead(target, 1, 0, out p)) { return null; }
        try {
            CREDENTIAL c = (CREDENTIAL)Marshal.PtrToStructure(p, typeof(CREDENTIAL));
            byte[] b = new byte[c.CredentialBlobSize];
            Marshal.Copy(c.CredentialBlob, b, 0, b.Length);
            return System.Text.Encoding.UTF8.GetString(b);
        } finally { CredFree(p); }
    }
    public static bool Delete(string target) { return CredDelete(target, 1, 0); }
}
"@

function Get-CredTarget([string]$UserHome) { return 'archivist:' + (Join-Path $UserHome '.archivist\credentials') }

function Get-SavedKey([string]$UserHome) {
    $file = Join-Path $UserHome '.archivist\credentials'
    if (Test-Path -LiteralPath $file) { return (Get-Content -LiteralPath $file -Raw).Trim() }
    return [ArchivistReplayCred]::Read((Get-CredTarget $UserHome))
}

# Assert-KeyInFile fails unless the credentials file holds Key and no
# Credential Manager item exists for this home (connect keeps its key in the
# file).
function Assert-KeyInFile([string]$UserHome, [string]$Key, [string]$What) {
    $file = Join-Path $UserHome '.archivist\credentials'
    if (-not (Test-Path -LiteralPath $file)) { Fail "${What}: no credentials file" }
    $saved = (Get-Content -LiteralPath $file -Raw).Trim()
    if ($Key -and $saved -ne $Key) { Fail "${What}: the credentials file holds another key" }
    if (-not $saved) { Fail "${What}: the credentials file is empty" }
    if ($null -ne [ArchivistReplayCred]::Read((Get-CredTarget $UserHome))) { Fail "${What}: a Credential Manager item was written" }
}

# Invoke-Bin runs the installed archivist.exe of UserHome in the replay
# environment; returns @{ Out; Code }.
function Invoke-Bin([string]$UserHome, [string[]]$BinArgs) {
    $vars = @{
        USERPROFILE = $UserHome; HOME = $UserHome; LOCALAPPDATA = (Join-Path $UserHome 'AppData\Local')
        ARCHIVIST_BASE_URL = $Base; ARCHIVIST_TOKEN = $null; STUB_LOG = $callLog
        STUB_STATE = (Join-Path $UserHome 'task-registered'); Path = "$stubs;$env:Path"
    }
    $saved = @{}
    foreach ($k in $vars.Keys) {
        $saved[$k] = [Environment]::GetEnvironmentVariable($k, 'Process')
        [Environment]::SetEnvironmentVariable($k, $vars[$k], 'Process')
    }
    $eap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $out = & (Join-Path $UserHome 'bin\archivist.exe') @BinArgs 2>&1 | Out-String
        return @{ Out = $out; Code = $LASTEXITCODE }
    } finally {
        $ErrorActionPreference = $eap
        foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k], 'Process') }
    }
}

# Get-TaskCommand is the <Command> of a UTF-16LE task XML.
function Get-TaskCommand([string]$Path) {
    $xml = [System.IO.File]::ReadAllText($Path, [System.Text.Encoding]::Unicode)
    if ($xml -notmatch '<Command>(.*?)</Command>') { Fail "no <Command> in $Path" }
    return $Matches[1]
}

# Get-RedeemCount is how many redeems the fake server has seen.
function Get-RedeemCount {
    if (-not (Test-Path -LiteralPath $redeemLog)) { return 0 }
    return @(Get-Content -LiteralPath $redeemLog | Where-Object { $_ -match '"path":"/agent-pairing/redeem"' }).Count
}

function Stop-StandIns {
    foreach ($log in $callLog, "$callLog.all") {
        if (-not (Test-Path -LiteralPath $log)) { continue }
        foreach ($line in Get-Content -LiteralPath $log) {
            if ($line -match '^daemon (\d+)$') { Stop-Process -Id ([int]$Matches[1]) -Force -ErrorAction SilentlyContinue }
        }
    }
}


try {
    # --- binaries ---------------------------------------------------------------
    $build = Join-Path $Work 'build'
    $oldBuild = Join-Path $Work 'build-old'
    $stubs = Join-Path $Work 'stubs'
    New-Item -ItemType Directory -Path $build, $stubs | Out-Null
    Push-Location $Root
    try {
        go build -o (Join-Path $build 'archivist.exe') ./cmd/archivist
        if ($LASTEXITCODE -ne 0) { Fail 'go build archivist.exe' }
        go build -ldflags '-H windowsgui' -o (Join-Path $build 'archivistw.exe') ./cmd/archivist
        if ($LASTEXITCODE -ne 0) { Fail 'go build archivistw.exe' }
        go build -o (Join-Path $stubs 'schtasks.exe') ./scripts/testdata/winreplay/schtasks
        if ($LASTEXITCODE -ne 0) { Fail 'go build the stub schtasks.exe' }
        go build -o (Join-Path $Work 'server.exe') ./scripts/testdata/winreplay/server
        if ($LASTEXITCODE -ne 0) { Fail 'go build the fake server' }
        # A release older than connect --preflight: the stand-in archivist.exe
        # with the real archivistw.exe.
        New-Item -ItemType Directory -Path $oldBuild | Out-Null
        go build -o (Join-Path $oldBuild 'archivist.exe') ./scripts/testdata/winreplay/oldarchivist
        if ($LASTEXITCODE -ne 0) { Fail 'go build the older archivist stand-in' }
        Copy-Item -LiteralPath (Join-Path $build 'archivistw.exe') -Destination (Join-Path $oldBuild 'archivistw.exe')
    } finally { Pop-Location }

    # --- test keys and the installer under test -----------------------------------
    # Throwaway keys: $testKey signs the fake releases, $otherKey is not trusted.
    $testKey = New-Object System.Security.Cryptography.RSACryptoServiceProvider 3072
    $otherKey = New-Object System.Security.Cryptography.RSACryptoServiceProvider 3072
    $pub = $testKey.ExportParameters($false)
    # install.ps1 with the lines between its release key markers replaced.
    $src = Get-Content -LiteralPath (Join-Path $Root 'scripts\install.ps1')
    if (@($src | Where-Object { $_ -eq '# BEGIN ARCHIVIST RELEASE KEY' }).Count -ne 1 -or
        @($src | Where-Object { $_ -eq '# END ARCHIVIST RELEASE KEY' }).Count -ne 1) { Fail 'install.ps1 has no single release key block' }
    $patched = New-Object System.Collections.Generic.List[string]
    $skip = $false
    foreach ($line in $src) {
        if ($line -eq '# END ARCHIVIST RELEASE KEY') { $skip = $false }
        if (-not $skip) { $patched.Add($line) }
        if ($line -eq '# BEGIN ARCHIVIST RELEASE KEY') {
            $patched.Add("`$ReleaseModulus = '$([Convert]::ToBase64String($pub.Modulus))'")
            $patched.Add("`$ReleaseExponent = '$([Convert]::ToBase64String($pub.Exponent))'")
            $skip = $true
        }
    }
    $Installer = Join-Path $Work 'install.ps1'
    Set-Content -LiteralPath $Installer -Value $patched -Encoding ascii
    if ((Get-Content -LiteralPath $Installer -Raw) -notmatch [regex]::Escape([Convert]::ToBase64String($pub.Modulus))) { Fail 'the test key was not patched in' }

    # Write-Signature signs a SHA256SUMS file (RSA PKCS#1 v1.5, SHA-256).
    function Write-Signature($Key, [string]$SumsPath) {
        $sig = $Key.SignData([System.IO.File]::ReadAllBytes($SumsPath), [System.Security.Cryptography.HashAlgorithmName]::SHA256,
            [System.Security.Cryptography.RSASignaturePadding]::Pkcs1)
        [System.IO.File]::WriteAllBytes("$SumsPath.sig", $sig)
    }

    # --- fake releases ---------------------------------------------------------------
    # Mode good: signed by the test key. mismatch: the zip does not match
    # SHA256SUMS. unsigned: SHA256SUMS (matching the zip) differs from the
    # signed one. nosig: no
    # signature. otherkey: signed by the untrusted key. old: good, with the
    # archivist.exe older than connect --preflight (Story 94.14).
    function New-Release([string]$Tag, [string]$Mode) {
        $ver = $Tag.Substring(1)
        $dir = Join-Path $Work "rel\download\$Tag"
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        $zip = Join-Path $dir "archivist_v${ver}_windows_amd64.zip"
        $src = $build
        if ($Mode -eq 'old') { $src = $oldBuild }
        Compress-Archive -Path (Join-Path $src 'archivist.exe'), (Join-Path $src 'archivistw.exe') -DestinationPath $zip
        # The skill bundle, listed in SHA256SUMS like the zip.
        $skillSrc = Join-Path $Work "skill-$Tag"
        New-Item -ItemType Directory -Force -Path $skillSrc | Out-Null
        Set-Content -LiteralPath (Join-Path $skillSrc 'SKILL.md') -Value "---`nname: archivist`n---" -Encoding ascii
        $bundle = Join-Path $dir "archivist_v${ver}_skill-bundle.tar.gz"
        & tar.exe -czf $bundle -C $skillSrc SKILL.md
        if ($LASTEXITCODE -ne 0) { Fail 'tar could not build the skill bundle' }
        $sumsPath = Join-Path $dir "archivist_v${ver}_SHA256SUMS"
        $writeSums = {
            $sums = foreach ($f in $zip, $bundle) {
                "$((Get-FileHash -LiteralPath $f -Algorithm SHA256).Hash.ToLowerInvariant())  $(Split-Path -Leaf $f)"
            }
            Set-Content -LiteralPath $sumsPath -Value $sums -Encoding ascii
        }
        & $writeSums
        if ($Mode -eq 'otherkey') { Write-Signature $otherKey $sumsPath }
        elseif ($Mode -ne 'nosig') { Write-Signature $testKey $sumsPath }
        if ($Mode -eq 'mismatch') { Add-Content -LiteralPath $zip -Value 'changed' }
        if ($Mode -eq 'unsigned') {
            $extra = Join-Path $Work "extra-$Tag.txt"
            Set-Content -LiteralPath $extra -Value 'extra' -Encoding ascii
            Remove-Item -LiteralPath $zip -Force
            Compress-Archive -Path (Join-Path $build 'archivist.exe'), (Join-Path $build 'archivistw.exe'), $extra -DestinationPath $zip
            & $writeSums
        }
    }
    New-Release 'v9.9.9' 'good'
    New-Release 'v9.9.8' 'mismatch'
    New-Release 'v9.9.7' 'unsigned'
    New-Release 'v9.9.6' 'nosig'
    New-Release 'v9.9.5' 'otherkey'
    New-Release 'v9.9.3' 'old'
    # The panel's one-liner fetches <base>/latest/download/install.ps1.
    $latest = Join-Path $Work 'rel\latest\download'
    New-Item -ItemType Directory -Force -Path $latest | Out-Null
    Copy-Item -LiteralPath $Installer -Destination (Join-Path $latest 'install.ps1')

    $portFile = Join-Path $Work 'port'
    $server = Start-Process -FilePath (Join-Path $Work 'server.exe') -ArgumentList (Join-Path $Work 'rel'), $redeemLog, $portFile -PassThru -WindowStyle Hidden
    for ($i = 0; $i -lt 100 -and -not (Test-Path -LiteralPath $portFile); $i++) { Start-Sleep -Milliseconds 100 }
    if (-not (Test-Path -LiteralPath $portFile)) { Fail 'the fake release server did not start' }
    $Base = 'http://127.0.0.1:' + (Get-Content -LiteralPath $portFile -Raw).Trim()

    $key1 = 'ak_replay_abcde12345' + ('x' * 24)
    $key2 = 'ak_replay_abcde12346' + ('x' * 24)

    # --- 1. install + pair + task --------------------------------------------------
    $h1 = Join-Path $Work 'home1'
    Reset-CallLog
    $r = Invoke-Install $h1 'v9.9.9' @('-Pair', 'abcde 12345')
    if ($r.Code -ne 0) { Fail "install + pair exited $($r.Code):`n$($r.Out)" }
    foreach ($exe in 'archivist.exe', 'archivistw.exe') {
        if (-not (Test-Path -LiteralPath (Join-Path $h1 "bin\$exe"))) { Fail "$exe not installed" }
    }
    Assert-KeyInFile $h1 $key1 'install + pair'
    $xml = Join-Path $h1 '.archivist\connect\MosaicArchivistConnect.xml'
    if (-not (Test-Path -LiteralPath $xml)) { Fail 'task XML not written' }
    $calls = Get-Content -LiteralPath $callLog -Raw
    if ($calls -notmatch 'schtasks /create /xml .* /tn MosaicArchivistConnect-\S+ /f') { Fail "task not created:`n$calls" }
    if ($calls -notmatch 'schtasks /run /tn MosaicArchivistConnect-') { Fail "task not run:`n$calls" }
    if ($calls -match 'ak_replay') { Fail 'a schtasks call carried the key' }
    $envFile = Get-Content -LiteralPath (Join-Path $h1 '.archivist\connect\service.env') -Raw
    if ($envFile -match 'ARCHIVIST_TOKEN|ak_replay') { Fail 'service.env carries a credential' }
    if ((Get-Content -LiteralPath $redeemLog -Raw) -notmatch '"code":"ABCDE12345"') { Fail 'redeem did not send the normalised code' }
    $pairingLines = @(Get-Content -LiteralPath $redeemLog | Where-Object { $_ -match '"path":"/agent-pairing/' })
    if (($pairingLines -match '"auth":"[^"]').Count -gt 0) { Fail 'pairing sent Authorization' }
    if (($pairingLines -match '"confirm":"browser"').Count -lt 1) { Fail 'redeem did not ask for the browser confirmation' }
    if (($pairingLines -match '"path":"/agent-pairing/pending/poll"').Count -lt 1) { Fail 'the pending pairing was not polled' }
    if ($r.Out -match [regex]::Escape($key1)) { Fail 'the key was printed' }
    if ($r.Out -notmatch 'Paired as archivist replay') { Fail "no pairing line:`n$($r.Out)" }
    if ($r.Out -notmatch 'Service:\s+running \(Task Scheduler:') { Fail "status not running:`n$($r.Out)" }
    if ($r.Out -notmatch 'Download verified\.') { Fail "no verified line:`n$($r.Out)" }
    if ($r.Out -notmatch [regex]::Escape('Starting Archivist when your computer starts (the default).')) { Fail "no default start line:`n$($r.Out)" }
    Pass 'install, pair, task created and running; no console takes the start default'

    # --- 2. update without -Pair recreates the task, no redeem --------------------
    Reset-CallLog
    $redeems = @(Get-Content -LiteralPath $redeemLog | Where-Object { $_ -match '"path":"/agent-pairing/redeem"' }).Count
    $bin1 = Join-Path $h1 'bin'
    foreach ($stale in 'archivist.exe.old-stale', 'archivistw.exe.old-stale', 'archivist.exe.old') {
        Set-Content -LiteralPath (Join-Path $bin1 $stale) -Value 'stale'
    }
    # Other files in a shared install directory are never touched.
    foreach ($keep in 'notes.old-draft', 'tool.exe.old-1', 'archivist.exe.older') {
        Set-Content -LiteralPath (Join-Path $bin1 $keep) -Value 'keep'
    }
    $r = Invoke-Install $h1 'v9.9.9' @()
    if ($r.Code -ne 0) { Fail "update exited $($r.Code):`n$($r.Out)" }
    $calls = Get-Content -LiteralPath $callLog -Raw
    if ($calls -notmatch 'schtasks /create' -or $calls -notmatch 'schtasks /end') { Fail "update did not reinstall the task:`n$calls" }
    if (@(Get-Content -LiteralPath $redeemLog | Where-Object { $_ -match '"path":"/agent-pairing/redeem"' }).Count -ne $redeems) { Fail 'update called redeem' }
    foreach ($stale in 'archivist.exe.old-stale', 'archivistw.exe.old-stale', 'archivist.exe.old') {
        if (Test-Path -LiteralPath (Join-Path $bin1 $stale)) { Fail "stale $stale was kept" }
    }
    foreach ($keep in 'notes.old-draft', 'tool.exe.old-1', 'archivist.exe.older') {
        if (-not (Test-Path -LiteralPath (Join-Path $bin1 $keep))) { Fail "unrelated $keep was removed" }
    }
    if ($r.Out -notmatch 'updated and restarted') { Fail "no restart line:`n$($r.Out)" }
    Pass 'update without -Pair reinstalls the task'

    # --- 2a. an update while the installed archivist.exe runs -------------------
    # The stand-in: `companies search --stdin --dry-run` reads its query from
    # stdin before anything else (no credentials, no network), so it runs
    # from the installed exe for as long as its stdin stays open.
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = Join-Path $bin1 'archivist.exe'
    $psi.Arguments = 'companies search --stdin --dry-run'
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    $psi.EnvironmentVariables['ARCHIVIST_TOKEN'] = ''
    $psi.EnvironmentVariables['ARCHIVIST_BASE_URL'] = 'http://127.0.0.1:1'
    $psi.EnvironmentVariables['USERPROFILE'] = $h1
    $psi.EnvironmentVariables['HOME'] = $h1
    $script:running = [System.Diagnostics.Process]::Start($psi)
    # Drain both streams so a failure can say why (and nothing blocks on a full pipe).
    $standOut = $script:running.StandardOutput.ReadToEndAsync()
    $standErr = $script:running.StandardError.ReadToEndAsync()
    Start-Sleep -Seconds 2
    if ($script:running.HasExited) {
        Fail "the stand-in archivist.exe exited (code $($script:running.ExitCode)):`n$($standOut.Result)`n$($standErr.Result)"
    }
    $r = Invoke-Install $h1 'v9.9.9' @()
    if ($r.Code -ne 0) { Fail "update while archivist.exe runs exited $($r.Code):`n$($r.Out)" }
    if ($script:running.HasExited) {
        Fail "the running archivist.exe stopped during the update (code $($script:running.ExitCode)):`n$($standOut.Result)`n$($standErr.Result)"
    }
    $olds = @(Get-ChildItem -LiteralPath $bin1 -Filter 'archivist.exe.old-*' -File | Where-Object { $_.Name -like 'archivist.exe.old-*' })
    if ($olds.Count -lt 1) { Fail 'the running archivist.exe was not renamed to archivist.exe.old-<id>' }
    if (-not (Test-Path -LiteralPath (Join-Path $bin1 'archivist.exe'))) { Fail 'no archivist.exe after the update' }
    # Closing stdin ends the stand-in normally (query read, dry run printed).
    $script:running.StandardInput.WriteLine('acme')
    $script:running.StandardInput.Close()
    if (-not $script:running.WaitForExit(15000)) { $script:running.Kill(); Fail 'the stand-in did not exit after its stdin closed' }
    if ($script:running.ExitCode -ne 0) {
        Fail "the stand-in exited $($script:running.ExitCode) after its stdin closed:`n$($standOut.Result)`n$($standErr.Result)"
    }
    $script:running = $null
    Pass 'an update while archivist.exe runs renames it aside and installs the new one'

    # --- 3. -NoService pairs without a task ---------------------------------------
    $h2 = Join-Path $Work 'home2'
    Reset-CallLog
    $r = Invoke-Install $h2 'v9.9.9' @('-Pair', 'ABCDE-12346', '-NoService')
    if ($r.Code -ne 0) { Fail "-NoService exited $($r.Code):`n$($r.Out)" }
    Assert-KeyInFile $h2 $key2 '-NoService pairing'
    if ((Get-Content -LiteralPath $callLog -Raw) -match 'schtasks /create') { Fail '-NoService created a task' }
    if (Test-Path -LiteralPath (Join-Path $h2 '.archivist\connect\MosaicArchivistConnect.xml')) { Fail '-NoService wrote a task XML' }
    Pass '-NoService pairs only'

    # --- 4. a failed task registration after pairing prints the resume step ------
    $h3 = Join-Path $Work 'home3'
    $r = Invoke-Install $h3 'v9.9.9' @('-Pair', 'ABCDE12345') @{ STUB_FAIL_CREATE = '1' }
    if ($r.Code -eq 0) { Fail 'a failed task registration exited 0' }
    Assert-KeyInFile $h3 '' 'pairing before a failed task registration'
    if ($r.Out -notmatch 'connect --install \(no new code is needed\)') { Fail "no resume step:`n$($r.Out)" }
    if ($r.Out -notmatch 'Access is denied') { Fail "schtasks output not shown:`n$($r.Out)" }
    Pass 'failed task registration after pairing prints the resume step'

    # --- 5. an archive that does not match its checksum: throws, nothing installed
    $h4 = Join-Path $Work 'home4'
    $r = Invoke-Install $h4 'v9.9.8' @('-Pair', 'ABCDE12345')
    if ($r.Code -eq 0) { Fail 'a mismatched archive exited 0' }
    if ($r.Out -notmatch 'checksum mismatch') { Fail "no checksum mismatch message:`n$($r.Out)" }
    if (Test-Path -LiteralPath (Join-Path $h4 'bin')) { Fail 'a mismatched archive left the install directory' }
    if (Test-Path -LiteralPath (Join-Path $h4 '.archivist')) { Fail 'a mismatched archive left ~\.archivist' }
    Pass 'an archive that does not match its checksum throws with nothing installed'

    # --- 6. the skill bundle lands in ~\.claude\skills\archivist -------------------
    $h6 = Join-Path $Work 'home6'
    New-Item -ItemType Directory -Force -Path (Join-Path $h6 '.claude\skills') | Out-Null
    $r = Invoke-Install $h6 'v9.9.9' @('-Pair', 'ABCDE12345', '-NoService')
    if ($r.Code -ne 0) { Fail "install with ~\.claude\skills exited $($r.Code):`n$($r.Out)" }
    if (-not (Test-Path -LiteralPath (Join-Path $h6 '.claude\skills\archivist\SKILL.md'))) { Fail "SKILL.md not installed:`n$($r.Out)" }
    Pass 'the skill bundle is verified and installed'

    # --- 7. the exact panel one-liner -------------------------------------------------
    $h7 = Join-Path $Work 'home7'
    Reset-CallLog
    $line = "& ([scriptblock]::Create((irm $Base/latest/download/install.ps1))) -Pair ABCDE12345"
    $r = Invoke-Install $h7 'v9.9.9' @() @{} $line
    if ($r.Code -ne 0) { Fail "the one-liner exited $($r.Code):`n$($r.Out)" }
    Assert-KeyInFile $h7 $key1 'the one-liner'
    if (-not (Test-Path -LiteralPath (Join-Path $h7 '.archivist\connect\MosaicArchivistConnect.xml'))) { Fail 'the one-liner did not install the task' }
    if ($r.Out -notmatch 'Service:\s+running \(Task Scheduler:') { Fail "the one-liner's status is not running:`n$($r.Out)" }
    Pass 'the panel one-liner installs, pairs and starts background connect'

    # --- 8. refusals: nothing installed ----------------------------------------------
    foreach ($case in @(
            @{ Name = 'a release whose checksums lack a valid signature'; Tag = 'v9.9.7'; Message = 'could not be verified'; Script = '' },
            @{ Name = 'a release without a signature'; Tag = 'v9.9.6'; Message = '404'; Script = '' },
            @{ Name = 'a release signed by another key'; Tag = 'v9.9.5'; Message = 'could not be verified'; Script = '' },
            @{ Name = 'the release key installer against a test signed release'; Tag = 'v9.9.9'; Message = 'could not be verified'; Script = (Join-Path $Root 'scripts\install.ps1') })) {
        $h = Join-Path $Work ('refuse-' + [guid]::NewGuid().ToString('N'))
        New-Item -ItemType Directory -Force -Path (Join-Path $h '.claude\skills') | Out-Null
        $r = Invoke-Install $h $case.Tag @('-Pair', 'ABCDE12345') @{} '' $case.Script
        Assert-Refused $case.Name $h $r $case.Message
        Pass "$($case.Name) is refused with nothing installed"
    }

    # --- 9. a plain http:// release URL that is not loopback, before any download ---
    foreach ($url in 'http://example.com/releases', 'http://127.0.0.1.example.com', 'https://user@example.com/releases', 'ftp://example.com') {
        $h = Join-Path $Work ('url-' + [guid]::NewGuid().ToString('N'))
        $r = Invoke-Install $h 'v9.9.9' @() @{ ARCHIVIST_RELEASE_BASE_URL = $url }
        Assert-Refused "release URL $url" $h $r 'ARCHIVIST_RELEASE_BASE_URL must'
        if ($r.Out -match 'Downloading archivist') { Fail "release URL $url started a download" }
    }
    Pass 'non loopback http:// and malformed release URLs are refused before any download'

    # --- 11. a probe launch failure stops before pairing (Story 94.14) ------------
    $h11 = Join-Path $Work 'home11'
    Reset-CallLog
    $redeems = Get-RedeemCount
    $r = Invoke-Install $h11 'v9.9.9' @('-Pair', 'ABCDE12345') @{ STUB_PROBE_RESULT = '2147942402' }
    if ($r.Code -eq 0) { Fail "a probe launch failure exited 0:`n$($r.Out)" }
    foreach ($want in 'This shell is sandboxed by a packaged app (for example the Claude desktop app), so Windows cannot see what it installs.',
        'Run this command in a normal PowerShell window.', 'Your pairing code was not used.') {
        if ($r.Out -notmatch [regex]::Escape($want)) { Fail "probe failure: no '$want':`n$($r.Out)" }
    }
    if ((Get-RedeemCount) -ne $redeems) { Fail 'a probe launch failure redeemed the code' }
    if (Test-Path -LiteralPath (Join-Path $h11 '.archivist\credentials')) { Fail 'a probe launch failure wrote credentials' }
    $calls = Get-Content -LiteralPath $callLog -Raw
    if ($calls -notmatch 'schtasks /run /tn MosaicArchivistProbe-\S+') { Fail "the probe did not run:`n$calls" }
    if ($calls -notmatch 'schtasks /delete /tn MosaicArchivistProbe-\S+ /f') { Fail "the probe task was not deleted:`n$calls" }
    if ($calls -match 'MosaicArchivistConnect-') { Fail "a probe launch failure touched the service task:`n$calls" }
    if (Test-Path -LiteralPath (Join-Path $h11 '.archivist\connect\MosaicArchivistProbe.xml')) { Fail 'the probe XML was kept' }
    Pass 'a probe launch failure stops before pairing; the code is not used'

    # --- 12. a task that does not start names Task Scheduler's result ------------
    $h12 = Join-Path $Work 'home12'
    Reset-CallLog
    $r = Invoke-Install $h12 'v9.9.9' @('-Pair', 'ABCDE12345') @{ STUB_RUN_FAIL = '2147942402' }
    if ($r.Code -eq 0) { Fail "a task that does not start exited 0:`n$($r.Out)" }
    Assert-KeyInFile $h12 $key1 'pairing before a task that does not start'
    foreach ($want in 'loaded, not running (Task Scheduler: the program file was not found (0x80070002)',
        'the Service line above says why', 'run this again without -Pair (no new code is needed)',
        "& ([scriptblock]::Create((irm $Base/latest/download/install.ps1)))") {
        if ($r.Out -notmatch [regex]::Escape($want)) { Fail "not running: no '$want':`n$($r.Out)" }
    }
    if ($r.Out -match 'last log line' -or $r.Out -match 'Last log:') { Fail "not running: a log line is named:`n$($r.Out)" }
    Pass 'a task that does not start names the Task Scheduler result and the rerun without -Pair'

    # --- 13. the default location, moving a legacy AppData install ---------------
    $h13 = Join-Path $Work 'home13'
    $legacy = Join-Path $h13 'AppData\Local\Programs\archivist'
    Reset-CallLog
    # A v0.2.35 style install: the exes and the task in %LOCALAPPDATA%\Programs\archivist.
    $r = Invoke-Install $h13 'v9.9.9' @('-Pair', 'ABCDE12345') @{ ARCHIVIST_INSTALL_DIR = $legacy }
    if ($r.Code -ne 0) { Fail "seeding the legacy install exited $($r.Code):`n$($r.Out)" }
    if ($r.Out -match 'Moved archivist') { Fail "an ARCHIVIST_INSTALL_DIR install moved something:`n$($r.Out)" }
    $xml13 = Join-Path $h13 '.archivist\connect\MosaicArchivistConnect.xml'
    if ((Get-TaskCommand $xml13) -ne (Join-Path $legacy 'archivistw.exe')) { Fail "the seeded task does not run the legacy exe: $(Get-TaskCommand $xml13)" }
    Reset-CallLog
    $r = Invoke-Install $h13 'v9.9.9' @() @{ ARCHIVIST_INSTALL_DIR = $null }
    if ($r.Code -ne 0) { Fail "the move exited $($r.Code):`n$($r.Out)" }
    $newDir = Join-Path $h13 '.archivist\bin'
    foreach ($exe in 'archivist.exe', 'archivistw.exe') {
        if (-not (Test-Path -LiteralPath (Join-Path $newDir $exe))) { Fail "$exe not in $newDir" }
        if (Test-Path -LiteralPath (Join-Path $legacy $exe)) { Fail "the legacy $exe was kept" }
    }
    if (Test-Path -LiteralPath $legacy) { Fail 'the empty legacy folder was kept' }
    if ((Get-TaskCommand $xml13) -ne (Join-Path $newDir 'archivistw.exe')) { Fail "the task does not run the new exe: $(Get-TaskCommand $xml13)" }
    if ($r.Out -notmatch [regex]::Escape("Moved archivist from $legacy to $newDir.")) { Fail "no move line:`n$($r.Out)" }
    if ($r.Out -notmatch 'Service:\s+running \(Task Scheduler:') { Fail "status after the move is not running:`n$($r.Out)" }
    Pass 'the default location is ~\.archivist\bin and a legacy AppData install moves there with its task'

    # --- 13b. a failed move keeps the legacy install ------------------------------
    $h13b = Join-Path $Work 'home13b'
    $legacy13b = Join-Path $h13b 'AppData\Local\Programs\archivist'
    Reset-CallLog
    $r = Invoke-Install $h13b 'v9.9.9' @('-Pair', 'ABCDE12345') @{ ARCHIVIST_INSTALL_DIR = $legacy13b }
    if ($r.Code -ne 0) { Fail "seeding the legacy install exited $($r.Code):`n$($r.Out)" }
    Reset-CallLog
    $r = Invoke-Install $h13b 'v9.9.9' @() @{ ARCHIVIST_INSTALL_DIR = $null; STUB_RUN_FAIL = '2147942402' }
    if ($r.Code -eq 0) { Fail "a move whose task does not start exited 0:`n$($r.Out)" }
    foreach ($exe in 'archivist.exe', 'archivistw.exe') {
        if (-not (Test-Path -LiteralPath (Join-Path $legacy13b $exe))) { Fail "a failed move removed the legacy $exe" }
    }
    if ($r.Out -match 'Moved archivist') { Fail "a failed move printed the move line:`n$($r.Out)" }
    if ($r.Out -notmatch [regex]::Escape('Once that is fixed, run this command again.')) { Fail "a failed move: no rerun line:`n$($r.Out)" }
    Pass 'a move whose task does not start keeps the legacy install'

    # --- 14. a re-pair under the installer: no restart hint, no variable left -----
    Reset-CallLog
    $line = "& ([scriptblock]::Create((irm $Base/latest/download/install.ps1))) -Pair ABCDE12346; Write-Host ('installer variable after: [' + `$env:ARCHIVIST_INSTALLER + ']')"
    $r = Invoke-Install $h7 'v9.9.9' @() @{} $line
    if ($r.Code -ne 0) { Fail "re-pair under the installer exited $($r.Code):`n$($r.Out)" }
    Assert-KeyInFile $h7 $key2 're-pair under the installer'
    if ($r.Out -match [regex]::Escape("to restart background connect with the new key")) { Fail "the redundant restart hint was printed:`n$($r.Out)" }
    if ($r.Out -notmatch 'updated and restarted') { Fail "the task was not reinstalled:`n$($r.Out)" }
    if ($r.Out -notmatch [regex]::Escape('installer variable after: []')) { Fail "ARCHIVIST_INSTALLER was left in the session:`n$($r.Out)" }
    Pass 're-pair under the installer prints no restart hint and leaves no ARCHIVIST_INSTALLER'

    # --- 15. an archivist older than connect --preflight: the check is skipped ----
    $h15 = Join-Path $Work 'home15'
    Reset-CallLog
    $redeems = Get-RedeemCount
    $r = Invoke-Install $h15 'v9.9.3' @('-Pair', 'ABCDE12345') @{ STUB_REAL_ARCHIVIST = (Join-Path $build 'archivist.exe') }
    if ($r.Code -ne 0) { Fail "an older archivist exited $($r.Code):`n$($r.Out)" }
    $calls = Get-Content -LiteralPath $callLog -Raw
    if ($calls -notmatch 'oldarchivist connect --preflight') { Fail "the older archivist was not asked for the check:`n$calls" }
    if ($calls -match 'MosaicArchivistProbe-') { Fail "a probe task ran for the older archivist:`n$calls" }
    if ($r.Out -match 'unknown flag') { Fail "the older archivist's unknown flag error was shown:`n$($r.Out)" }
    if ($r.Out -match [regex]::Escape('Your pairing code was not used')) { Fail "the older archivist stopped the install:`n$($r.Out)" }
    if ((Get-RedeemCount) -ne $redeems + 1) { Fail 'the older archivist did not pair once' }
    Assert-KeyInFile $h15 $key1 'pairing with an older archivist'
    if ($r.Out -notmatch 'Service:\s+running \(Task Scheduler:') { Fail "status with an older archivist is not running:`n$($r.Out)" }
    Pass 'an archivist older than connect --preflight skips the check silently and pairs'

    # --- 10. connect --uninstall cleans this computer up (Story 94.6) ---------------
    Reset-CallLog
    $deletesBefore = @(Get-Content -LiteralPath $redeemLog | Where-Object { $_ -match '"method":"DELETE"' }).Count
    $r = Invoke-Bin $h1 @('connect', '--uninstall')
    if ($r.Code -ne 0) { Fail "uninstall exited $($r.Code):`n$($r.Out)" }
    $deletes = @(Get-Content -LiteralPath $redeemLog | Where-Object { $_ -match '"method":"DELETE"' })
    if ($deletes.Count -ne $deletesBefore + 1) { Fail "uninstall did not revoke once:`n$($deletes -join "`n")" }
    if ($deletes[-1] -notmatch [regex]::Escape("""auth"":""Bearer $key1""")) { Fail "uninstall did not revoke the saved key with itself:`n$($deletes[-1])" }
    $calls = Get-Content -LiteralPath $callLog -Raw
    if ($calls -notmatch 'schtasks /delete') { Fail "the task was not deleted:`n$calls" }
    if (Get-SavedKey $h1) { Fail 'uninstall kept the saved key' }
    foreach ($left in (Join-Path $h1 '.archivist\connect'), (Join-Path $h1 '.archivist\credentials')) {
        if (Test-Path -LiteralPath $left) { Fail "uninstall left $left" }
    }
    if ($r.Out -match [regex]::Escape($key1)) { Fail 'uninstall printed the key' }
    if ($r.Out -notmatch 'no longer works') { Fail "no revoke line:`n$($r.Out)" }
    $r = Invoke-Bin $h1 @('connect', '--uninstall')
    if ($r.Code -ne 0 -or $r.Out -notmatch 'Background connect is not installed\.') { Fail "second uninstall exited $($r.Code):`n$($r.Out)" }
    if (@(Get-Content -LiteralPath $redeemLog | Where-Object { $_ -match '"method":"DELETE"' }).Count -ne $deletesBefore + 1) { Fail 'a second uninstall revoked again' }
    Pass 'uninstall revokes the key, deletes the task and removes the key and the connect files'

    Write-Host 'install.ps1 replay: all checks passed'
}
finally {
    if ($script:running -and -not $script:running.HasExited) { $script:running.Kill() }
    Stop-StandIns
    if ($server) { Stop-Process -Id $server.Id -Force -ErrorAction SilentlyContinue }
    # Credential Manager items the replay's installs created (Story 94.6).
    foreach ($d in @(Get-ChildItem -LiteralPath $Work -Directory -ErrorAction SilentlyContinue)) {
        [void][ArchivistReplayCred]::Delete((Get-CredTarget $d.FullName))
    }
    Remove-Item -LiteralPath $Work -Recurse -Force -ErrorAction SilentlyContinue
}

# Every check passed (a failed one threw above). Exit 0 explicitly: the last
# native command of the replay (a refused install) set $LASTEXITCODE, and
# a runner that exits with $LASTEXITCODE would report that as a failure.
exit 0
