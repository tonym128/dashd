# Installer for the dashd push agent on Windows.
#
#   irm https://raw.githubusercontent.com/tonym128/dashd/main/packaging/install.ps1 | iex
#
# Downloads the release for this machine's architecture, verifies it against
# the release's checksums.txt, stores the ingest token in a file only the
# current user can read, and registers a hidden Task Scheduler task that
# restarts on failure. Every prompt has a parameter, so it can run unattended:
#
#   & ([scriptblock]::Create((irm .../install.ps1))) -Remote https://dashd.example.com -TokenFile .\ingest.token -Yes

param(
    [switch]$Upgrade,
    [switch]$Uninstall,
    [switch]$Yes,
    [string]$Remote = "",
    [string]$TokenFile = "",
    [string]$Machine = "",
    [string]$Interval = "",
    [string]$ModelsRefresh = "",
    [string]$RedactPaths = "",
    [string]$AutoUpdate = "",
    [string]$Version = ""
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$Repo = "tonym128/dashd"
$InstallDir = Join-Path $env:LOCALAPPDATA "dashd"
$ModelsPath = Join-Path $InstallDir "models.json"
$StatePath = Join-Path $InstallDir "push-state.db"
$ExePath = Join-Path $InstallDir "dashd.exe"
$TokenPath = Join-Path $InstallDir "ingest.token"
$TaskName = "dashd-push"

function Ask([string]$Current, [string]$Prompt, [string]$Default) {
    if ($Current) { return $Current }
    if ($Yes) { return $Default }
    $reply = Read-Host "$Prompt [$Default]"
    if ([string]::IsNullOrWhiteSpace($reply)) { return $Default }
    return $reply
}

if ($Uninstall) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    if (Test-Path $ExePath) { Remove-Item -Force $ExePath }
    Write-Host "Removed the '$TaskName' task and $ExePath. Kept $StatePath, $ModelsPath and $TokenPath."
    exit 0
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

$Arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    default { throw "Unsupported architecture: $($env:PROCESSOR_ARCHITECTURE)" }
}

if (-not $Version) {
    Write-Host "Fetching latest release metadata from GitHub ($Repo)..."
    $Release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{ "User-Agent" = "dashd-installer" }
    $Version = $Release.tag_name
}
$Ver = $Version.TrimStart("v")
$Archive = "dashd_${Ver}_windows_${Arch}.zip"
$Base = "https://github.com/$Repo/releases/download/$Version"

$Tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("dashd-" + [guid]::NewGuid())
New-Item -ItemType Directory -Force -Path $Tmp | Out-Null
try {
    $Zip = Join-Path $Tmp $Archive
    Write-Host "Downloading $Base/$Archive..."
    Invoke-WebRequest -Uri "$Base/$Archive" -OutFile $Zip -UseBasicParsing
    $Sums = (Invoke-WebRequest -Uri "$Base/checksums.txt" -UseBasicParsing).Content
    if ($Sums -is [byte[]]) { $Sums = [System.Text.Encoding]::UTF8.GetString($Sums) }
    $Want = ($Sums -split "`n" | ForEach-Object { $_.Trim() } | Where-Object { $_ -match "\s\*?$([regex]::Escape($Archive))$" } |
        ForEach-Object { ($_ -split "\s+")[0] } | Select-Object -First 1)
    if (-not $Want) { throw "$Archive is not listed in checksums.txt; not installing" }
    $Got = (Get-FileHash -Algorithm SHA256 -Path $Zip).Hash.ToLowerInvariant()
    if ($Got -ne $Want.ToLowerInvariant()) { throw "Checksum mismatch for $Archive (got $Got, release says $Want); not installing" }
    Write-Host "Checksum verified."

    Expand-Archive -Path $Zip -DestinationPath $Tmp -Force
    $Extracted = Join-Path $Tmp "dashd_${Ver}_windows_${Arch}"
    Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    Copy-Item -Force (Join-Path $Extracted "dashd.exe") $ExePath
    if (Test-Path (Join-Path $Extracted "models.json")) {
        Copy-Item -Force (Join-Path $Extracted "models.json") $ModelsPath
    }
} finally {
    Remove-Item -Recurse -Force $Tmp -ErrorAction SilentlyContinue
}
Write-Host "Installed dashd $Version to $ExePath"

