#Requires -Version 7.0
param(
    [Parameter(Mandatory)][string]$Executable,
    [switch]$VerifyRestart,
    [string]$TaskScript=(Join-Path $PSScriptRoot 'pulse-task.ps1')
)

$ErrorActionPreference = 'Stop'
if (-not $IsWindows) { throw '此验收仅适用于 Windows' }
$binary = (Get-Item -LiteralPath $Executable).FullName
$scriptPath = (Get-Item -LiteralPath $TaskScript).FullName
$fixture = Join-Path (Split-Path $PSScriptRoot -Parent) ('.cache/task-acceptance-' + [guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $fixture -Force | Out-Null
$database = Join-Path $fixture 'test monitor.db'
$backup = Join-Path $fixture 'backup.zip'
$profile = 'verify-' + [guid]::NewGuid().ToString('N').Substring(0, 20)
$reservation = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
$reservation.Start()
$port = $reservation.LocalEndpoint.Port
$reservation.Stop()
$base = "http://127.0.0.1:$port"

function Wait-For([scriptblock]$Read, [int]$Seconds, [string]$Description) {
    $deadline = [Diagnostics.Stopwatch]::StartNew()
    while ($deadline.Elapsed.TotalSeconds -lt $Seconds) {
        $value = & $Read
        if ($value) { return $value }
        Start-Sleep -Milliseconds 250
    }
    throw "未等到$Description"
}

function Read-Pulse {
    try { Invoke-RestMethod "$base/api/status" -TimeoutSec 2 -ErrorAction Stop } catch { $null }
}

$installed = $null
$report = [ordered]@{}
try {
    $installed = & $scriptPath Install -Profile $profile -Executable $binary -Database $database -Port $port -DesktopAlerts
    if ($installed.TaskName -notlike 'Pulse-*') { throw '未安装预期的隔离任务' }
    $system = New-Object -ComObject Schedule.Service
    $system.Connect()
    $folder = $system.GetFolder('\')
    $otherProfile = 'verify-' + [guid]::NewGuid().ToString('N').Substring(0,20)
    $otherName = (& $scriptPath Status -Profile $otherProfile).TaskName
    $other = $system.NewTask(0)
    $userId = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
    $other.RegistrationInfo.Source = 'Unmanaged synthetic conflict fixture'
    $other.Principal.UserId = $userId
    $other.Principal.LogonType = 3
    $otherAction = $other.Actions.Create(0)
    $otherAction.Path = $binary
    $otherAction.Arguments = 'serve -h'
    $null = $folder.RegisterTaskDefinition($otherName,$other,2,$userId,$null,3,$null)
    try {
        foreach ($operation in 'Start','Stop','Remove') {
            try {
                & $scriptPath $operation -Profile $otherProfile | Out-Null
                throw '意外操作了同名的非 Pulse 任务'
            } catch { if ($_.Exception.Message -notmatch '拒绝操作') { throw } }
        }
        if ($folder.GetTask($otherName).Definition.RegistrationInfo.Source -ne $other.RegistrationInfo.Source) { throw '冲突任务被更改' }
        $report.unmanaged_task_untouched = $true
    } finally { $folder.DeleteTask($otherName,0) }
    $ticket = $folder.GetTask($installed.TaskName)
    $definition = $ticket.Definition
    $trigger = $definition.Triggers.Item(1)
    $action = $definition.Actions.Item(1)
    if ($definition.Triggers.Count -ne 1 -or $trigger.Type -ne 9 -or -not $trigger.Enabled -or
        $definition.Principal.LogonType -ne 3 -or $definition.Principal.RunLevel -ne 0 -or
        $definition.Settings.ExecutionTimeLimit -ne 'PT0S' -or $definition.Settings.MultipleInstances -ne 2 -or
        $definition.Settings.DisallowStartIfOnBatteries -or $definition.Settings.StopIfGoingOnBatteries -or
        $action.Path -ine $binary -or $action.WorkingDirectory -ine (Split-Path $binary -Parent) -or
        -not $action.Arguments.StartsWith('task-supervise ') -or
        -not $action.Arguments.Contains('"' + $database + '"') -or -not $action.Arguments.Contains('-desktop-alerts')) {
        throw '任务定义与登录触发、权限级别或启动参数不符'
    }
    $report.logon_trigger_configuration = $true
    try {
        & $scriptPath Install -Profile $profile -Executable $binary -Database $database -Port $port | Out-Null
        throw '任务安装意外覆盖了现有配置'
    } catch {
        if ($_.Exception.Message -notmatch '同名任务已存在') { throw }
    }
    & $scriptPath Start -Profile $profile | Out-Null
    $first = Wait-For { $state = Read-Pulse; if ($state.monitor_health.instance_id) { $state } } 20 'Pulse 启动'
    $instance = $first.monitor_health.instance_id
    $payload = @{ name='task test (synthetic)'; url="$base/api/status"; enabled=$true; interval_seconds=60; timeout_seconds=5 } | ConvertTo-Json
    $target = Invoke-RestMethod "$base/api/targets" -Method Post -ContentType application/json -Body $payload
    $oldCheck = Wait-For { ((Invoke-RestMethod "$base/api/targets/$($target.id)/checks").items | Select-Object -First 1) } 15 '首条监测结果'
    & $scriptPath Start -Profile $profile | Out-Null
    $same = Read-Pulse
    if ($ticket.GetInstances(0).Count -ne 1 -or $same.monitor_health.instance_id -ne $instance) { throw '重复触发启动了其他实例' }
    $report.duplicate_ignored = $true
    if ($VerifyRestart) {
        $listener = Get-NetTCPConnection -LocalAddress '127.0.0.1' -LocalPort $port -State Listen -ErrorAction Stop
        $proc = Get-CimInstance Win32_Process -Filter "ProcessId=$($listener.OwningProcess)" -ErrorAction Stop
        if ($proc.ExecutablePath -ine $binary -or -not $proc.CommandLine.Contains($database) -or -not $proc.CommandLine.Contains('-windows-stop-event')) { throw '拒绝中断非本次测试进程' }
        $procHandle = Get-Process -Id $proc.ProcessId -ErrorAction Stop
        [void]$procHandle.Handle
        Stop-Process -Id $proc.ProcessId -Force -ErrorAction Stop
        $procHandle.WaitForExit(3000) | Out-Null
        "中断后的进程退出码 $($procHandle.ExitCode)" | Write-Verbose
        $elapsed = [Diagnostics.Stopwatch]::StartNew()
        try { $null = Wait-For { $st = Read-Pulse; if ($st.monitor_health.instance_id -and $st.monitor_health.instance_id -ne $instance) { $st } } 110 '系统在异常退出后自动重启' }
        catch { throw "$($_.Exception.Message)（进程退出码 $($procHandle.ExitCode)，任务状态 $($ticket.State)，任务结果 $($ticket.LastTaskResult)）" }
        finally { $procHandle.Dispose() }
        $report.restarted_after_abnormal_exit = $true
        $report.restart_wait_seconds = [Math]::Round($elapsed.Elapsed.TotalSeconds, 2)
        $latest = (Invoke-RestMethod "$base/api/targets/$($target.id)/checks").items
        if ($oldCheck.id -notin $latest.id) { throw '重启后找不到原检查记录' }
    }
    $stopped = & $scriptPath Stop -Profile $profile
    if ($stopped.Running -or $stopped.Enabled -or $stopped.LastResult -ne 0 -or (Read-Pulse)) { throw '任务未正常退出、暂停触发并释放端口' }
    & $binary maintain backup -db $database -out $backup | Out-Null
    if ($LASTEXITCODE -ne 0) { throw '正常停止后备份失败' }
    & $binary maintain verify -in $backup | Out-Null
    if ($LASTEXITCODE -ne 0) { throw '备份验证失败' }
    $report.graceful_exit_and_backup = $true
    $controlLock = [Threading.Mutex]::new($false,'Global\PulseTaskManagement.' + $installed.TaskName)
    $locked = $controlLock.WaitOne([TimeSpan]::FromSeconds(4))
    if (-not $locked) { $controlLock.Dispose(); throw '测试无法获得任务管理锁' }
    $nextStart = $null
    try {
        $nextStart = Start-Job -ArgumentList $scriptPath,$profile -ScriptBlock {
            param($ControlScript,$TaskProfile)
            '尝试并发启动'
            & $ControlScript Start -Profile $TaskProfile
        }
        try {
            $null = Wait-For { '尝试并发启动' -in @(Receive-Job $nextStart -Keep -ErrorAction Stop) } 10 '其他进程尝试启动'
            Start-Sleep -Milliseconds 400
            if ($folder.GetTask($installed.TaskName).Enabled -or $nextStart.State -ne 'Running') { throw '并发 Start 绕过管理锁' }
        } finally { $controlLock.ReleaseMutex(); $locked = $false }
        $null = Wait-Job $nextStart -Timeout 10
        if ($nextStart.State -ne 'Completed') { throw '管理锁释放后 Start 未完成' }
        Receive-Job $nextStart -ErrorAction Stop | Out-Null
    } finally {
        if ($locked) { $controlLock.ReleaseMutex() }
        $controlLock.Dispose()
        if ($null -ne $nextStart) { Remove-Job $nextStart -Force }
    }
    $report.management_operations_serialized = $true
    $null = Wait-For { (Read-Pulse).monitor_health.instance_id } 20 '维护后重新启动'
    if (-not (& $scriptPath Status -Profile $profile).Enabled) { throw 'Start 未恢复已暂停的登录任务' }
    $report.start_reenabled_task = $true
} finally {
    if ($installed) {
        & $scriptPath Remove -Profile $profile | Out-Null
        $current = & $scriptPath Status -Profile $profile
        if ($current.Installed) { throw '测试任务未清理' }
        $report.temporary_task_removed = $true
    }
}
$report | ConvertTo-Json
