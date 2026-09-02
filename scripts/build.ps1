# hui-api 一键构建脚本（Windows PowerShell 5.1+）
# 用法：powershell -ExecutionPolicy Bypass -File scripts/build.ps1 [build|test|vet|run|verify]
param(
    [Parameter(Position = 0)]
    [ValidateSet('build', 'test', 'vet', 'run', 'verify')]
    [string]$Target = 'build'
)

$ErrorActionPreference = 'Stop'
# 仓库根 = 脚本所在目录（scripts/）的上一级，不依赖调用者工作目录
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot
try {
    switch ($Target) {
        'build' {
            go build ./...
            Write-Host 'build: OK'
        }
        'test' {
            go test ./...
            Write-Host 'test: OK'
        }
        'vet' {
            go vet ./...
            Write-Host 'vet: OK'
        }
        'run' {
            go run ./cmd/hui-api -addr :3100
        }
        'verify' {
            go vet ./...
            go test ./...
            go build ./...
            Write-Host 'verify (vet+test+build): OK'
        }
    }
} finally {
    Pop-Location
}
