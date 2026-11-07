#Requires -Version 7.0
param(
    [ValidateSet('Start', 'Seed', 'Stop')][string]$Action = 'Start',
    [ValidateRange(1024, 65535)][int]$Port = 18082,
    [ValidateRange(1024, 65535)][int]$FixturePort = 18083
)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path $PSScriptRoot -Parent
$sessionPath = Join-Path $projectRoot '.cache/page-acceptance-session.json'
$nodeExe = (Get-Command node -ErrorAction Stop).Source

function Get-OwnedProcess($record, [string]$expectedPath, [switch]$AllowMissing) {
    $process = Get-Process -Id $record.pid -ErrorAction SilentlyContinue
    if ($null -eq $process) {
        if ($AllowMissing) { return $null }
        throw "验收进程 $($record.pid) 已退出，拒绝继续写入。"
    }
    if ($process.Path -ne $expectedPath -or $process.StartTime.ToUniversalTime().Ticks.ToString() -ne $record.startedTicks) {
        throw "PID $($record.pid) 的程序路径或启动时间与记录不符，拒绝操作。"
    }
    return $process
}
function Stop-OwnedProcess($record, [string]$expectedPath) {
    $process = Get-OwnedProcess $record $expectedPath -AllowMissing
    if ($null -eq $process) { return }
    Stop-Process -Id $process.Id
    $process.WaitForExit()
}
function Process-Record($process, [string]$path) {
    [pscustomobject]@{ pid = $process.Id; path = $path; startedTicks = $process.StartTime.ToUniversalTime().Ticks.ToString() }
}
function Wait-Ready([string]$url) {
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        try { $null = Invoke-RestMethod $url -TimeoutSec 1; return } catch { Start-Sleep -Milliseconds 100 }
    }
    throw "服务未就绪：$url"
}
if ($Action -ne 'Start') {
    $session = Get-Content -LiteralPath $sessionPath -Raw | ConvertFrom-Json
    $allowedCache = [IO.Path]::GetFullPath((Join-Path $projectRoot '.cache')) + [IO.Path]::DirectorySeparatorChar
    $monitorPath = [IO.Path]::GetFullPath($session.monitor.path)
    if (-not $monitorPath.StartsWith($allowedCache, [StringComparison]::OrdinalIgnoreCase) -or [IO.Path]::GetFileName($monitorPath) -ne 'browser-acceptance.exe') {
        throw '验收程序路径不属于项目缓存，拒绝操作。'
    }
    if ($Action -eq 'Stop') {
        Stop-OwnedProcess $session.monitor $monitorPath
        Stop-OwnedProcess $session.fixture $nodeExe
        Remove-Item -LiteralPath $sessionPath
        Write-Output "已停止验收进程，数据库和日志保留于 $($session.directory)"
        return
    }
    # Populate via the real HTTP checker and API, never by writing fabricated rows.
    $base = "http://127.0.0.1:$($session.port)"
    $null = Get-OwnedProcess $session.monitor $monitorPath
    $null = Get-OwnedProcess $session.fixture $nodeExe
    Wait-Ready "$base/api/status"
    $inputBody = @{name = '验收 · 分页样本'; url = "http://127.0.0.1:$($session.fixturePort)/ok"; interval_seconds = 60; timeout_seconds = 1; enabled = $false} | ConvertTo-Json
    $null = Get-OwnedProcess $session.monitor $monitorPath
    $null = Get-OwnedProcess $session.fixture $nodeExe
    $target = Invoke-RestMethod "$base/api/targets" -Method Post -ContentType 'application/json' -Body ([Text.Encoding]::UTF8.GetBytes($inputBody))
    for ($index = 1; $index -le 55; $index++) {
        $deadline = [DateTime]::UtcNow.AddSeconds(5)
        do {
            $null = Get-OwnedProcess $session.monitor $monitorPath
            $null = Get-OwnedProcess $session.fixture $nodeExe
            try {
                $null = Invoke-RestMethod "$base/api/targets/$($target.id)/check" -Method Post
                break
            } catch {
                # Only a rejected (409) trigger is safe to retry without adding
                # an accidental extra sample after an ambiguous network error.
                if ($_.Exception.Response.StatusCode -ne [Net.HttpStatusCode]::Conflict -or [DateTime]::UtcNow -ge $deadline) { throw }
                Start-Sleep -Milliseconds 20
            }
        } while ($true)
        $deadline = [DateTime]::UtcNow.AddSeconds(5)
        do {
            $history = Invoke-RestMethod "$base/api/targets/$($target.id)/checks?limit=200"
            $currentTargets = Invoke-RestMethod "$base/api/targets"
            $current = $currentTargets | Where-Object { $_.id -eq $target.id }
            if (@($history.items).Count -ge $index -and $null -ne $current -and -not $current.running) { break }
            if ([DateTime]::UtcNow -gt $deadline) { throw "第 $index 条历史未能完成" }
            Start-Sleep -Milliseconds 20
        } while ($true)
    }
    Write-Output "已创建暂停目标 ID=$($target.id)，含55条真实检查记录。Seed每执行一次会创建一个新目标。"
    return
}
if ($Port -eq $FixturePort) { throw '监测端口与测试接口端口必须不同。' }
if (Test-Path -LiteralPath $sessionPath) { throw '已有验收会话记录，请先执行 -Action Stop；不要覆盖运行中的会话。' }
foreach ($candidatePort in @($Port, $FixturePort)) {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, $candidatePort)
    try { $listener.Start() } finally { $listener.Stop() }
}
$directory = Join-Path $projectRoot ('.cache/page-acceptance-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $directory | Out-Null
$exe = Join-Path $directory 'browser-acceptance.exe'
& (Join-Path $projectRoot 'go.ps1') build -buildvcs=false -o $exe .
if ($LASTEXITCODE -ne 0) { throw '验收程序构建失败。' }
$fixture = $null
$monitor = $null
try {
    $fixtureScript = Join-Path $PSScriptRoot 'browser-fixture.mjs'
    $fixture = Start-Process -FilePath $nodeExe -ArgumentList @(('"' + $fixtureScript + '"'), "$FixturePort") -WindowStyle Hidden -WorkingDirectory $projectRoot -RedirectStandardOutput (Join-Path $directory 'fixture-out.log') -RedirectStandardError (Join-Path $directory 'fixture-err.log') -PassThru
    $fixtureRecord = Process-Record $fixture $nodeExe
    Wait-Ready "http://127.0.0.1:$FixturePort/ok"
    $db = Join-Path $directory 'monitor.db'
    $monitor = Start-Process -FilePath $exe -ArgumentList @('serve', '-addr', "127.0.0.1:$Port", '-db', ('"' + $db + '"')) -WindowStyle Hidden -WorkingDirectory $projectRoot -RedirectStandardOutput (Join-Path $directory 'monitor-out.log') -RedirectStandardError (Join-Path $directory 'monitor-err.log') -PassThru
    $monitorRecord = Process-Record $monitor $exe
    Wait-Ready "http://127.0.0.1:$Port/api/status"
    [pscustomobject]@{directory = $directory; port = $Port; fixturePort = $FixturePort; monitor = $monitorRecord; fixture = $fixtureRecord} | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $sessionPath -Encoding utf8
    Write-Output "验收页面：http://127.0.0.1:$Port；上游：http://127.0.0.1:$FixturePort/ok、/error、/slow、/redirect"
    Write-Output '初始数据库为空。-Action Seed 生成分页样本；-Action Stop 停止本轮进程。'
} catch {
    if ($null -ne $monitor -and -not $monitor.HasExited) { Stop-OwnedProcess $monitorRecord $exe }
    if ($null -ne $fixture -and -not $fixture.HasExited) { Stop-OwnedProcess $fixtureRecord $nodeExe }
    throw
}
