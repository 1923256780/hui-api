# migrate-drill.ps1 - M4 本地迁移演练编排（旧库只读纪律 + 幂等确定性证明）。
#
# 流程：
#   0. 构建 migrate.exe / hui-api.exe；
#   1. 解压最新旧库备份 .db.gz 到临时目录（备份原件只读不动）；
#   2. 循环 2 遍：清空目标库 -> migrate（对账 + JSON 报告 + 令牌清单）->
#      起 hui-api 于 3200 端口（隔离数据目录）-> 冒烟（/health、/api/status、
#      迁移令牌 /v1/models 200、错 key 401、真实转发计费复核）-> 停服；
#   3. 两遍报告 diff + 令牌清单 diff -> 确定性结论；
#   4. 汇总产物路径输出 drill-summary.txt。
#
# 用法：
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts\migrate-drill.ps1 `
#     [-BackupDir D:\EngineeringFiles\Backups] [-OutDir $env:TEMP\hui-m4w1-drill] `
#     [-Model glm-5.3-flash] [-Port 3200] [-GoExe go]
#
# 退出码：0 全部冒烟通过（或真实转发降级为计费引擎层复核并注明）；1 有硬失败。
# 产物：-OutDir 下 run1\run2（report.json / tokens.tsv / smoke.txt）、
#       report-diff.txt、tokens-diff.txt、drill-summary.txt；转发计费复核由
#       演练生成的 bin\drill-read（Go 侧过滤，输出单对象 JSON）完成。
# 安全：tokens.tsv 含令牌明文，仅存在于本地产物目录；演练结束（含 migrate 失败中断）
# 自动删除（L5 评审），对账结论留存 tokens-diff.txt。

param(
    [string]$BackupDir = 'D:\EngineeringFiles\Backups',
    [string]$OutDir = "$env:TEMP\hui-m4w1-drill",
    [string]$Model = 'glm-5.3-flash',
    [int]$Port = 3200,
    [string]$GoExe = ''
)

$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.IO.Compression | Out-Null

$repoRoot = Split-Path -Parent $PSScriptRoot
if (-not $GoExe) {
    $cmd = Get-Command go -ErrorAction SilentlyContinue
    if ($cmd) { $GoExe = $cmd.Source } else { throw "go not found in PATH; pass -GoExe <path-to-go.exe>" }
}

# ---- 0. 产物目录与构建 -------------------------------------------------
$binDir = Join-Path $OutDir 'bin'
$legacyDir = Join-Path $OutDir 'legacy'
New-Item -ItemType Directory -Force -Path $binDir, $legacyDir | Out-Null
$serverExe = Join-Path $binDir 'hui-api.exe'
$migrateExe = Join-Path $binDir 'migrate.exe'

Write-Host '[drill] go      :' $GoExe
Write-Host '[drill] out     :' $OutDir
Push-Location $repoRoot
try {
    & $GoExe build -o $serverExe ./cmd/hui-api
    if ($LASTEXITCODE -ne 0) { throw "build hui-api failed" }
    & $GoExe build -o $migrateExe ./cmd/migrate
    if ($LASTEXITCODE -ne 0) { throw "build migrate failed" }
} finally { Pop-Location }
Write-Host '[drill] build   : OK'

# ---- 1. 最新旧库备份 -> 临时目录解压（原件只读不动） --------------------
$backup = Get-ChildItem -Path $BackupDir -Recurse -Filter '*.db.gz' -File |
    Sort-Object LastWriteTime -Descending | Select-Object -First 1
if (-not $backup) { throw "no .db.gz backup found under $BackupDir" }
Write-Host '[drill] backup  :' $backup.FullName
$legacyDb = Join-Path $legacyDir 'legacy.db'
$src = [IO.File]::OpenRead($backup.FullName)
try {
    $gz = New-Object IO.Compression.GZipStream($src, [IO.Compression.CompressionMode]::Decompress)
    try {
        $dst = [IO.File]::Create($legacyDb)
        try { $gz.CopyTo($dst) } finally { $dst.Dispose() }
    } finally { $gz.Dispose() }
} finally { $src.Dispose() }
if ((Get-Item $legacyDb).Length -le 0) { throw "decompressed legacy db is empty" }
Write-Host '[drill] legacy  :' $legacyDb

