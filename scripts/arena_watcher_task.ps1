# Register the Arena command-bus watcher as a Windows Scheduled Task.
#
# The watcher polls GitHub issues for commands ([CMD] ...) and runs them from a
# fixed whitelist. It must run as the interactive user: gh stores its token in
# the user's keyring, so a SYSTEM task cannot authenticate.
#
# Usage (no admin rights needed for a per-user task):
#   powershell -ExecutionPolicy Bypass -File scripts\arena_watcher_task.ps1 -Action register
#   powershell -ExecutionPolicy Bypass -File scripts\arena_watcher_task.ps1 -Action status
#   powershell -ExecutionPolicy Bypass -File scripts\arena_watcher_task.ps1 -Action unregister
#   powershell -ExecutionPolicy Bypass -File scripts\arena_watcher_task.ps1 -Action register -Force
#
# Rollback: -Action unregister. Nothing else changes; the git bridge, the Go
# code and the station do not depend on this task.

[CmdletBinding()]
param(
    [ValidateSet('register', 'unregister', 'status', 'run-now', 'stop')]
    [string]$Action = 'status',

    [string]$TaskName = 'ArenaWatcher',

    [string]$Python = '',

    [string]$Config = '.arena\watcher.json',

    [switch]$Force
)

$ErrorActionPreference = 'Stop'

$repo = Split-Path -Parent $PSScriptRoot
$script = Join-Path $repo 'scripts\arena_watcher.py'
$configPath = Join-Path $repo $Config

function Get-Task {
    Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
}

function Resolve-Python {
    if ($Python) { return $Python }
    $cmd = Get-Command python -ErrorAction SilentlyContinue
    if (-not $cmd) { throw 'python.exe not found in PATH; pass -Python <path>' }
    return $cmd.Source
}

switch ($Action) {
    'register' {
        if (-not (Test-Path $script)) { throw "watcher not found: $script" }
        if (-not (Test-Path $configPath)) { throw "config not found: $configPath (copy scripts\arena_watcher.example.json)" }

        $existing = Get-Task
        if ($existing -and -not $Force) {
            throw "task '$TaskName' already exists; use -Force to replace it"
        }

        $exe = Resolve-Python
        $user = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
        $taskAction = New-ScheduledTaskAction -Execute $exe `
            -Argument ('"{0}" --config "{1}"' -f $script, $configPath) `
            -WorkingDirectory $repo
        $trigger = New-ScheduledTaskTrigger -AtLogOn -User $user
        $settings = New-ScheduledTaskSettingsSet `
            -AllowStartIfOnBatteries `
            -DontStopIfGoingOnBatteries `
            -StartWhenAvailable `
            -MultipleInstances IgnoreNew `
            -RestartInterval (New-TimeSpan -Minutes 1) `
            -RestartCount 3 `
            -ExecutionTimeLimit ([TimeSpan]::Zero)
        $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType Interactive -RunLevel Limited

        if ($existing) { Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false }
        Register-ScheduledTask -TaskName $TaskName -Action $taskAction -Trigger $trigger `
            -Settings $settings -Principal $principal `
            -Description 'Arena command bus: runs whitelisted commands from GitHub issues' | Out-Null

        Write-Output "registered: $TaskName"
        Write-Output "  python: $exe"
        Write-Output "  script: $script"
        Write-Output "  config: $configPath"
        Write-Output "  log:    $(Join-Path $repo '.arena\watcher.log')"
        Write-Output 'start it now with: -Action run-now'
    }

    'unregister' {
        if (-not (Get-Task)) { Write-Output "no task named $TaskName"; break }
        Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
        Write-Output "unregistered: $TaskName"
    }

    'status' {
        $task = Get-Task
        if (-not $task) { Write-Output "no task named $TaskName"; break }
        $info = Get-ScheduledTaskInfo -TaskName $TaskName
        $task | Select-Object TaskName, State
        $info | Select-Object LastRunTime, LastTaskResult, NextRunTime
        $lock = Join-Path $repo '.arena\watcher.lock'
        if (Test-Path $lock) {
            # Windows refuses to read a file another process has locked; that
            # is the normal sign of a running watcher, not an error.
            try { Write-Output ("lock pid: " + (Get-Content $lock -Raw).Trim()) }
            catch { Write-Output 'lock: held by a running watcher' }
        }
        else { Write-Output 'lock: absent' }
        $paused = Join-Path $repo '.arena\paused'
        Write-Output ("paused: " + (Test-Path $paused))
    }

    'run-now' {
        if (-not (Get-Task)) { throw "task '$TaskName' is not registered" }
        Start-ScheduledTask -TaskName $TaskName
        Start-Sleep -Seconds 2
        Get-ScheduledTask -TaskName $TaskName | Select-Object TaskName, State
    }

    'stop' {
        $task = Get-Task
        if ($task) { Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue }
        Write-Output "stopped: $TaskName (create .arena\paused to keep it idle)"
    }
}
