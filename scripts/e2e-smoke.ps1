# e2e-smoke.ps1 - M3-wave4 smoke against the isolated e2e environment (port 3100).
#
# Prereq : build bin\hui-api.exe first, then start scripts\e2e-run.ps1 in a
#          separate console (isolated sqlite at bin\e2e-data\e2e.db).
# Usage  : powershell -NoProfile -ExecutionPolicy Bypass -File scripts\e2e-smoke.ps1
#
# Chain  : root login -> enable register + epay + aff -> root aff_code ->
#          register (with aff_code) -> login -> 2FA setup/enable -> stage1 login ->
#          login/2fa -> redemption create -> topup redeem -> /api/log/mine
#          (session scope + field whitelist) -> epay order -> hand-signed MD5
#          notify callback -> quota credited -> aff rebate -> order paid.
# Order-expiry worker (5min cycle) is covered by unit tests, not awaited here.
# Register email-verification is off by default in this env (no SMTP mock needed).

$ErrorActionPreference = 'Stop'
$base = 'http://127.0.0.1:3100'
$script:passed = 0
$script:failed = 0

function Step([string]$name, [bool]$ok, [string]$detail = '') {
    if ($ok) {
        $script:passed++
        Write-Host ("  [PASS] {0}" -f $name)
    } else {
        $script:failed++
        Write-Host ("  [FAIL] {0}  {1}" -f $name, $detail)
    }
}

function Get-Totp([string]$secret) {
    # RFC 6238 TOTP, SHA1 / 6 digits / 30s step (pquerna/otp defaults).
    # NOTE: PS 5.1 precedence quirk - compute each byte in its own statement,
    # never mix comma-separated array elements with bitwise operators.
    $alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
    $bits = ''
    foreach ($c in $secret.ToUpperInvariant().ToCharArray()) {
        $idx = $alphabet.IndexOf($c)
        if ($idx -ge 0) { $bits += [Convert]::ToString($idx, 2).PadLeft(5, '0') }
    }
    $keyLen = [math]::Floor($bits.Length / 8)
    $key = New-Object byte[] $keyLen
    for ($i = 0; $i -lt $keyLen; $i++) { $key[$i] = [Convert]::ToByte($bits.Substring($i * 8, 8), 2) }
    $counter = [long]([math]::Floor([DateTimeOffset]::UtcNow.ToUnixTimeSeconds() / 30))
    $c0 = ([long]$counter -shr 56) -band 0xFF
    $c1 = ([long]$counter -shr 48) -band 0xFF
    $c2 = ([long]$counter -shr 40) -band 0xFF
    $c3 = ([long]$counter -shr 32) -band 0xFF
    $c4 = ([long]$counter -shr 24) -band 0xFF
    $c5 = ([long]$counter -shr 16) -band 0xFF
    $c6 = ([long]$counter -shr 8) -band 0xFF
    $c7 = $counter -band 0xFF
    $cb = [byte[]]@($c0, $c1, $c2, $c3, $c4, $c5, $c6, $c7)
    $hmac = New-Object System.Security.Cryptography.HMACSHA1(, $key)
    $d = $hmac.ComputeHash($cb)
    $off = $d[$d.Length - 1] -band 0x0F
    $b0 = ($d[$off] -band 0x7F) -shl 24
    $b1 = ($d[$off + 1] -band 0xFF) -shl 16
    $b2 = ($d[$off + 2] -band 0xFF) -shl 8
    $b3 = $d[$off + 3] -band 0xFF
    $code = [long]($b0 -bor $b1 -bor $b2 -bor $b3)
    return ([string]($code % 1000000)).PadLeft(6, '0')
}

function Get-EpaySign([System.Collections.IDictionary]$params, [string]$key) {
    # EPay MD5: sort by key (exclude sign/sign_type/empty), join k=v& with a
    # TRAILING & after EVERY pair, append key, hex lowercase (see epaySignBase).
    $pairs = @($params.Keys | Where-Object { $_ -ne 'sign' -and $_ -ne 'sign_type' -and "$($params[$_])" -ne '' } |
        Sort-Object | ForEach-Object { "$_=$($params[$_])&" })
    $raw = ($pairs -join '') + $key
    $md5 = [System.Security.Cryptography.MD5]::Create()
    $hash = $md5.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($raw))
    return (-join ($hash | ForEach-Object { $_.ToString('x2') }))
}

function JsonBody([object]$obj) {
    return ($obj | ConvertTo-Json -Depth 6)
}

function SendJson([string]$method, [string]$uri, [string]$body, [object]$session) {
    $p = @{ Uri = $uri; Method = $method; ContentType = 'application/json'
            WebSession = $session; UseBasicParsing = $true; TimeoutSec = 30 }
    if ($body -ne '') { $p.Body = [System.Text.Encoding]::UTF8.GetBytes($body) }
    return Invoke-WebRequest @p
}

function DataOf([string]$respText) {
    return ($respText | ConvertFrom-Json).data
}