# ---- 2. 内嵌 helper：drill-read（生成并编译到 bin/drill-read，bin/ 不入库）----
# 时间窗/协议过滤在 Go 侧完成，只输出单对象 JSON——数组解析留在 Go，
# 规避 PS 5.1 ConvertFrom-Json 管道数组解析的属性堆叠坑（演练实跑复现过：
# 8 行日志数组被堆叠成单对象、字段值变 Object[]，[int64] cast 即炸）。
$drillReadDir = Join-Path $repoRoot 'bin\drill-read'
New-Item -ItemType Directory -Force -Path $drillReadDir | Out-Null
@'
// drill-read 演练辅助（scripts/migrate-drill.ps1 生成，bin/ 不入库）：
// 在 Go 侧按时间窗/协议过滤目标库 logs，输出单对象 JSON 供计费复核。
// 不参与任何生产链路。
package main

import (
	"encoding/json"
	"flag"
	"os"

	"github.com/1923256780/hui-api/internal/model"
	"github.com/1923256780/hui-api/internal/store"
)

type checkResult struct {
	Found   bool  `json:"found"`
	Checked int   `json:"checked"`
	Quota   int64 `json:"quota"`
}

func main() {
	target := flag.String("target", "", "hui-api SQLite 库路径")
	since := flag.Int64("since", 0, "仅统计 created_time >= since 的日志（unix 秒，0 不过滤）")
	protocol := flag.String("protocol", "openai", "仅统计该协议的日志（空串不过滤）")
	limit := flag.Int("limit", 50, "扫描的最新日志行数上限")
	flag.Parse()
	if *target == "" {
		os.Stderr.WriteString("-target 必填\n")
		os.Exit(2)
	}
	st, err := store.Open(*target)
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	defer st.Close()
	// 显式空 Model：GORM 复用 dest 结构体时非零主键会成为隐式条件（docs/11）。
	q := st.Read.Model(&model.Log{}).Order("id desc").Limit(*limit)
	if *since > 0 {
		q = q.Where("created_time >= ?", *since)
	}
	if *protocol != "" {
		q = q.Where("protocol = ?", *protocol)
	}
	var rows []model.Log
	if err := q.Find(&rows).Error; err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(1)
	}
	res := checkResult{Checked: len(rows)}
	for _, r := range rows {
		if r.Quota > res.Quota {
			res.Quota = r.Quota
		}
	}
	res.Found = res.Quota > 0
	_ = json.NewEncoder(os.Stdout).Encode(res)
}
'@ | Set-Content -Path (Join-Path $drillReadDir 'main.go') -Encoding UTF8
$drillReadExe = Join-Path $binDir 'drill-read.exe'
Push-Location $repoRoot
try {
    & $GoExe build -o $drillReadExe ./bin/drill-read
    if ($LASTEXITCODE -ne 0) { throw "build drill-read failed" }
} finally { Pop-Location }

# ---- 3. 两遍演练 -------------------------------------------------------
$summary = New-Object System.Collections.Generic.List[string]
$summary.Add("=== migrate drill summary ===")
$summary.Add("backup : " + $backup.FullName)
$summary.Add("model  : " + $Model)
$summary.Add("port   : " + $Port)
$hardFail = $false

