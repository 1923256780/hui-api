# hui-api 一键构建脚本（Windows PowerShell 5.1+）
# 用法：powershell -ExecutionPolicy Bypass -File scripts/build.ps1 [build|test|vet|run|verify|web]
param(
    [Parameter(Position = 0)]
    [ValidateSet('build', 'test', 'vet', 'run', 'verify', 'web')]
    [string]$Target = 'build'
)

$ErrorActionPreference = 'Stop'
# 仓库根 = 脚本所在目录（scripts/）的上一级，不依赖调用者工作目录
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot
try {
    # 确保 web/dist 存在（go:embed 编译硬依赖）。
    # 优先真实构建（npm ci + build）；npm 不可用或失败时回退占位页，保证 go build 仍可通过。
    function Ensure-WebDist {
        if (-not (Test-Path 'web')) { return }
        if (Test-Path 'web/dist/index.html') { return }
        Write-Host 'web/dist 缺失，开始前端构建…'
        Push-Location web
        try {
            if (Test-Path 'package-lock.json') { npm ci } else { npm install }
            npm run build
        }
        finally {
            Pop-Location
        }
        if (-not (Test-Path 'web/dist/index.html')) {
            Write-Host 'npm 构建失败，写入占位 index.html（仅保证 go build 通过，页面非真实产物）'
            New-Item -ItemType Directory -Force -Path 'web/dist' | Out-Null
            Set-Content -LiteralPath 'web/dist/index.html' -Value '<!doctype html><meta charset="utf-8"><title>hui-api</title><p>web/dist 为占位产物：前端未构建。</p>'
        }
    }

    switch ($Target) {
        'build' {
            Ensure-WebDist
            go build ./...
            Write-Host 'build: OK'
        }
        'test' {
            Ensure-WebDist
            go test ./...
            Write-Host 'test: OK'
        }
        'vet' {
            Ensure-WebDist
            go vet ./...
            Write-Host 'vet: OK'
        }
        'run' {
            Ensure-WebDist
            go run ./cmd/hui-api -addr :3100
        }
        'verify' {
            Ensure-WebDist
            go vet ./...
            go test ./...
            go build ./...
            Write-Host 'verify (vet+test+build): OK'
        }
        'web' {
            if (-not (Test-Path 'web')) {
                Write-Host 'web/ 尚未创建，跳过（CI 同样按目录存在与否自动启用）'
                return
            }
            Push-Location web
            try {
                if (Test-Path 'package-lock.json') { npm ci } else { npm install }
                npm run build
            }
            finally {
                Pop-Location
            }
            Write-Host 'web: OK'
        }
    }
}
finally {
    Pop-Location
}
