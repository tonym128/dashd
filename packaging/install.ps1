# Interactive installer for the dashd push agent on Windows.
# Downloads the latest release from GitHub, prompts for configuration,
# updates models.json, and registers a Task Scheduler background task.

param(
    [switch]$Upgrade,
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"

$Repo = "tonym128/dashd"
$InstallDir = Join-Path $env:LOCALAPPDATA "dashd"
$ModelsPath = Join-Path $InstallDir "models.json"
$DbPath = Join-Path $InstallDir "push-state.db"
$ExePath = Join-Path $InstallDir "dashd.exe"
$TaskName = "dashd-push"

if ($Uninstall) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    if (Test-Path $ExePath) { Remove-Item -Force $ExePath }
    Write-Host "Uninstalled '$TaskName' scheduled task and $ExePath. Database ($DbPath) and environment variables were preserved."
    exit 0
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

Write-Host "Fetching latest release metadata from GitHub ($Repo)..."
$Release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{ "User-Agent" = "dashd-installer" }
$Tag = $Release.tag_name
$Ver = $Tag.TrimStart("v")
$Archive = "dashd_${Ver}_windows_amd64.zip"
$Url = "https://github.com/$Repo/releases/download/$Tag/$Archive"

$TmpZip = Join-Path $env:TEMP $Archive
$TmpExtract = Join-Path $env:TEMP "dashd-extract-$Ver"
if (Test-Path $TmpExtract) { Remove-Item -Recurse -Force $TmpExtract }

Write-Host "Downloading $Url..."
Invoke-WebRequest -Uri $Url -OutFile $TmpZip
Expand-Archive -Path $TmpZip -DestinationPath $TmpExtract -Force

$ExtractedDir = Join-Path $TmpExtract "dashd_${Ver}_windows_amd64"
Copy-Item -Force (Join-Path $ExtractedDir "dashd.exe") $ExePath
if (Test-Path (Join-Path $ExtractedDir "models.json")) {
    Copy-Item -Force (Join-Path $ExtractedDir "models.json") $ModelsPath
}
Remove-Item -Force $TmpZip
Remove-Item -Recurse -Force $TmpExtract
Write-Host "Installed dashd $Tag to $ExePath"

if ($Upgrade) {
    try {
        Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
        Start-ScheduledTask -TaskName $TaskName
        Write-Host "Upgraded dashd binary and restarted '$TaskName' (kept existing configuration)."
        exit 0
    } catch {
        Write-Warning "Could not restart existing task; continuing with interactive setup."
    }
}

Write-Host ""
Write-Host "=== dashd Agent Configuration ==="
do {
    $RemoteUrl = Read-Host "Remote dashd server URL (e.g. https://dashd.example.com)"
} while ([string]::IsNullOrWhiteSpace($RemoteUrl))

$SecureToken = Read-Host "Auth token (leave empty if server has no -auth-token)" -AsSecureString
$Bstr = [System.Runtime.InteropServices.Marshal]::SecureStringToBSTR($SecureToken)
try {
    $AuthToken = [System.Runtime.InteropServices.Marshal]::PtrToStringAuto($Bstr)
} finally {
    [System.Runtime.InteropServices.Marshal]::ZeroFreeBSTR($Bstr)
}

$DefaultMachine = $env:COMPUTERNAME
$MachineName = Read-Host "Machine name [$DefaultMachine]"
if ([string]::IsNullOrWhiteSpace($MachineName)) { $MachineName = $DefaultMachine }

$PushInterval = Read-Host "Push interval [30s]"
if ([string]::IsNullOrWhiteSpace($PushInterval)) { $PushInterval = "30s" }

$ModelsRefresh = Read-Host "Auto-update models.json interval (e.g. 24h, or 0 to disable) [24h]"
if ([string]::IsNullOrWhiteSpace($ModelsRefresh)) { $ModelsRefresh = "24h" }

if ($ModelsRefresh -ne "0" -or -not (Test-Path $ModelsPath)) {
    Write-Host "Updating models.json..."
    try {
        & $ExePath update-models -models $ModelsPath
    } catch {
        Write-Warning "Initial models update failed; using bundled fallback."
    }
}

# Store credentials in User environment variables so they do not appear in task arguments.
[Environment]::SetEnvironmentVariable("DASHD_REMOTE", $RemoteUrl, "User")
[Environment]::SetEnvironmentVariable("DASHD_AUTH_TOKEN", $AuthToken, "User")

$TaskName = "dashd-push"
$Args = "push -machine `"$MachineName`" -interval $PushInterval -models `"$ModelsPath`" -models-refresh $ModelsRefresh -db `"$DbPath`""

$Action = New-ScheduledTaskAction -Execute $ExePath -Argument $Args
$Trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$Settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit (New-TimeSpan -Days 0)

Register-ScheduledTask -TaskName $TaskName -Action $Action -Trigger $Trigger -Settings $Settings -Force | Out-Null
Start-ScheduledTask -TaskName $TaskName

Write-Host ""
Write-Host "Scheduled Task '$TaskName' registered and started!"
Write-Host "Manage via Task Scheduler or: Get-ScheduledTask -TaskName $TaskName"