if ($Upgrade) {
    if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
        Start-ScheduledTask -TaskName $TaskName
        Write-Host "Upgraded and restarted '$TaskName' (configuration kept)."
    } else {
        Write-Host "Upgraded the binary. No '$TaskName' task exists; run without -Upgrade to configure one."
    }
    exit 0
}

Write-Host ""
Write-Host "=== dashd push agent configuration ==="
while (-not $Remote) {
    if ($Yes) { throw "-Remote is required with -Yes" }
    $Remote = Read-Host "Remote dashd server URL (e.g. https://dashd.example.com)"
}
if ($Remote -like "http://*" -and $Remote -notmatch "^http://(127\.0\.0\.1|localhost)") {
    Write-Warning "$Remote is plain HTTP; the token and every session travel unencrypted."
}

$Token = ""
if ($TokenFile) {
    $Token = (Get-Content -Raw -Path $TokenFile).Trim()
} elseif (-not $Yes) {
    $Secure = Read-Host "Ingest token (the server's -ingest-token; leave empty if it has none)" -AsSecureString
    $Bstr = [System.Runtime.InteropServices.Marshal]::SecureStringToBSTR($Secure)
    try { $Token = [System.Runtime.InteropServices.Marshal]::PtrToStringBSTR($Bstr) }
    finally { [System.Runtime.InteropServices.Marshal]::ZeroFreeBSTR($Bstr) }
}

$Machine = Ask $Machine "Machine name" $env:COMPUTERNAME
$Interval = Ask $Interval "Push interval" "30s"
$ModelsRefresh = Ask $ModelsRefresh "Auto-update models.json interval (e.g. 24h, or 0 to disable)" "24h"
$RedactPaths = Ask $RedactPaths "Send project paths as (none, home, basename, hash)" "none"
$AutoUpdate = Ask $AutoUpdate "Auto-update dashd itself (e.g. 24h, or 0 to disable)" "0"

if ($ModelsRefresh -ne "0" -or -not (Test-Path $ModelsPath)) {
    Write-Host "Updating models.json..."
    & $ExePath update-models -models $ModelsPath
    if ($LASTEXITCODE -ne 0) { Write-Warning "Initial models update failed; using the bundled fallback." }
}

# The token goes in a file whose ACL grants only this user access (inheritance
# removed): the Windows equivalent of chmod 600. It is in neither the task's
# arguments nor an environment variable every process can read. (DPAPI would
# be stronger at rest, but an S4U task cannot decrypt user-scoped DPAPI data.)
$TaskArgs = @("push", "-remote", $Remote, "-machine", $Machine, "-interval", $Interval,
    "-models", $ModelsPath, "-models-refresh", $ModelsRefresh, "-state-db", $StatePath,
    "-redact-paths", $RedactPaths, "-auto-update", $AutoUpdate)
if ($Token) {
    Set-Content -Path $TokenPath -Value $Token -NoNewline -Encoding ascii
    $Acl = New-Object System.Security.AccessControl.FileSecurity
    $Acl.SetAccessRuleProtection($true, $false)
    $Rule = New-Object System.Security.AccessControl.FileSystemAccessRule($env:USERNAME, "FullControl", "Allow")
    $Acl.SetAccessRule($Rule)
    Set-Acl -Path $TokenPath -AclObject $Acl
    $TaskArgs += @("-ingest-token-file", $TokenPath)
}
# Clean up the plaintext environment variables earlier installers used.
[Environment]::SetEnvironmentVariable("DASHD_AUTH_TOKEN", $null, "User")

$Quoted = ($TaskArgs | ForEach-Object { '"' + ($_ -replace '"', '\"') + '"' }) -join " "
$Action = New-ScheduledTaskAction -Execute $ExePath -Argument $Quoted -WorkingDirectory $InstallDir
$Trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
# S4U runs the task without an interactive desktop, so no console window opens,
# and without storing the user's password.
$Principal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType S4U -RunLevel Limited
$Settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -ExecutionTimeLimit (New-TimeSpan -Days 0) -StartWhenAvailable -MultipleInstances IgnoreNew

Register-ScheduledTask -TaskName $TaskName -Action $Action -Trigger $Trigger -Principal $Principal -Settings $Settings -Force | Out-Null
Start-ScheduledTask -TaskName $TaskName

Write-Host ""
Write-Host "Scheduled task '$TaskName' registered and started (hidden; restarts on failure)."
Write-Host "Status: & '$ExePath' status -state-db '$StatePath'"
