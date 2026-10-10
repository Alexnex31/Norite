<#
.SYNOPSIS
Install the Norite client for the current user on Windows: norite.exe (the command line and the terminal
client) and norite-daemon.exe (the background program it talks to). It never asks for administrator rights.

.DESCRIPTION
In order: gets the two programs (a release archive, checked, or a build of the checkout it sits in), puts
them in %LOCALAPPDATA%\Programs\Norite and on your PATH, starts the daemon, then offers to create an account
or sign in.

    irm https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install.ps1 | iex

    # with options, which `irm | iex` cannot carry:
    & ([scriptblock]::Create((irm https://raw.githubusercontent.com/Alexnex31/Norite/main/scripts/install.ps1))) -Instance https://chat.example.com

    .\scripts\install.ps1            # in a checkout: builds from that source instead

Written for Windows PowerShell 5.1, the one every Windows has, so nothing newer than it is used. The work is
inside Install-Norite, called on the last line, so a download cut short is a parse error that runs nothing.

.PARAMETER Version
Install this release, such as v0.1.0-alpha. Default: the newest.
.PARAMETER FromSource
Build the checkout this script is in. The default when it is in one; needs Go.
.PARAMETER Release
Download a release even when run from a checkout.
.PARAMETER BinDir
Install into this folder. Default: %LOCALAPPDATA%\Programs\Norite.
.PARAMETER Instance
The instance to register on or sign in to. Default: ask.
.PARAMETER InviteCode
The instance invite code, for an instance that needs one to register.
.PARAMETER NoDaemon
Do not start the daemon.
.PARAMETER AutoStart
Also register the daemon to start at every logon, with `norite daemon install`.
.PARAMETER NoAccount
Do not offer to register or sign in.
.PARAMETER RequireSignature
Fail unless cosign is installed and the release's signature verifies.
.PARAMETER Uninstall
Sign out, stop the daemon, and remove the programs and the PATH entry.
#>
[CmdletBinding()]
param(
    [string]$Version,
    [switch]$FromSource,
    [switch]$Release,
    [string]$BinDir,
    [string]$Instance,
    [string]$InviteCode,
    [switch]$NoDaemon,
    [switch]$AutoStart,
    [switch]$NoAccount,
    [switch]$RequireSignature,
    [switch]$Uninstall
)

function Install-Norite {
    param(
        [string]$Version, [bool]$FromSource, [bool]$Release, [string]$BinDir, [string]$Instance,
        [string]$InviteCode, [bool]$NoDaemon, [bool]$AutoStart, [bool]$NoAccount, [bool]$RequireSignature,
        [bool]$Uninstall, [string]$ScriptPath
    )

    # Local to this function, so running through `iex` leaves the caller's session as it was.
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'   # the progress bar makes Invoke-WebRequest many times slower

    $repo = 'Alexnex31/Norite'

    function Step($text) { Write-Host ''; Write-Host "==> $text" }
    function Say($text) { Write-Host $text }
    function Note($text) { Write-Host "warning: $text" -ForegroundColor Yellow }

    # NORITE_INSTALL_ALLOW_ANY_OS exists so the download and checking code can be exercised where there is no
    # Windows; nothing after the files are in place can work there.
    $onWindows = ($env:OS -eq 'Windows_NT')
    if (-not $onWindows -and -not $env:NORITE_INSTALL_ALLOW_ANY_OS) {
        throw 'This script is for Windows. On macOS and Linux, use scripts/install.sh'
    }

    $archName = $env:PROCESSOR_ARCHITEW6432
    if (-not $archName) { $archName = $env:PROCESSOR_ARCHITECTURE }
    if (-not $archName) { $archName = [string][System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture }
    switch -Regex ($archName) {
        '^(AMD64|X64)$' { $arch = 'amd64'; break }
        '^ARM64$' { $arch = 'arm64'; break }
        default { throw "Norite is built for x64 and ARM64 Windows, not $archName" }
    }

    if (-not $BinDir) {
        if (-not $env:LOCALAPPDATA) { throw 'LOCALAPPDATA is not set; pass -BinDir' }
        $BinDir = Join-Path $env:LOCALAPPDATA 'Programs\Norite'
    }
    $norite = Join-Path $BinDir 'norite.exe'
    $daemon = Join-Path $BinDir 'norite-daemon.exe'

    # Stop-Daemon stops a daemon running from $BinDir. Windows will not replace or delete a program that is
    # running. The daemon is asked first, by the norite already installed, which is the running daemon's own
    # version and so can attach to it: a daemon that stops itself finishes storing a renewed sign-in, and one
    # ended while doing that is signed out at its next start. Whatever is still running after that, an older
    # daemon that does not know the request or one that is stuck, is ended outright.
    function Stop-Daemon {
        # In a try: with no daemon and no task, `daemon stop` says so on stderr and exits non-zero, and under
        # $ErrorActionPreference = 'Stop' Windows PowerShell 5.1 turns a redirected stderr line from a program
        # into a terminating error, which would end the install before a file was copied.
        if (Test-Path $norite) { try { & $norite daemon stop 2>$null | Out-Null } catch { } }
        Get-Process -Name 'norite-daemon' -ErrorAction SilentlyContinue |
            Where-Object { $_.Path -eq $daemon } |
            Stop-Process -Force -ErrorAction SilentlyContinue
    }

    function Get-UserPathEntries {
        $p = [Environment]::GetEnvironmentVariable('Path', 'User')
        if (-not $p) { return @() }
        return @($p -split ';' | Where-Object { $_ })
    }

    if ($Uninstall) {
        if (-not (Test-Path $norite)) { throw "There is no norite.exe in $BinDir; pass -BinDir if it was installed elsewhere" }
        Step "Removing Norite from $BinDir"
        & $norite logout 2>$null | Out-Null
        & $norite daemon uninstall 2>$null | Out-Null
        Stop-Daemon
        Start-Sleep -Milliseconds 500
        Remove-Item -Force -ErrorAction SilentlyContinue $norite, $daemon
        $kept = @(Get-UserPathEntries | Where-Object { $_ -ne $BinDir })
        [Environment]::SetEnvironmentVariable('Path', ($kept -join ';'), 'User')
        Say 'Removed. The daemon''s state is left in place; delete it for a clean slate:'
        Say "  $env:LOCALAPPDATA\Norite"
        return
    }

    # A script run from a checkout builds that checkout unless told otherwise. Through `iex` there is no file,
    # so no checkout.
    $checkout = $null
    if ($ScriptPath) {
        $root = Split-Path -Parent (Split-Path -Parent $ScriptPath)
        if ((Test-Path (Join-Path $root 'go.work')) -and (Test-Path (Join-Path $root 'cli')) -and (Test-Path (Join-Path $root 'daemon'))) {
            $checkout = $root
        }
    }
    $fromSourceMode = $FromSource -or ($checkout -and -not $Release -and -not $Version)

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ('norite-install-' + [Guid]::NewGuid().ToString('N'))
    $out = Join-Path $tmp 'out'
    New-Item -ItemType Directory -Force -Path $out | Out-Null

    try {
        if ($fromSourceMode) {
            if (-not $checkout) { throw '-FromSource needs this script to be run from a checkout of the repository' }
            if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw 'Building from source needs Go 1.26 or newer: https://go.dev/dl/' }
            Step "Building Norite from $checkout"
            # The way a release builds: no cgo, no workspace. Restored afterwards, since they are the session's.
            $savedCgo = $env:CGO_ENABLED; $savedWork = $env:GOWORK
            $env:CGO_ENABLED = '0'; $env:GOWORK = 'off'
            try {
                Push-Location (Join-Path $checkout 'cli')
                try { & go build -trimpath -o (Join-Path $out 'norite.exe') ./cmd/app; if ($LASTEXITCODE -ne 0) { throw 'building norite.exe failed' } }
                finally { Pop-Location }
                Push-Location (Join-Path $checkout 'daemon')
                try { & go build -trimpath -o (Join-Path $out 'norite-daemon.exe') ./cmd/daemond; if ($LASTEXITCODE -ne 0) { throw 'building norite-daemon.exe failed' } }
                finally { Pop-Location }
            }
            finally { $env:CGO_ENABLED = $savedCgo; $env:GOWORK = $savedWork }
        }
        else {
            # Windows PowerShell 5.1 offers TLS 1.0 first unless told; GitHub refuses it.
            [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

            $tag = $Version
            if (-not $tag) {
                # The list rather than /releases/latest, because "latest" skips prereleases and the alpha is one.
                $releases = @(Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$repo/releases?per_page=1")
                if ($releases.Count -gt 0 -and $releases[0]) { $tag = [string]$releases[0].tag_name }
            }
            if (-not $tag) { throw 'No release is published yet. In a checkout, run this script with -FromSource' }
            # The tag becomes part of a URL and a file name, so it is held to what a tag is made of.
            if ($tag -notmatch '^[A-Za-z0-9._+-]+$') { throw "'$tag' is not a release tag" }
            $ver = $tag -replace '^v', ''
            $base = $env:NORITE_RELEASE_BASE_URL
            if (-not $base) { $base = "https://github.com/$repo/releases/download/$tag" }

            Step "Downloading Norite $tag for windows/$arch"
            $sums = Join-Path $tmp 'checksums.txt'
            Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile $sums

            # The signature is over checksums.txt, so it is checked before anything checksums.txt vouches for.
            if (Get-Command cosign -ErrorAction SilentlyContinue) {
                $bundle = Join-Path $tmp 'checksums.txt.sigstore.json'
                Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt.sigstore.json" -OutFile $bundle
                & cosign verify-blob $sums --bundle $bundle `
                    --certificate-identity "https://github.com/$repo/.github/workflows/release.yml@refs/tags/$tag" `
                    --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' 2>&1 | Out-Null
                if ($LASTEXITCODE -ne 0) { throw 'The release''s signature did not verify; not installing it' }
                Say "signature: verified, signed by this repository's release workflow at $tag"
            }
            elseif ($RequireSignature) {
                throw '-RequireSignature needs cosign: https://docs.sigstore.dev/cosign/system_config/installation/'
            }
            else {
                Say 'signature: not checked, because cosign is not installed. The checksum below proves the'
                Say '           download is intact, not who built it; install cosign and pass -RequireSignature for that.'
            }

            $archive = "norite_${ver}_windows_${arch}.zip"
            $zip = Join-Path $tmp $archive
            Invoke-WebRequest -UseBasicParsing -Uri "$base/$archive" -OutFile $zip
            $want = $null
            foreach ($line in Get-Content $sums) {
                $fields = @($line -split '\s+' | Where-Object { $_ })
                if ($fields.Count -ge 2 -and ($fields[1] -eq $archive -or $fields[1] -eq "*$archive")) { $want = $fields[0].ToLower() }
            }
            if (-not $want) { throw "$archive is not listed in this release's checksums.txt" }
            $got = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash.ToLower()
            if ($want -ne $got) { throw "$archive does not match its checksum (wanted $want, got $got); not installing it" }
            Say "checksum:  $archive matches"
            Expand-Archive -Path $zip -DestinationPath $out -Force
        }

        foreach ($name in 'norite.exe', 'norite-daemon.exe') {
            if (-not (Test-Path (Join-Path $out $name))) { throw "$name is missing from what was fetched" }
        }

        Step "Installing into $BinDir"
        New-Item -ItemType Directory -Force -Path $BinDir | Out-Null
        Stop-Daemon
        foreach ($name in 'norite.exe', 'norite-daemon.exe') {
            $target = Join-Path $BinDir $name
            # A program still running cannot be overwritten, but it can be renamed out of the way.
            if (Test-Path $target) {
                try { Remove-Item -Force $target }
                catch { Move-Item -Force $target ($target + '.old-' + [Guid]::NewGuid().ToString('N').Substring(0, 8)) }
            }
            Copy-Item -Force (Join-Path $out $name) $target
            if ($onWindows) { Unblock-File -Path $target -ErrorAction SilentlyContinue }
        }
        Get-ChildItem -Path $BinDir -Filter '*.old-*' -ErrorAction SilentlyContinue | Remove-Item -Force -ErrorAction SilentlyContinue
        Say 'norite.exe, norite-daemon.exe'
    }
    finally {
        Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $tmp
    }

    if (-not $onWindows) { Say 'Not Windows: stopping after placing the files.'; return }

    & $norite --help | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "$norite was installed but does not run on this machine" }

    if ((Get-UserPathEntries) -notcontains $BinDir) {
        $entries = @(Get-UserPathEntries) + $BinDir
        [Environment]::SetEnvironmentVariable('Path', ($entries -join ';'), 'User')
        $env:Path = "$env:Path;$BinDir"
        Say "Added $BinDir to your PATH. New terminal windows will find 'norite'; this one already does."
    }

    if (-not $NoDaemon) {
        Step 'Starting the daemon'
        if ($AutoStart) {
            & $norite daemon install
            if ($LASTEXITCODE -eq 0) { & $norite daemon start }
            if ($LASTEXITCODE -ne 0) { Note 'the daemon could not be registered to start at logon; starting it for this session instead.' }
        }
        if (-not (Get-Process -Name 'norite-daemon' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $daemon })) {
            Start-Process -FilePath $daemon -WindowStyle Hidden
        }
        Say 'The daemon is running in the background.'
        if (-not $AutoStart) {
            Say 'It does not start by itself after a restart of Windows. To start it then:'
            Say "  Start-Process '$daemon' -WindowStyle Hidden"
            Say 'or run this installer again with -AutoStart to register it at logon.'
        }
    }

    if (-not $NoAccount) {
        $canAsk = [Environment]::UserInteractive -and -not [Console]::IsInputRedirected
        if (-not $canAsk) {
            Say ''
            Say 'No console to ask on, so no account was set up. When you are ready:'
            Say '  norite register --instance INSTANCE-URL    # or: norite login --instance INSTANCE-URL'
        }
        else {
            Step 'Your account'
            if (-not $Instance) { $Instance = (Read-Host 'Instance address, e.g. https://chat.example.com (Enter to skip)').Trim() }
            if (-not $Instance) {
                Say 'Skipped. Later: norite register --instance URL, or norite login --instance URL'
            }
            else {
                $existing = (Read-Host "Do you already have an account on $Instance? [y/N]").Trim()
                # Asked here and handed to both commands, so it is typed once rather than once each.
                $email = (Read-Host 'Email').Trim()
                if (-not $email) {
                    Say 'No email given, so no account was set up.'
                }
                else {
                    $ok = $true
                    if ($existing -notmatch '^(y|yes)$') {
                        if (-not $InviteCode) { $InviteCode = (Read-Host 'Instance invite code, if this instance needs one to register (Enter for none)').Trim() }
                        $registerArgs = @('register', '--instance', $Instance, '--email', $email)
                        if ($InviteCode) { $registerArgs += @('--invite-code', $InviteCode) }
                        & $norite @registerArgs
                        if ($LASTEXITCODE -ne 0) {
                            Note "registering did not finish. Try again with: norite register --instance $Instance"
                            $ok = $false
                        }
                    }
                    if ($ok) {
                        & $norite login --instance $Instance --email $email
                        if ($LASTEXITCODE -ne 0) { Note "signing in did not finish. Try again with: norite login --instance $Instance" }
                    }
                }
            }
        }
    }

    Step 'Done'
    Say "Run 'norite' to open the client, and 'norite --help' for everything else."
}

Install-Norite -Version $Version -FromSource $FromSource.IsPresent -Release $Release.IsPresent -BinDir $BinDir `
    -Instance $Instance -InviteCode $InviteCode -NoDaemon $NoDaemon.IsPresent -AutoStart $AutoStart.IsPresent `
    -NoAccount $NoAccount.IsPresent -RequireSignature $RequireSignature.IsPresent -Uninstall $Uninstall.IsPresent `
    -ScriptPath $PSCommandPath
