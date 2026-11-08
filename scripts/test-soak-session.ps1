#Requires -Version 7.0
$ErrorActionPreference='Stop'
$root=Split-Path $PSScriptRoot -Parent
$directory=Join-Path $root ('.cache/soak-session-tests-'+[Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $directory|Out-Null
$session=Join-Path $directory 'session.json'
$output=Join-Path $directory 'evidence'
New-Item -ItemType Directory -Path $output|Out-Null
$record=@{worker_pid=2147483000;worker_path='not-an-active-worker';worker_started_ticks='0';configured_seconds=86400;output_directory=$output}
$record|ConvertTo-Json|Set-Content -LiteralPath $session -Encoding utf8
$result=@{worker_pid=$record.worker_pid;configured_seconds=86400;elapsed_seconds=60;passed=$true;phase='completed';cleanup=@{passed=$true}}
$result|ConvertTo-Json|Set-Content -LiteralPath (Join-Path $output 'result.json') -Encoding utf8
$state=& (Join-Path $PSScriptRoot 'soak-session.ps1') -Action Status -SessionFile $session|ConvertFrom-Json
if($state.state -eq 'completed'){throw 'Short/partial result was falsely accepted as a completed 24-hour run'}
$result.elapsed_seconds=86400
$result|ConvertTo-Json|Set-Content -LiteralPath (Join-Path $output 'result.json') -Encoding utf8
$state=& (Join-Path $PSScriptRoot 'soak-session.ps1') -Action Status -SessionFile $session|ConvertFrom-Json
if($state.state -ne 'completed'){throw 'Valid completed result was not recognized'}
$result.cleanup.passed=$false
$result|ConvertTo-Json|Set-Content -LiteralPath (Join-Path $output 'result.json') -Encoding utf8
$state=& (Join-Path $PSScriptRoot 'soak-session.ps1') -Action Status -SessionFile $session|ConvertFrom-Json
if($state.state -eq 'completed'){throw 'Failed cleanup was falsely accepted'}
$rejected=$false
try{& (Join-Path $PSScriptRoot 'soak-session.ps1') -Action Start -OutputDirectory (Join-Path $root '../outside-soak') -SessionFile $session -AllowDirty}catch{$rejected=$true}
if(-not $rejected){throw 'Output path escape was accepted'}
$rejected=$false
try{& (Join-Path $PSScriptRoot 'soak-session.ps1') -Action Start -OutputDirectory $output -SessionFile $session -AllowDirty}catch{$rejected=$true}
if(-not $rejected){throw 'Existing evidence directory was accepted'}
'PASS: status rejects partial/cleanup-failed runs; start rejects path escape and existing evidence'

# Replace only the process-launch boundary: no real worker is created by these
# failure-path tests, and Kill(true) is observed on the fake process object.
$global:soakMockStarts=0;$global:soakMockKills=0
$global:soakMockLogPaths=[Collections.Generic.List[string]]::new()
function Start-Process {
    param([string]$FilePath,[object[]]$ArgumentList,[string]$WorkingDirectory,[string]$WindowStyle,[string]$RedirectStandardOutput,[string]$RedirectStandardError,[switch]$PassThru)
    $global:soakMockStarts++
    $global:soakMockLogPaths.Add($RedirectStandardOutput)
    $fake=[pscustomobject]@{Id=2147483000;Path=$FilePath;StartTime=[DateTime]::UtcNow;HasExited=$false}
    $fake|Add-Member ScriptMethod Kill {param([bool]$tree);if(-not $tree){throw 'failed launch must clean its full private tree'};$global:soakMockKills++}
    $fake|Add-Member ScriptMethod WaitForExit {}
    return $fake
}
$missing=Join-Path $directory 'missing-parent/session.json'
try{& (Join-Path $PSScriptRoot 'soak-session.ps1') -Action Start -Seconds 1 -AllowDirty -SessionFile $missing}catch{}
if($global:soakMockStarts -ne 0){throw 'Invalid session parent launched a worker'}
$locked=Join-Path $directory 'locked.json'
[IO.File]::WriteAllText($locked+'.lock','fixture lock')
try{& (Join-Path $PSScriptRoot 'soak-session.ps1') -Action Start -Seconds 1 -AllowDirty -SessionFile $locked}catch{}
if($global:soakMockStarts -ne 0){throw 'Concurrent session lock did not prevent worker launch'}
Remove-Item -LiteralPath ($locked+'.lock')
function Move-Item {param([string]$LiteralPath,[string]$Destination,[switch]$Force);throw 'injected session publication failure'}
$failedSession=Join-Path $directory 'publish-failed.json'
$rejected=$false
try{& (Join-Path $PSScriptRoot 'soak-session.ps1') -Action Start -Seconds 1 -AllowDirty -SessionFile $failedSession}catch{$rejected=$true}
Remove-Item Function:\Move-Item
if(-not $rejected -or $global:soakMockStarts -ne 1 -or $global:soakMockKills -ne 1){throw 'Failed session publication did not stop its own worker tree'}
if(Test-Path -LiteralPath ($failedSession+'.lock')){throw 'Failed launch left its lock'}
foreach($nested in @('first','second')){
    $folder=Join-Path $directory $nested;New-Item -ItemType Directory -Path $folder|Out-Null
    $sessionOut=Join-Path $directory ($nested+'.json')
    $null=& (Join-Path $PSScriptRoot 'soak-session.ps1') -Action Start -Seconds 1 -AllowDirty -SessionFile $sessionOut -OutputDirectory (Join-Path $folder 'same-basename')
}
if($global:soakMockLogPaths[1] -eq $global:soakMockLogPaths[2]){throw 'Separate evidence directories reused worker log paths'}
Remove-Item Function:\Start-Process
'PASS: invalid parent/lock block launch; failed publication cleans worker tree; logs are unique'