# ---------- 0. health ----------
Write-Host '[0] health probe'
try {
    $h = Invoke-WebRequest -Uri "$base/api/status" -UseBasicParsing -TimeoutSec 10
    Step 'GET /api/status reachable' ($h.StatusCode -eq 200)
} catch {
    Step 'GET /api/status reachable' $false $_.Exception.Message
    exit 1
}

# ---------- 1. root login (creates session) ----------
Write-Host '[1] root login'
$r = Invoke-WebRequest -Uri "$base/api/user/login" -Method Post `
    -Body ([System.Text.Encoding]::UTF8.GetBytes((JsonBody @{ username = 'root'; password = 'e2eRoot2026' }))) `
    -ContentType 'application/json' -SessionVariable rootSession -UseBasicParsing -TimeoutSec 30
$root = DataOf $r.Content
Step 'root login ok' ($null -ne $root -and $root.role -eq 100) $r.Content

# ---------- 2. options: register + epay + aff ----------
Write-Host '[2] configure options (register/epay/aff)'
$epaySecret = 'e2e-epay-secret-2026'
$r = SendJson 'Put' "$base/api/option" (JsonBody @{
        options = [ordered]@{
            'register.enabled'   = 'true'
            'epay.enabled'       = 'true'
            'epay.pid'           = '1001'
            'epay.gateway'       = 'https://epay-mock.local'
            'epay.secret_key'    = $epaySecret
            'epay.pay_type'      = 'wxpay'
            'aff.rebate_percent' = '10'
        }
    }) $rootSession
$ver = (DataOf $r.Content).version
Step 'PUT /api/option ok (hot reload)' ($r.StatusCode -eq 200 -and $null -ne $ver) $r.Content

# ---------- 3. root aff_code ----------
$r = SendJson 'Get' "$base/api/user/aff" '' $rootSession
$aff = DataOf $r.Content
Step 'GET /api/user/aff returns aff_code' ($null -ne $aff -and $aff.aff_code -ne '') $r.Content
$affQuotaBefore = [long]$aff.aff_history_quota

# ---------- 4. register invitee ----------
Write-Host '[4] register new user (invited by root)'
$stamp = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$uname = "e2ew4$stamp"
$r = Invoke-WebRequest -Uri "$base/api/user/register" -Method Post `
    -Body ([System.Text.Encoding]::UTF8.GetBytes((JsonBody @{
            username = $uname; password = 'e2ePass2026'; email = "$uname@test.local"; aff_code = $aff.aff_code
        }))) `
    -ContentType 'application/json' -SessionVariable anonSession -UseBasicParsing -TimeoutSec 30
Step 'POST /api/user/register ok' ($r.StatusCode -eq 200 -or $r.StatusCode -eq 201) $r.Content

# ---------- 5. invitee login ----------
$r = Invoke-WebRequest -Uri "$base/api/user/login" -Method Post `
    -Body ([System.Text.Encoding]::UTF8.GetBytes((JsonBody @{ username = $uname; password = 'e2ePass2026' }))) `
    -ContentType 'application/json' -SessionVariable userSession -UseBasicParsing -TimeoutSec 30
$user = DataOf $r.Content
Step 'invitee login ok' ($null -ne $user -and $user.username -eq $uname) $r.Content
$quotaBefore = [long](DataOf (SendJson 'Get' "$base/api/user/self" '' $userSession).Content).quota

# ---------- 6. 2FA setup/enable + two-stage login ----------
Write-Host '[6] 2FA setup/enable + stage1 login'
$r = SendJson 'Post' "$base/api/user/totp/setup" '{}' $userSession
$t = DataOf $r.Content
Step 'totp setup returns secret' ($null -ne $t -and $t.secret -ne '') $r.Content
$code = Get-Totp $t.secret
$r = SendJson 'Post' "$base/api/user/totp/enable" (JsonBody @{ code = $code }) $userSession
Step 'totp enable with TOTP code' ($r.StatusCode -eq 200) $r.Content

$r = Invoke-WebRequest -Uri "$base/api/user/login" -Method Post `
    -Body ([System.Text.Encoding]::UTF8.GetBytes((JsonBody @{ username = $uname; password = 'e2ePass2026' }))) `
    -ContentType 'application/json' -SessionVariable stage1Session -UseBasicParsing -TimeoutSec 30
$s1 = DataOf $r.Content
Step 'stage1 login require_2fa=true' ($null -ne $s1 -and $s1.require_2fa -eq $true) $r.Content
Start-Sleep -Seconds 1
$code2 = Get-Totp $t.secret
$r = SendJson 'Post' "$base/api/user/login/2fa" (JsonBody @{ code = $code2 }) $stage1Session
$s2 = DataOf $r.Content
Step 'login/2fa issues full session' ($null -ne $s2 -and $s2.username -eq $uname) $r.Content

