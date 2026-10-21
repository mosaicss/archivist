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

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version 3.0
$ProgressPreference = 'SilentlyContinue'

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
        STUB_FAIL_CREATE = $null; Path = "$stubs;$env:Path"
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
    # signature. otherkey: signed by the untrusted key.
    function New-Release([string]$Tag, [string]$Mode) {
        $ver = $Tag.Substring(1)
        $dir = Join-Path $Work "rel\download\$Tag"
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        $zip = Join-Path $dir "archivist_v${ver}_windows_amd64.zip"
        Compress-Archive -Path (Join-Path $build 'archivist.exe'), (Join-Path $build 'archivistw.exe') -DestinationPath $zip
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
    # The panel's one-liner fetches <base>/latest/download/install.ps1.
    $latest = Join-Path $Work 'rel\latest\download'
    New-Item -ItemType Directory -Force -Path $latest | Out-Null
    Copy-Item -LiteralPath $Installer -Destination (Join-Path $latest 'install.ps1')

    $portFile = Join-Path $Work 'port'
    $redeemLog = Join-Path $Work 'redeem.log'
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
    if ((Get-Content -LiteralPath (Join-Path $h1 '.archivist\credentials') -Raw).Trim() -ne $key1) { Fail 'credentials not saved' }
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
    Pass 'install, pair, task created and running'

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
    if ((Get-Content -LiteralPath (Join-Path $h2 '.archivist\credentials') -Raw).Trim() -ne $key2) { Fail '-NoService did not pair' }
    if ((Get-Content -LiteralPath $callLog -Raw) -match 'schtasks /create') { Fail '-NoService created a task' }
    if (Test-Path -LiteralPath (Join-Path $h2 '.archivist\connect\MosaicArchivistConnect.xml')) { Fail '-NoService wrote a task XML' }
    Pass '-NoService pairs only'

    # --- 4. a failed task registration after pairing prints the resume step ------
    $h3 = Join-Path $Work 'home3'
    $r = Invoke-Install $h3 'v9.9.9' @('-Pair', 'ABCDE12345') @{ STUB_FAIL_CREATE = '1' }
    if ($r.Code -eq 0) { Fail 'a failed task registration exited 0' }
    if (-not (Test-Path -LiteralPath (Join-Path $h3 '.archivist\credentials'))) { Fail 'pairing was lost' }
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
    if ((Get-Content -LiteralPath (Join-Path $h7 '.archivist\credentials') -Raw).Trim() -ne $key1) { Fail 'the one-liner did not pair' }
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

    Write-Host 'install.ps1 replay: all checks passed'
}
finally {
    if ($script:running -and -not $script:running.HasExited) { $script:running.Kill() }
    Stop-StandIns
    if ($server) { Stop-Process -Id $server.Id -Force -ErrorAction SilentlyContinue }
    Remove-Item -LiteralPath $Work -Recurse -Force -ErrorAction SilentlyContinue
}

# Every check passed (a failed one threw above). Exit 0 explicitly: the last
# native command of the replay (a refused install) set $LASTEXITCODE, and
# a runner that exits with $LASTEXITCODE would report that as a failure.
exit 0
