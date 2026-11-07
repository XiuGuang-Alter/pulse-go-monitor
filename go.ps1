# 优先使用项目内的 Go；只设置当前进程环境，不修改系统 PATH。
$goArguments = $args

$ErrorActionPreference = 'Stop'
$goExecutable = Join-Path $PSScriptRoot '.tools/go/bin/go.exe'
if (-not (Test-Path -LiteralPath $goExecutable)) {
    $goExecutable = (Get-Command go -ErrorAction Stop).Source
}
$env:GOCACHE = Join-Path $PSScriptRoot '.cache/build'
$env:GOPATH = Join-Path $PSScriptRoot '.cache/gopath'
$env:GOTELEMETRY = 'off'
$env:GOTOOLCHAIN = 'local'
Push-Location $PSScriptRoot
try {
    & $goExecutable @goArguments
    $goExitCode = $LASTEXITCODE
} finally {
    Pop-Location
}
exit $goExitCode