# ---------- 7. redemption: create (root) + redeem (invitee) ----------
Write-Host '[7] redemption create + redeem'
$r = SendJson 'Post' "$base/api/redemption" (JsonBody @{ count = 1; name = "e2ew4$stamp"; quota = 1000; expired_time = -1 }) $rootSession
$keys = (DataOf $r.Content).keys
Step 'POST /api/redemption returns keys' ($null -ne $keys -and @($keys).Count -eq 1) $r.Content
$r = SendJson 'Post' "$base/api/user/topup" (JsonBody @{ key = $keys[0] }) $userSession
$redeem = DataOf $r.Content
Step 'POST /api/user/topup credits quota' ($null -ne $redeem -and [long]$redeem.quota_added -eq 1000) $r.Content

# ---------- 8. /api/log/mine: session scope + field whitelist ----------
Write-Host '[8] /api/log/mine whitelist + admin-only guard'
$r = SendJson 'Get' "$base/api/log/mine?page=1&page_size=20" '' $userSession
$mine = DataOf $r.Content
$hasRow = ($null -ne $mine.items -and @($mine.items).Count -ge 1)
Step 'GET /api/log/mine has rows' $hasRow $r.Content
if ($hasRow) {
    $props = @(($mine.items[0]).PSObject.Properties.Name)
    Step 'log/mine whitelist: no user_id/channel_id' (($props -notcontains 'user_id') -and ($props -notcontains 'channel_id')) ($props -join ',')
    Step 'log/mine has consume fields' (($props -contains 'model_name') -and ($props -contains 'quota') -and ($props -contains 'created_time')) ($props -join ',')
}
try {
    SendJson 'Get' "$base/api/log?page=1&page_size=10" '' $userSession | Out-Null
    Step 'GET /api/log forbidden for normal user' $false 'expected 403 but got 200'
} catch {
    $sc = 0
    if ($_.Exception.Response) { $sc = [int]$_.Exception.Response.StatusCode }
    Step 'GET /api/log forbidden for normal user' ($sc -eq 403) "status=$sc"
}

# ---------- 9. epay order + hand-signed notify callback ----------
Write-Host '[9] epay mock order + MD5-signed notify'
$r = SendJson 'Post' "$base/api/user/topup/order" (JsonBody @{ gateway = 'epay'; amount_cents = 1000 }) $userSession
$order = DataOf $r.Content
Step 'POST /api/user/topup/order (epay) ok' ($null -ne $order -and $order.order_no -ne '') $r.Content
Step 'order quota = 694444 (rate 720)' ([long]$order.quota -eq 694444) ("quota=" + $order.quota)

$notify = [ordered]@{
    pid          = '1001'
    trade_no     = "E2E$stamp"
    out_trade_no = $order.order_no
    trade_status = 'TRADE_SUCCESS'
    type         = 'wxpay'
    name         = 'e2e-topup'
    money        = '10.00'
}
$sig = Get-EpaySign $notify $epaySecret
$qs = ($notify.Keys | ForEach-Object { $v = [uri]::EscapeDataString("$($notify[$_])"); "$_=$v" }) -join '&'
$qs = "$qs&sign_type=MD5&sign=$sig"
try {
    $r = Invoke-WebRequest -Uri "$base/api/pay/epay/notify?$qs" -UseBasicParsing -TimeoutSec 30
    Step 'epay notify returns success' ($r.Content.Trim() -eq 'success') $r.Content
} catch {
    Step 'epay notify returns success' $false $_.Exception.Message
}
Start-Sleep -Milliseconds 500

$self = DataOf (SendJson 'Get' "$base/api/user/self" '' $userSession).Content
$expectQuota = $quotaBefore + 1000 + 694444
Step 'buyer quota credited (+1000 +694444)' ([long]$self.quota -eq $expectQuota) ("quota=$($self.quota) expect=$expectQuota")

$r = SendJson 'Get' "$base/api/user/topup/orders?page=1&page_size=10" '' $userSession
$row = @((DataOf $r.Content).items) | Where-Object { $_.order_no -eq $order.order_no } | Select-Object -First 1
Step 'order status = 2 (paid)' ($null -ne $row -and [int]$row.status -eq 2) ("status=" + $row.status)

# ---------- 10. aff rebate to root (same transaction) ----------
$r = SendJson 'Get' "$base/api/user/aff" '' $rootSession
$affAfter = DataOf $r.Content
$expectAff = $affQuotaBefore + 69444
Step 'root aff rebate credited (+69444)' ([long]$affAfter.aff_history_quota -eq $expectAff) ("aff=$($affAfter.aff_history_quota) expect=$expectAff")

# ---------- summary ----------
Write-Host ''
Write-Host ("[smoke] passed={0} failed={1}" -f $script:passed, $script:failed)
if ($script:failed -gt 0) { exit 1 }
Write-Host '[smoke] ALL GREEN: register -> login -> 2FA -> redemption -> /api/log/mine -> epay notify -> aff rebate'
exit 0
