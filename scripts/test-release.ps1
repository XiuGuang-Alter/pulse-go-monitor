#Requires -Version 7.0
param([string]$ZipPath=(Join-Path (Split-Path $PSScriptRoot -Parent) 'dist/pulse-v1.0.0-windows-amd64.zip'))
$ErrorActionPreference='Stop'
$root=Split-Path $PSScriptRoot -Parent
$directory=Join-Path $root ('.cache/release-smoke-'+[Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $directory|Out-Null
$archive=[IO.Compression.ZipFile]::OpenRead((Resolve-Path -LiteralPath $ZipPath).Path)
try{
    $allowed=@('go-monitor.exe','启动监测.cmd','启动演示.cmd','LICENSE','THIRD_PARTY_NOTICES.txt','使用说明.md','BUILD.txt')
    if($archive.Entries.Count -ne $allowed.Count){throw '下载包文件数不符合白名单'}
    foreach($entry in $archive.Entries){if($entry.FullName -notin $allowed){throw "下载包含非发布文件：$($entry.FullName)"}}
}finally{$archive.Dispose()}
Expand-Archive -LiteralPath $ZipPath -DestinationPath $directory
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
    "PASS: 新目录解压、发布白名单、无Go/Node PATH运行、页面/API及正常/500/超时"
}finally{
    if($null -ne $process -and -not $process.HasExited){$process.Kill();$process.WaitForExit()}
    $env:PATH=$oldPath
}
