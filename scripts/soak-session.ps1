#Requires -Version 7.0
param(
    [ValidateSet('Start','Status')][string]$Action='Status',
    [ValidateRange(1,172800)][int]$Seconds=86400,
    [string]$OutputDirectory='',
    [ValidateRange(1,3600)][int]$MutationSeconds=300,
    [string]$SessionFile='',
    [switch]$AllowDirty
)
$ErrorActionPreference='Stop'
$root=[IO.Path]::GetFullPath((Split-Path $PSScriptRoot -Parent))
$cache=[IO.Path]::GetFullPath((Join-Path $root '.cache'))
$prefix=$cache+[IO.Path]::DirectorySeparatorChar
function Cache-Path([string]$value){
    $resolved=[IO.Path]::GetFullPath($value,$root)
    if(-not $resolved.StartsWith($prefix,[StringComparison]::OrdinalIgnoreCase)){throw '长跑证据路径必须位于本项目.cache目录内'}
    $probe=$resolved
    while($probe.StartsWith($prefix,[StringComparison]::OrdinalIgnoreCase) -or $probe -eq $cache){
        if(Test-Path -LiteralPath $probe){
            if((Get-Item -LiteralPath $probe -Force).Attributes -band [IO.FileAttributes]::ReparsePoint){throw '长跑证据路径不允许符号链接或目录联接'}
        }
        $probe=Split-Path $probe -Parent
    }
    return $resolved
}
if(-not $SessionFile){$SessionFile=Join-Path $cache 'soak-active-session.json'}
$SessionFile=Cache-Path $SessionFile
function Worker-IsAlive($record){
    $worker=Get-Process -Id $record.worker_pid -ErrorAction SilentlyContinue
    if($null -eq $worker){return $false}
    return $worker.Path -eq $record.worker_path -and $worker.StartTime.ToUniversalTime().Ticks.ToString() -eq $record.worker_started_ticks
}
if($Action -eq 'Status'){
    $record=Get-Content -LiteralPath $SessionFile -Raw | ConvertFrom-Json
    $directory=Cache-Path $record.output_directory
    $alive=Worker-IsAlive $record
    $progress=$null;$result=$null;$failure=$null
    if(Test-Path -LiteralPath (Join-Path $directory 'progress.json')){$progress=Get-Content -LiteralPath (Join-Path $directory 'progress.json') -Raw|ConvertFrom-Json}
    if(Test-Path -LiteralPath (Join-Path $directory 'result.json')){$result=Get-Content -LiteralPath (Join-Path $directory 'result.json') -Raw|ConvertFrom-Json}
    if(Test-Path -LiteralPath (Join-Path $directory 'failure.txt')){$failure=Get-Content -LiteralPath (Join-Path $directory 'failure.txt') -Raw}
    $validResult=$result -and $result.worker_pid -eq $record.worker_pid -and $result.configured_seconds -eq $record.configured_seconds -and $result.elapsed_seconds -ge $record.configured_seconds -and $result.phase -eq 'completed' -and $result.passed -and $result.cleanup.passed
    $failed=$failure -or ($result -and ($result.phase -eq 'failed' -or $result.passed -eq $false -or $result.cleanup.passed -eq $false))
    $stale=$alive -and $progress -and $progress.phase -eq 'measuring' -and ([DateTimeOffset]::UtcNow-[DateTimeOffset]$progress.updated_utc).TotalSeconds -gt 30
    $state=if($failed){'failed'}elseif($validResult -and -not $alive){'completed'}elseif($stale){'stale'}elseif($alive){'running'}else{'incomplete'}
    [ordered]@{state=$state;worker_alive=$alive;session=$record;progress=$progress;result=$result;failure=$failure}|ConvertTo-Json -Depth 12
    return
}
if(-not $IsWindows){throw '此后台启动器仅用于Windows；其它系统可直接运行node scripts/soak.cjs'}
$sessionParent=Split-Path $SessionFile -Parent
if(-not (Test-Path -LiteralPath $sessionParent -PathType Container)){throw '会话记录的父目录不存在，未启动worker'}
$lockPath=$SessionFile+'.lock'
$lockStream=$null;$tempStream=$null;$temporary=$null;$worker=$null;$workerTicks=$null;$nodeExecutable=$null;$published=$false
$oldSeconds=$env:PULSE_SOAK_SECONDS;$oldOutput=$env:PULSE_SOAK_OUT;$oldMutation=$env:PULSE_SOAK_MUTATION_SECONDS
try{
    # The lock covers prior-state validation, process start and session publication.
    $lockStream=[IO.File]::Open($lockPath,[IO.FileMode]::CreateNew,[IO.FileAccess]::ReadWrite,[IO.FileShare]::None)
    if(Test-Path -LiteralPath $SessionFile){
        $prior=Get-Content -LiteralPath $SessionFile -Raw|ConvertFrom-Json
        if(Worker-IsAlive $prior){throw '已有匹配身份的长跑worker在运行，拒绝并发启动'}
    }
    if(-not $OutputDirectory){$OutputDirectory=Join-Path $cache ('soak-v12-'+[DateTime]::UtcNow.ToString('yyyyMMdd-HHmmss')+'-'+[Guid]::NewGuid().ToString('N').Substring(0,8))}
    $OutputDirectory=Cache-Path $OutputDirectory
    if(Test-Path -LiteralPath $OutputDirectory){throw '证据目录已经存在，拒绝覆盖'}
    $commit=git -c "safe.directory=$root" -C $root rev-parse HEAD
    if($LASTEXITCODE -ne 0){throw '无法读取候选提交'}
    $dirty=@(git -c "safe.directory=$root" -C $root status --porcelain)
    if($LASTEXITCODE -ne 0){throw '无法核查工作区'}
    if($dirty.Count -and -not $AllowDirty){throw '冻结候选必须是干净工作区；仅短预检可使用-AllowDirty'}
    if($Seconds -ge 86400 -and ($AllowDirty -or $MutationSeconds -ne 300)){throw '24小时验收不允许脏工作区或短预检变更周期'}
    $nodeExecutable=(Get-Command node -ErrorAction Stop).Source
    $logStem=Join-Path $cache ('soak-worker-'+[Guid]::NewGuid().ToString('N'))
    # Reserve the record before side effects: path/ACL failures cannot orphan a worker.
    $temporary=$SessionFile+'.tmp-'+[Guid]::NewGuid().ToString('N')
    $tempStream=[IO.File]::Open($temporary,[IO.FileMode]::CreateNew,[IO.FileAccess]::ReadWrite,[IO.FileShare]::None)
    $placeholder=[Text.Encoding]::UTF8.GetBytes('{"state":"launching"}')
    $tempStream.Write($placeholder);$tempStream.Flush($true)
    $env:PULSE_SOAK_SECONDS=$Seconds.ToString();$env:PULSE_SOAK_OUT=$OutputDirectory;$env:PULSE_SOAK_MUTATION_SECONDS=$MutationSeconds.ToString()
    $worker=Start-Process -FilePath $nodeExecutable -ArgumentList @('scripts/soak.cjs') -WorkingDirectory $root -WindowStyle Hidden -RedirectStandardOutput ($logStem+'.stdout.log') -RedirectStandardError ($logStem+'.stderr.log') -PassThru
    $workerTicks=$worker.StartTime.ToUniversalTime().Ticks.ToString()
    $record=[ordered]@{worker_pid=$worker.Id;worker_started_ticks=$workerTicks;worker_path=$nodeExecutable;started_utc=[DateTime]::UtcNow.ToString('o');configured_seconds=$Seconds;source_commit=$commit;dirty_preflight=[bool]$dirty.Count;output_directory=$OutputDirectory;stdout_log=$logStem+'.stdout.log';stderr_log=$logStem+'.stderr.log'}
    $encoded=[Text.Encoding]::UTF8.GetBytes(($record|ConvertTo-Json))
    $tempStream.Position=0;$tempStream.SetLength(0);$tempStream.Write($encoded);$tempStream.Flush($true);$tempStream.Dispose();$tempStream=$null
    Move-Item -LiteralPath $temporary -Destination $SessionFile -Force
    $published=$true
    $record|ConvertTo-Json
}catch{
    $launchError=$_
    if($worker -and -not $published -and -not $worker.HasExited){
        if($worker.Id -eq $PID -or $worker.Path -ne $nodeExecutable -or $worker.StartTime.ToUniversalTime().Ticks.ToString() -ne $workerTicks){
            throw "会话发布失败且worker身份无法核对，未终止不明进程；请检查PID $($worker.Id) 与启动日志。原错误：$launchError"
        }
        try{$worker.Kill($true);$worker.WaitForExit()}catch{throw "会话发布失败，本次worker进程树终止失败；PID $($worker.Id)。原错误：$launchError；清理错误：$_"}
    }
    throw $launchError
}finally{
    $env:PULSE_SOAK_SECONDS=$oldSeconds;$env:PULSE_SOAK_OUT=$oldOutput;$env:PULSE_SOAK_MUTATION_SECONDS=$oldMutation
    if($tempStream){$tempStream.Dispose()}
    if($temporary -and (Test-Path -LiteralPath $temporary)){Remove-Item -LiteralPath $temporary}
    if($lockStream){$lockStream.Dispose();Remove-Item -LiteralPath $lockPath}
}
