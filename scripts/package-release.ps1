#Requires -Version 7.0
param([ValidatePattern('^\d+\.\d+\.\d+$')][string]$Version='1.2.0',[switch]$Unstamped)
$ErrorActionPreference='Stop'
$root=Split-Path $PSScriptRoot -Parent
$commit=git -C $root rev-parse --verify HEAD 2>$null
if($LASTEXITCODE -ne 0){$commit='uncommitted-local-candidate'}
if(-not $Unstamped){
    if($commit -notmatch '^[0-9a-f]{40}$'){throw '正式候选打包需要可验证的Git提交；源码归档仅可用于Unstamped本地夹具'}
    $dirty=@(git -C $root status --porcelain=v1 --untracked-files=all)
    if($LASTEXITCODE -ne 0){throw '无法读取构建前Git状态'}
    if($dirty.Count){throw ('构建前Git工作区不干净，拒绝打包：'+($dirty -join '; '))}
}
$dist=Join-Path $root 'dist'
$stage=Join-Path $dist ('staging-'+[Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $stage -Force|Out-Null
$oldOS=$env:GOOS; $oldArch=$env:GOARCH; $oldCGO=$env:CGO_ENABLED
try {
    $env:GOOS='windows';$env:GOARCH='amd64';$env:CGO_ENABLED='0'
    $buildArgs=@('build','-trimpath','-ldflags=-s -w','-o',(Join-Path $stage 'go-monitor.exe'))
    if($Unstamped){$buildArgs+='-buildvcs=false'}
    $buildArgs+='.'
    & (Join-Path $root 'go.ps1') @buildArgs
    if($LASTEXITCODE -ne 0){throw 'Release构建失败'}
} finally {$env:GOOS=$oldOS;$env:GOARCH=$oldArch;$env:CGO_ENABLED=$oldCGO}
if(-not $Unstamped){
    $buildInfo=& (Join-Path $root 'go.ps1') version -m (Join-Path $stage 'go-monitor.exe')
    if($LASTEXITCODE -ne 0){throw '无法读取候选二进制构建元数据'}
    $metadata=$buildInfo -join "`n"
    if($metadata -notmatch ('(?m)^\s*build\s+vcs\.revision='+[regex]::Escape($commit)+'\r?$') -or $metadata -notmatch '(?m)^\s*build\s+vcs\.modified=false\r?$'){throw '候选二进制提交或clean标记不符，拒绝发布包'}
    $afterCommit=git -C $root rev-parse --verify HEAD
    if($LASTEXITCODE -ne 0 -or $afterCommit -ne $commit){throw '构建期间Git提交改变'}
    $afterDirty=@(git -C $root status --porcelain=v1 --untracked-files=all)
    if($LASTEXITCODE -ne 0 -or $afterDirty.Count){throw ('构建后Git状态异常：'+($afterDirty -join '; '))}
}
foreach($name in @('启动监测.cmd','启动演示.cmd','LICENSE','THIRD_PARTY_NOTICES.txt')){Copy-Item -LiteralPath (Join-Path $root $name) -Destination $stage}
Copy-Item -LiteralPath (Join-Path $root 'docs/下载包使用说明.md') -Destination (Join-Path $stage '使用说明.md')
Copy-Item -LiteralPath (Join-Path $root 'docs/数据维护.md') -Destination (Join-Path $stage '数据维护.md')
@("Pulse v$Version", "Commit: $commit",'Target: windows/amd64',('Built UTC: '+[DateTime]::UtcNow.ToString('o'))) | Set-Content -LiteralPath (Join-Path $stage 'BUILD.txt') -Encoding utf8
$zip=Join-Path $dist "pulse-v$Version-windows-amd64.zip"
Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zip -Force
$hash=(Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
[IO.File]::WriteAllText((Join-Path $dist 'SHA256SUMS.txt'),"$hash  $([IO.Path]::GetFileName($zip))"+[Environment]::NewLine,[Text.UTF8Encoding]::new($false))
Write-Output "Package: $zip"
Write-Output "SHA256: $hash"
