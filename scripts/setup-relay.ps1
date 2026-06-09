# One-command InternetMerge relay setup for Windows.
# Prefers Docker when available; otherwise installs the binary and registers a
# scheduled task that runs the relay at logon.
#   irm <raw-url>/scripts/setup-relay.ps1 | iex
#   .\setup-relay.ps1 -Native   # force native install
param(
  [switch]$Docker,
  [switch]$Native
)
$ErrorActionPreference = "Stop"
$Repo = "dikeckaan/internetmerge"
$Port = "7000"

function New-Key {
  $bytes = New-Object byte[] 32
  [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
  [Convert]::ToBase64String($bytes)
}

function Write-Conn($key) {
  $ip = try { (Invoke-RestMethod -Uri "https://api.ipify.org") } catch { "YOUR_SERVER_IP" }
  Write-Host ""
  Write-Host "Relay is up. Paste this into InternetMerge:"
  Write-Host "  Address: ${ip}:$Port"
  Write-Host "  Key:     $key"
  Write-Host ""
  Write-Host "Open TCP port $Port in Windows Firewall."
}

function Setup-Docker {
  Write-Host "Using Docker."
  $key = if ($env:INTERNETMERGE_RELAY_KEY) { $env:INTERNETMERGE_RELAY_KEY } else { New-Key }
  docker rm -f internetmerge-relay 2>$null | Out-Null
  docker run -d --name internetmerge-relay --restart unless-stopped `
    -p "${Port}:7000" -e "INTERNETMERGE_RELAY_KEY=$key" `
    ghcr.io/dikeckaan/internetmerge:latest
  Write-Conn $key
}

function Setup-Native {
  $arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }
  $asset = "internetmerge-windows-$arch.exe"
  $dest = "$env:ProgramFiles\InternetMerge\internetmerge.exe"
  New-Item -ItemType Directory -Force -Path (Split-Path $dest) | Out-Null
  Invoke-WebRequest -Uri "https://github.com/$Repo/releases/latest/download/$asset" -OutFile $dest
  $key = if ($env:INTERNETMERGE_RELAY_KEY) { $env:INTERNETMERGE_RELAY_KEY } else { New-Key }
  # Run the relay at logon via a scheduled task. ScheduledTaskAction has no
  # environment-variable parameter, so pass the key through a cmd.exe wrapper.
  $cmdArgs = "/c set INTERNETMERGE_RELAY_KEY=$key && `"$dest`" relay --listen :$Port"
  $action  = New-ScheduledTaskAction -Execute "cmd.exe" -Argument $cmdArgs
  $trigger = New-ScheduledTaskTrigger -AtLogOn
  $settings = New-ScheduledTaskSettingsSet -StartWhenAvailable
  Register-ScheduledTask -TaskName "InternetMergeRelay" -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
  Start-ScheduledTask -TaskName "InternetMergeRelay"
  Write-Conn $key
}

if ($Native) { Setup-Native }
elseif ($Docker) { Setup-Docker }
elseif (Get-Command docker -ErrorAction SilentlyContinue) { Setup-Docker }
else { Setup-Native }
