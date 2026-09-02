# 竞品词扫描（本地版，与 .github/workflows/ci.yml 的 competitor-words job 逻辑一致）
# 用法：powershell -ExecutionPolicy Bypass -File scripts/check-competitor-words.ps1
# 扫描范围排除：词表文件自身、workflow 目录、本脚本自身。
$ErrorActionPreference = 'Stop'
# git 输出统一按 UTF-8 解码，并关闭非 ASCII 路径转义（否则中文文件名变成 \346... 八进制串，
# Test-Path 直接报错，导致本地扫描硬失败、CI 侧静默跳过中文文件名）。
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
# 仓库根 = 脚本所在目录（scripts/）的上一级，不依赖调用者工作目录
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot
try {
    $words = Get-Content -LiteralPath '.github/competitor-words.txt' -Encoding UTF8 |
        Where-Object { $_.Trim() -and -not $_.TrimStart().StartsWith('#') } |
        ForEach-Object { $_.Trim() }
    if (-not $words) {
        Write-Host '词表为空，跳过扫描'
        exit 0
    }
    $pattern = ($words -join '|')
    $excludes = @(
        '^\.github/competitor-words\.txt$',
        '^\.github/workflows/',
        '^scripts/check-competitor-words\.ps1$'
    )
    $files = git -c core.quotepath=false ls-files | Where-Object {
        $f = $_
        -not ($excludes | Where-Object { $f -match $_ })
    }
    $violations = @()
    foreach ($file in $files) {
        if (Test-Path -LiteralPath $file -PathType Leaf) {
            if (Select-String -LiteralPath $file -Pattern $pattern -Quiet -ErrorAction SilentlyContinue) {
                $violations += $file
            }
        }
    }
    if ($violations.Count -gt 0) {
        Write-Host '以下文件包含竞品词，请改用中性表述（规则见 docs/decisions/0002）：'
        $violations | ForEach-Object { Write-Host "  $_" }
        exit 1
    }
    Write-Host '竞品词扫描通过'
    exit 0
} finally {
    Pop-Location
}
