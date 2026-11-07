#Requires -Version 7.0
param(
    [ValidateRange(1024, 65535)][int]$Port = 18084,
    [ValidateRange(1024, 65535)][int]$FixturePort = 18085
)
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path $PSScriptRoot -Parent
$scriptPath = Join-Path $PSScriptRoot 'page-acceptance.ps1'
$sessionPath = Join-Path $projectRoot '.cache/page-acceptance-session.json'
$base = "http://127.0.0.1:$Port"
if (Test-Path -LiteralPath $sessionPath) { throw '请先停止已有脚本验收会话；测试不覆盖已有会话记录。' }
$failures = @()
foreach ($scenario in @('stale identity', 'busy slots')) {
    $originalSession = $null
    try {
        & $scriptPath -Action Start -Port $Port -FixturePort $FixturePort
        if ($scenario -eq 'stale identity') {
            $originalSession = [IO.File]::ReadAllBytes($sessionPath)
            $session = Get-Content -LiteralPath $sessionPath -Raw | ConvertFrom-Json
            $session.monitor.startedTicks = '0'
            $session | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath $sessionPath -Encoding utf8
            $rejected = $false
            try { & $scriptPath -Action Seed } catch { $rejected = $true }
            if (-not $rejected) { throw 'Seed accepted a stale process identity and wrote test data' }
            if ((Invoke-WebRequest "$base/api/targets").Content.Trim() -ne '[]') { throw 'Seed wrote data before rejecting stale identity' }
        } else {
            # Occupy all four actual scheduler slots with the local slow upstream.
            for ($index = 0; $index -lt 4; $index++) {
                $body = @{ name = "busy $index"; url = "http://127.0.0.1:$FixturePort/slow"; interval_seconds = 60; timeout_seconds = 5; enabled = $false } | ConvertTo-Json
                $target = Invoke-RestMethod "$base/api/targets" -Method Post -ContentType 'application/json' -Body ([Text.Encoding]::UTF8.GetBytes($body))
                $null = Invoke-RestMethod "$base/api/targets/$($target.id)/check" -Method Post
            }
            if ((Invoke-RestMethod "$base/api/status").running -ne 4) { throw 'Test did not occupy all scheduler slots' }
            & $scriptPath -Action Seed
            $targets = Invoke-RestMethod "$base/api/targets"
            $sample = @($targets | Where-Object name -eq '验收 · 分页样本')
            if ($sample.Count -ne 1 -or $sample[0].stats.samples -ne 55) { throw 'Seed failed to produce 55 real samples after temporary saturation' }
            $page = Invoke-RestMethod "$base/api/targets/$($sample[0].id)/checks?limit=50"
            $older = Invoke-RestMethod "$base/api/targets/$($sample[0].id)/checks?limit=50&before_id=$($page.next_before_id)"
            if ($page.items.Count -ne 50 -or $older.items.Count -ne 5) { throw 'Prepared history does not paginate as 50+5' }
        }
        Write-Output "PASS: $scenario"
    } catch {
        $failures += "${scenario}: $($_.Exception.Message)"
        Write-Output "FAIL: $($failures[-1])"
    } finally {
        if ($null -ne $originalSession) { [IO.File]::WriteAllBytes($sessionPath, $originalSession) }
        if (Test-Path -LiteralPath $sessionPath) { & $scriptPath -Action Stop }
    }
}
if ($failures.Count) { throw ($failures -join [Environment]::NewLine) }
