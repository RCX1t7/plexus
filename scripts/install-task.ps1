# Registers Plexus to start at logon for the current Windows user.
# Run it yourself (or via `plexus.exe install-task`); Plexus never runs it
# on its own. No administrator rights are needed: the task runs as you, with
# limited privileges, only while you are logged on.
#
#   powershell -NoProfile -ExecutionPolicy Bypass -File install-task.ps1 -Exe "C:\Tools\plexus.exe"
#   powershell -NoProfile -ExecutionPolicy Bypass -File install-task.ps1 -Uninstall
param(
    [string]$Exe = (Join-Path $PSScriptRoot "plexus.exe"),
    [string]$TaskName = "Plexus",
    [switch]$Uninstall
)
$ErrorActionPreference = "Stop"

if ($Uninstall) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
    Write-Host "Removed scheduled task '$TaskName'."
    return
}

$Exe = (Resolve-Path -LiteralPath $Exe).Path
$user = "$env:USERDOMAIN\$env:USERNAME"
$action = New-ScheduledTaskAction -Execute $Exe -Argument "run --hidden" -WorkingDirectory (Split-Path $Exe)
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
    -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
    -MultipleInstances IgnoreNew -StartWhenAvailable
$principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings `
    -Principal $principal -Description "Plexus: Slack bots for the coding agents on this PC" -Force | Out-Null
Write-Host "Registered '$TaskName': $Exe run --hidden, at logon of $user."
Write-Host "Start it now with: Start-ScheduledTask -TaskName $TaskName"
