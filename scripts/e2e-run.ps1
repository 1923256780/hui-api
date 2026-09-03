# e2e-run.ps1 - Isolated launcher for hui-api browser acceptance (Task #18).
#
# Starts the admin console on http://127.0.0.1:3100 with an isolated SQLite
# database under bin\e2e-data\, so production / existing data is never touched.
#
# Root login : username=root  password=e2eRoot2026
# Stop       : Ctrl+C (graceful shutdown) or close the console window.
# Reuse      : powershell -NoProfile -ExecutionPolicy Bypass -File scripts\e2e-run.ps1

$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$exe = Join-Path $repoRoot 'bin\hui-api.exe'
if (-not (Test-Path $exe)) {
    throw "hui-api.exe not found at $exe. Build first: go build -o bin\hui-api.exe ./cmd/hui-api"
}

$dataDir = Join-Path $repoRoot 'bin\e2e-data'
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null

# Isolated runtime environment (bootstrap track): port / sqlite / session / root password.
$env:PORT = '3100'
$env:SQLITE_PATH = Join-Path $dataDir 'e2e.db'
$env:SESSION_SECRET = 'e2e-test-secret-32chars-minimum-ok'
$env:HUI_API_ROOT_PASSWORD = 'e2eRoot2026'

Write-Host '[e2e] exe    :' $exe
Write-Host '[e2e] sqlite :' $env:SQLITE_PATH
Write-Host '[e2e] url    : http://127.0.0.1:3100/  (login: root / e2eRoot2026)'

& $exe
exit $LASTEXITCODE
