#Requires -Version 7.0
param(
    [Parameter(Mandatory,Position=0)][ValidateSet('Install','Status','Start','Stop','Remove')][string]$Action,
    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9-]{0,31}$')][string]$Profile = 'default',
    [string]$Executable = (Join-Path $PSScriptRoot 'go-monitor.exe'),
    [string]$Database = 'data/monitor.db',
    [ValidateRange(1,65535)][int]$Port = 8080,
    [switch]$DesktopAlerts
)

$ErrorActionPreference = 'Stop'
if (-not $IsWindows) { throw 'Pulse 计划任务仅适用于 Windows' }
if ($Action -ne 'Install') {
    foreach ($option in 'Executable','Database','Port','DesktopAlerts') {
        if ($PSBoundParameters.ContainsKey($option)) { throw "参数 -$option 仅在 Install 时使用" }
    }
}

$account = [Security.Principal.WindowsIdentity]::GetCurrent()
try { $sid = $account.User.Value } finally { $account.Dispose() }
$digest = [Security.Cryptography.SHA256]::Create()
try { $identityHash = $digest.ComputeHash([Text.Encoding]::UTF8.GetBytes($sid)) } finally { $digest.Dispose() }
$identityPrefix = [BitConverter]::ToString($identityHash, 0, 8).Replace('-', '').ToLowerInvariant()
$taskName = 'Pulse-' + $identityPrefix + '-' + $Profile.ToLowerInvariant()
$source = 'Pulse Go Monitor interactive task v1'
$scheduler = New-Object -ComObject Schedule.Service
$scheduler.Connect()
$folder = $scheduler.GetFolder('\')

function Find-Task {
    try { $folder.GetTask($taskName) }
    catch {
        if ($_.Exception.GetBaseException().HResult -ne -2147024894) { throw }
    }
}

function Get-TaskStopToken($Task) {
    $definition = $Task.Definition
    $taskUser = [string]$definition.Principal.UserId
    if ($taskUser -notmatch '^S-1-') {
        try { $taskUser = [Security.Principal.NTAccount]::new($taskUser).Translate([Security.Principal.SecurityIdentifier]).Value }
        catch { throw '无法核对任务所属用户' }
    }
    if ($definition.RegistrationInfo.Source -cne $source -or $taskUser -ne $sid -or
        $definition.Principal.LogonType -ne 3 -or $definition.Principal.RunLevel -ne 0 -or $definition.Actions.Count -ne 1) {
        throw '现有任务不符合当前用户的 Pulse 配置，拒绝操作'
    }
    $exec = $definition.Actions.Item(1)
    if ($exec.Type -ne 0 -or [IO.Path]::GetFileName($exec.Path) -ine 'go-monitor.exe' -or $exec.Arguments -cnotmatch '^task-supervise .+ -windows-stop-event ([a-f0-9]{32})$') {
        throw '现有任务没有可识别的 Pulse 启动参数，拒绝操作'
    }
    $Matches[1]
}

function Test-ActiveTask($Task) {
    $Task.GetInstances(0).Count -gt 0 -or [int]$Task.State -in 2,4
}

function Read-PulseTask($Task) {
    if ($null -eq $Task) {
        return [pscustomobject]@{ TaskName=$taskName; Installed=$false; Running=$false; Enabled=$false; State='NotInstalled'; LastResult=$null }
    }
    $states = 'Unknown','Disabled','Queued','Ready','Running'
    [pscustomobject]@{
        TaskName=$taskName; Installed=$true; Running=(Test-ActiveTask $Task); Enabled=[bool]$Task.Enabled
        State=$states[[int]$Task.State]; LastResult=[int]$Task.LastTaskResult
    }
}

function Stop-PulseTask($Task, [string]$Token) {
    $wasActive = Test-ActiveTask $Task
    $waiting = [Diagnostics.Stopwatch]::StartNew()
    $signalled = $false
    while (Test-ActiveTask $Task) {
        if ($waiting.Elapsed.TotalSeconds -ge 18) {
            throw '18 秒内无法确认 Pulse 已正常退出。请在同一 Windows 登录会话查询 Status，确认停服后再维护数据库。'
        }
        if (-not $signalled) {
            $event = $null
            try { $event = [Threading.EventWaitHandle]::OpenExisting('Local\PulseStop.' + $Token) }
            catch { if ($_.Exception.GetBaseException() -isnot [Threading.WaitHandleCannotBeOpenedException]) { throw } }
            if ($null -ne $event) {
                try { $signalled = $event.Set() } finally { $event.Dispose() }
            }
        }
        Start-Sleep -Milliseconds 150
    }
    if ($wasActive -and [int]$Task.LastTaskResult -ne 0) {
        throw "Pulse 已退出，但任务结果为 $($Task.LastTaskResult)。请检查状态后再维护数据库。"
    }
}

function Get-LocalPath([string]$Path, [string]$Base) {
    $resolved = [IO.Path]::GetFullPath($Path, $Base)
    if ($resolved -notmatch '^[A-Za-z]:\\' -or $resolved -match '[%"\r\n]') { throw '路径须位于本机盘符下，且不能包含百分号、引号或换行' }
    $resolved
}

function Invoke-PulseTask {
    $task = Find-Task
    if ($Action -eq 'Install') {
        if ($null -ne $task) { throw '同名任务已存在，请先查询 Status；安装不会覆盖现有任务' }
        $program = Get-Item -LiteralPath (Get-LocalPath $Executable $PWD.ProviderPath)
        if ($program.PSIsContainer -or $program.Name -ine 'go-monitor.exe') { throw '请选择打包后的 go-monitor.exe' }
        $databasePath = Get-LocalPath $Database $program.DirectoryName
        if ([string]::IsNullOrWhiteSpace([IO.Path]::GetFileName($databasePath)) -or (Test-Path -LiteralPath $databasePath -PathType Container)) {
            throw '数据库路径必须指向文件'
        }
        $helpText = & $program.FullName task-supervise -h 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0 -or $helpText -notmatch '(?m)^\s+-windows-stop-event\b') { throw '此版本的 Pulse 不支持计划任务正常停止' }
        $token = [Guid]::NewGuid().ToString('N')
        $extra = if ($DesktopAlerts) { ' -desktop-alerts' } else { '' }
        $definition = $scheduler.NewTask(0)
        $definition.RegistrationInfo.Source = $source
        $definition.RegistrationInfo.Description = 'Pulse 当前用户登录后启动的本机监测任务'
        $definition.Principal.UserId = $sid
        $definition.Principal.LogonType = 3 # TASK_LOGON_INTERACTIVE_TOKEN
        $definition.Principal.RunLevel = 0 # TASK_RUNLEVEL_LUA
        $settings = $definition.Settings
        $settings.ExecutionTimeLimit = 'PT0S'
        $settings.MultipleInstances = 2 # TASK_INSTANCES_IGNORE_NEW
        $settings.DisallowStartIfOnBatteries = $false
        $settings.StopIfGoingOnBatteries = $false
        $settings.WakeToRun = $false
        $trigger = $definition.Triggers.Create(9) # TASK_TRIGGER_LOGON
        $trigger.UserId = $sid
        $trigger.Enabled = $true
        $exec = $definition.Actions.Create(0) # TASK_ACTION_EXEC
        $exec.Path = $program.FullName
        $exec.WorkingDirectory = $program.DirectoryName
        $exec.Arguments = 'task-supervise -addr 127.0.0.1:' + $Port + ' -db "' + $databasePath + '"' + $extra + ' -windows-stop-event ' + $token
        $task = $folder.RegisterTaskDefinition($taskName, $definition, 2, $sid, $null, 3, $null) # TASK_CREATE
        Read-PulseTask $task
        return
    }

    if ($null -eq $task) {
        if ($Action -in 'Start','Stop') { throw '任务尚未安装' }
        Read-PulseTask $null
        return
    }
    $token = Get-TaskStopToken $task
    switch ($Action) {
        'Status' { Read-PulseTask $task }
        'Start' {
            if (-not $task.Enabled) {
                if (Test-ActiveTask $task) { throw '任务仍在停止，请等 Status 显示 Running=False 后再启动' }
                $task.Enabled = $true
            }
            if (-not (Test-ActiveTask $task)) { [void]$task.Run($null) }
            Read-PulseTask $task
        }
        'Stop' {
            $task.Enabled = $false
            Stop-PulseTask $task $token
            Read-PulseTask $task
        }
        'Remove' {
            $task.Enabled = $false
            Stop-PulseTask $task $token
            $current = Find-Task
            if ($null -ne $current -and ((Get-TaskStopToken $current) -cne $token -or (Test-ActiveTask $current) -or $current.Enabled)) {
                throw '任务在停止期间发生变化，拒绝删除当前任务'
            }
            if ($null -ne $current) { $folder.DeleteTask($taskName, 0) }
            Read-PulseTask $null
        }
    }
}

$mutex = [Threading.Mutex]::new($false, 'Global\PulseTaskManagement.' + $taskName)
$locked = $false
try {
    try { $locked = $mutex.WaitOne([TimeSpan]::FromSeconds(25)) }
    catch [Threading.AbandonedMutexException] { $locked = $true }
    if (-not $locked) { throw '另一条命令正在管理此任务，请稍后再试' }
    Invoke-PulseTask
} finally {
    if ($locked) { $mutex.ReleaseMutex() }
    $mutex.Dispose()
}
