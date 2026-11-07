#Requires -Version 7.0
param([ValidatePattern('^\d+\.\d+\.\d+$')][string]$Version='1.0.0',[switch]$Unstamped)
$ErrorActionPreference='Stop'
$root=Split-Path $PSScriptRoot -Parent
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
foreach($name in @('启动监测.cmd','启动演示.cmd','LICENSE','THIRD_PARTY_NOTICES.txt')){Copy-Item -LiteralPath (Join-Path $root $name) -Destination $stage}
Copy-Item -LiteralPath (Join-Path $root 'docs/下载包使用说明.md') -Destination (Join-Path $stage '使用说明.md')
$commit=git -C $root rev-parse --verify HEAD 2>$null
if($LASTEXITCODE -ne 0){$commit='uncommitted-local-candidate'}
@("Pulse v$Version", "Commit: $commit",'Target: windows/amd64',('Built UTC: '+[DateTime]::UtcNow.ToString('o'))) | Set-Content -LiteralPath (Join-Path $stage 'BUILD.txt') -Encoding utf8
$zip=Join-Path $dist "pulse-v$Version-windows-amd64.zip"
Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zip -Force
$hash=(Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
[IO.File]::WriteAllText((Join-Path $dist 'SHA256SUMS.txt'),"$hash  $([IO.Path]::GetFileName($zip))"+[Environment]::NewLine,[Text.UTF8Encoding]::new($false))
Write-Output "Package: $zip"
Write-Output "SHA256: $hash"