for ($run = 1; $run -le 2; $run++) {
    $runDir = Join-Path $OutDir ("run" + $run)
    New-Item -ItemType Directory -Force -Path $runDir | Out-Null
    $reportPath = Join-Path $runDir 'report.json'
    $tokensPath = Join-Path $runDir 'tokens.tsv'
    $smokePath = Join-Path $runDir 'smoke.txt'
    $smoke = New-Object System.Collections.Generic.List[string]
    Write-Host ('[drill] run' + $run + ' --------')

    # a. 清空目标库（幂等重跑语义：同一路径从零开始）。
    $huiDir = Join-Path $OutDir 'hui'
    New-Item -ItemType Directory -Force -Path $huiDir | Out-Null
    $targetDb = Join-Path $huiDir 'hui.db'
    Get-ChildItem -Path $huiDir -Filter 'hui.db*' -File -ErrorAction SilentlyContinue |
        Remove-Item -Force

    # b. migrate：对账 + 报告 + 令牌清单。
    & $migrateExe -legacy $legacyDb -target $targetDb -report $reportPath -export-tokens $tokensPath
    if ($LASTEXITCODE -ne 0) {
        $smoke.Add("FAIL migrate exit=$LASTEXITCODE")
        $smoke | Set-Content -Path $smokePath -Encoding UTF8
        $hardFail = $true
        break
    }
    $smoke.Add("PASS migrate (exit=0, report+tokens exported)")

    # c. 起 hui-api 于 3200 端口（隔离数据目录）。
    $env:PORT = [string]$Port
    $env:SQLITE_PATH = $targetDb
    $env:SESSION_SECRET = 'drill-session-secret-32chars-minimum-ok'
    $env:HUI_API_ROOT_PASSWORD = 'drillRoot2026'   # 迁移库 root 已存在，此项通常不生效
    $stdoutLog = Join-Path $runDir 'server-stdout.log'
    $stderrLog = Join-Path $runDir 'server-stderr.log'
    $proc = Start-Process -FilePath $serverExe -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput $stdoutLog -RedirectStandardError $stderrLog
    $base = "http://127.0.0.1:$Port"

    try {
        # d. 等待 /health 就绪。
        $ready = $false
        for ($i = 0; $i -lt 30; $i++) {
            try {
                $r = Invoke-WebRequest -Uri "$base/health" -UseBasicParsing -TimeoutSec 2
                if ($r.StatusCode -eq 200) { $ready = $true; break }
            } catch { }
            Start-Sleep -Seconds 1
        }
        if (-not $ready) { throw "hui-api not ready on $base within 30s" }
        $smoke.Add("PASS /health 200")

        # e. 冒烟。
        $st = Invoke-RestMethod -Uri "$base/api/status" -TimeoutSec 10
        if ($st.success -ne $true) { throw "/api/status success != true" }
        $smoke.Add("PASS /api/status 200 (schema=" + $st.data.schema_version + ")")

        # 迁移令牌（关键锚点：key_hash 口径）调 /v1/models。
        $toks = Import-Csv -Path $tokensPath -Delimiter "`t"
        $migKey = ($toks | Where-Object { $_.id -eq '1' }).key
        if (-not $migKey) { throw "token id=1 missing in $tokensPath" }
        $models = Invoke-RestMethod -Uri "$base/v1/models" `
            -Headers @{ Authorization = "Bearer $migKey" } -TimeoutSec 10
        $nModels = @($models.data).Count
        $smoke.Add("PASS /v1/models (migrated token) 200 models=$nModels")

        # 错 key 必须 401。
        $wrongCode = $null
        try {
            $wr = Invoke-WebRequest -Uri "$base/v1/models" `
                -Headers @{ Authorization = 'Bearer sk-drill-wrong-key' } `
                -UseBasicParsing -TimeoutSec 10
            $wrongCode = [int]$wr.StatusCode
        } catch {
            if ($_.Exception.Response) { $wrongCode = [int]$_.Exception.Response.StatusCode }
        }
        if ($wrongCode -ne 401) { throw "wrong key expected 401, got $wrongCode" }
        $smoke.Add("PASS /v1/models (wrong key) 401")

        # 真实转发 + 计费复核（迁移渠道之一真实调用上游；失败降级并注明）。
        $t0 = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
        $body = @{ model = $Model; stream = $false; max_tokens = 32
                   messages = @(@{ role = 'user'; content = '回复两个字：好的' }) } | ConvertTo-Json -Depth 4
        # PS 5.1 字符串 body 不按 UTF-8 编码（中文变 ?），必须转字节原样发送。
        $bodyBytes = [Text.Encoding]::UTF8.GetBytes($body)
        $fwdOk = $false; $fwdNote = ''
        try {
            $resp = Invoke-RestMethod -Uri "$base/v1/chat/completions" -Method Post `
                -ContentType 'application/json; charset=utf-8' -Body $bodyBytes -TimeoutSec 90 `
                -Headers @{ Authorization = "Bearer $migKey" }
            $pt = $resp.usage.prompt_tokens; $ct = $resp.usage.completion_tokens
            if ($null -eq $pt -or $null -eq $ct) { throw "forward 200 but usage missing" }
            # 异步日志排空窗口，drill-read 在 Go 侧过滤后复核计费入账。
            Start-Sleep -Seconds 3
            $chk = & $drillReadExe -target $targetDb -since $t0
            if ($LASTEXITCODE -ne 0) { throw "drill-read failed (exit=$LASTEXITCODE)" }
            $chk = $chk | ConvertFrom-Json
            if (-not $chk.found -or $chk.quota -le 0) {
                throw "no fresh consume log with quota>0 (checked=$($chk.checked))"
            }
            $quota = $chk.quota
            $smoke.Add("PASS forward+billing: usage p=$pt c=$ct, log quota=$quota")
            $fwdOk = $true
        } catch {
            $fwdNote = "{0} (at script line {1})" -f $_.Exception.Message, $_.InvocationInfo.ScriptLineNumber
        }
        if (-not $fwdOk) {
            # 降级：计费引擎层复核（黄金锚定测试），如实注明，不得伪造通过。
            Push-Location $repoRoot
            try {
                & $GoExe test ./internal/migrate/ -run 'TestModelRatioGoldenAnchoring' -count=1
                $testOk = ($LASTEXITCODE -eq 0)
            } finally { Pop-Location }
            if ($testOk) {
                $smoke.Add("DEGRADED forward upstream/network fail ($fwdNote) -> billing-engine golden test PASS")
            } else {
                $smoke.Add("FAIL forward ($fwdNote) and golden test failed")
                $hardFail = $true
            }
        }
    } finally {
        # f. 停服（尽力优雅失败即强杀；演练库无关紧要）。
        if ($proc -and -not $proc.HasExited) {
            Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
        }
        Start-Sleep -Seconds 1
    }

    $smoke | Set-Content -Path $smokePath -Encoding UTF8
    $summary.Add("run$run smoke:")
    foreach ($line in $smoke) { $summary.Add("  " + $line) }
}

# ---- 4. 两遍产物 diff（确定性证明）--------------------------------------
$reportDiff = Join-Path $OutDir 'report-diff.txt'
$tokensDiff = Join-Path $OutDir 'tokens-diff.txt'
if (-not $hardFail) {
    $r1 = Get-Content (Join-Path $OutDir 'run1\report.json')
    $r2 = Get-Content (Join-Path $OutDir 'run2\report.json')
    $d = Compare-Object $r1 $r2
    if ($d.Count -eq 0) {
        Set-Content -Path $reportDiff -Value "IDENTICAL (run1 == run2, byte-level per line)" -Encoding UTF8
        $summary.Add("report diff: IDENTICAL")
    } else {
        $d | ForEach-Object { "{0} {1}" -f $_.SideIndicator, $_.InputObject } |
            Set-Content -Path $reportDiff -Encoding UTF8
        $summary.Add("report diff: DIFF (" + $d.Count + " lines) -> " + $reportDiff)
        $hardFail = $true
    }
    $t1 = Get-Content (Join-Path $OutDir 'run1\tokens.tsv')
    $t2 = Get-Content (Join-Path $OutDir 'run2\tokens.tsv')
    $dt = Compare-Object $t1 $t2
    if ($dt.Count -eq 0) {
        Set-Content -Path $tokensDiff -Value "IDENTICAL" -Encoding UTF8
        $summary.Add("tokens diff: IDENTICAL")
    } else {
        $dt | ForEach-Object { "{0} {1}" -f $_.SideIndicator, $_.InputObject } |
            Set-Content -Path $tokensDiff -Encoding UTF8
        $summary.Add("tokens diff: DIFF (" + $dt.Count + " lines) -> " + $tokensDiff)
        $hardFail = $true
    }
}

# ---- 4.5 清理令牌明文（L5 评审：tokens.tsv 含明文，diff 完成即删，含中断路径）----
foreach ($r in 1..2) {
    $tp = Join-Path (Join-Path $OutDir ('run' + $r)) 'tokens.tsv'
    if (Test-Path $tp) { Remove-Item -Path $tp -Force -ErrorAction SilentlyContinue }
}
$summary.Add('tokens.tsv 明文清单已自动删除（对账结论留存 tokens-diff.txt）')

# ---- 5. 汇总 ------------------------------------------------------------
$summary.Add("artifacts: " + $OutDir)
$summaryPath = Join-Path $OutDir 'drill-summary.txt'
$summary | Set-Content -Path $summaryPath -Encoding UTF8
Write-Host ""
foreach ($line in $summary) { Write-Host $line }
if ($hardFail) {
    Write-Host "[drill] RESULT: FAILED"
    exit 1
}
Write-Host "[drill] RESULT: PASSED"
exit 0
