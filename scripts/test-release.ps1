#Requires -Version 7.0
param([string]$ZipPath=(Join-Path (Split-Path $PSScriptRoot -Parent) 'dist/pulse-v1.1.0-windows-amd64.zip'))
$ErrorActionPreference='Stop'
$root=Split-Path $PSScriptRoot -Parent
$directory=Join-Path $root ('.cache/release-smoke-'+[Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $directory|Out-Null
$archive=[IO.Compression.ZipFile]::OpenRead((Resolve-Path -LiteralPath $ZipPath).Path)
try{
    $allowed=@('go-monitor.exe','启动监测.cmd','启动演示.cmd','LICENSE','THIRD_PARTY_NOTICES.txt','使用说明.md','数据维护.md','BUILD.txt')
    if($archive.Entries.Count -ne $allowed.Count){throw '下载包文件数不符合白名单'}
    foreach($entry in $archive.Entries){if($entry.FullName -notin $allowed){throw "下载包含非发布文件：$($entry.FullName)"}}
}finally{$archive.Dispose()}
Expand-Archive -LiteralPath $ZipPath -DestinationPath $directory
$nodeExecutable=(Get-Command node -ErrorAction Stop).Source
$oldPath=$env:PATH
$process=$null
try{
    $env:PATH="$env:SystemRoot\System32;$env:SystemRoot"
    $out=Join-Path $directory 'stdout.log'
    $process=Start-Process -FilePath (Join-Path $directory 'go-monitor.exe') -ArgumentList @('serve','-addr','127.0.0.1:0','-demo','-demo-addr','127.0.0.1:0','-db','data/smoke.db') -WorkingDirectory $directory -WindowStyle Hidden -RedirectStandardOutput $out -RedirectStandardError (Join-Path $directory 'stderr.log') -PassThru
    $base=$null
    for($attempt=0;$attempt -lt 100;$attempt++){
        if($process.HasExited){throw '下载包程序提前退出'}
        if(Test-Path -LiteralPath $out){$text=Get-Content -LiteralPath $out -Raw;if($text -match 'http://127\.0\.0\.1:\d+'){$base=$Matches[0];break}}
        Start-Sleep -Milliseconds 100
    }
    if(-not $base){throw '下载包未启动'}
    if((Invoke-RestMethod "$base/api/status").status -ne 'ok'){throw '服务状态异常'}
    if((Invoke-WebRequest $base).Content -notmatch 'Pulse'){throw '页面未加载'}
    $kinds=@()
    for($attempt=0;$attempt -lt 100;$attempt++){
        $targets=Invoke-RestMethod "$base/api/targets"
        $kinds=@($targets|ForEach-Object {$_.latest.kind}|Sort-Object -Unique)
        if('ok' -in $kinds -and 'http_error' -in $kinds -and 'timeout' -in $kinds){break}
        Start-Sleep -Milliseconds 100
    }
    if('ok' -notin $kinds -or 'http_error' -notin $kinds -or 'timeout' -notin $kinds){throw '演示三种结果未全部产生'}
    $normal=$targets | Where-Object {$_.url -match '/ok$'} | Select-Object -First 1
    $payload=@{name='package-rule-fixture';url=$normal.url;interval_seconds=60;timeout_seconds=1;enabled=$false;rules=@{json_equals=@(@{pointer='/message';equals='wrong'})}}
    $ruleTarget=Invoke-RestMethod "$base/api/targets" -Method Post -ContentType 'application/json' -Body ($payload|ConvertTo-Json -Depth 8)
    $expectedVersion=1
    foreach($expectedKind in @('assertion_failed','ok')){
        if($expectedKind -eq 'ok'){
            $payload.rules.json_equals[0].equals='演示服务正常'
            $ruleTarget=Invoke-RestMethod "$base/api/targets/$($ruleTarget.id)" -Method Put -ContentType 'application/json' -Body ($payload|ConvertTo-Json -Depth 8)
            $expectedVersion++
        }
        Invoke-RestMethod "$base/api/targets/$($ruleTarget.id)/check" -Method Post | Out-Null
        $last=$null
        for($attempt=0;$attempt -lt 100;$attempt++){
            $allTargets=Invoke-RestMethod "$base/api/targets"
            $current=$allTargets | Where-Object {$_.id -eq $ruleTarget.id}
            $last=$current.latest
            if($last.config_version -eq $expectedVersion -and $last.kind -eq $expectedKind -and -not $current.running){break}
            Start-Sleep -Milliseconds 100
        }
        if($current.running -or $last.kind -ne $expectedKind -or $last.config_version -ne $expectedVersion -or $null -eq $last.total_duration_ms){throw '下载包业务规则或完整耗时异常'}
    }
    $daily=Invoke-RestMethod "$base/api/targets/$($ruleTarget.id)/daily?days=1"
    if($daily.timezone -ne 'UTC' -or $daily.buckets[0].samples -ne 2 -or $daily.buckets[0].successes -ne 1){throw '下载包日汇总异常'}
    & $nodeExecutable (Join-Path $PSScriptRoot 'test-maintenance.cjs') (Join-Path $directory 'go-monitor.exe')
    if($LASTEXITCODE -ne 0){throw '下载包维护命令工作流失败'}
    "PASS: 新目录解压、发布白名单、无Go/Node PATH运行、页面/API、正常/500/超时、规则失败与修复及UTC汇总"
}finally{
    if($null -ne $process -and -not $process.HasExited){$process.Kill();$process.WaitForExit()}
    $env:PATH=$oldPath
}
