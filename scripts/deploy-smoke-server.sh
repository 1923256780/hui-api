#!/usr/bin/env bash
# deploy-smoke-server.sh — hui-api 服务器旁路部署冒烟（M4-wave2，可复跑）。
#
# 用法（服务器上，ubuntu 用户）：
#   ./deploy-smoke-server.sh <tokens.tsv> [expect_schema]
#   HUI_BASE=http://127.0.0.1:3000 可覆盖目标（旁路 3100 缺省；3000 接管后切换冒烟复用）
#   tokens.tsv      migrate -export-tokens 产物（TSV 表头 id/user_id/key，含令牌明文，
#                   敏感文件——脚本退出（含中断）时自动删除（L5 评审），不留明文落盘）
#   expect_schema   期望 schema 版本（缺省 4；schema 演进时由 runbook 传入新值）
#
# 冒烟项（全部走 127.0.0.1:3100 旁路，不触碰生产网关与其数据）：
#   a /health 200
#   b /api/status 200 且 schema_version 等于期望值
#   c 进程 RSS 基线（systemd MainPID）
#   d 迁移令牌调 /v1/models 200 —— key_hash 口径在生产数据上成立的关键锚点
#   e 真实转发计费复核（迁移渠道小 prompt，logs 表 quota 入账由 logcheck 复核；
#     上游不可达时如实降级报告，不伪造通过）
#   f journalctl -u hui-api 无 panic/fatal/异常退出（Go log 走 stderr，journald 会将
#     全部 stderr 标为 err 优先级，故以错误关键字为口径而非 -p err）
#   g free -m 与进程 RSS 排行 —— 双进程并存内存复核
#
# 退出码：0 全部通过；1 存在失败项。
set -uo pipefail

BASE=${HUI_BASE:-http://127.0.0.1:3100}
ROOT=${HUI_ROOT:-/home/ubuntu/hui-api}
UNIT_NAME=${HUI_UNIT:-hui-api}
TOKEN_FILE=${1:?usage: deploy-smoke-server.sh <tokens.tsv> [expect_schema]}
# 令牌清单含明文，脚本退出（含 Ctrl-C 中断）即自动删除——不留明文落盘（L5 评审）。
trap 'rm -f -- "$TOKEN_FILE"' EXIT
EXPECT_SCHEMA=${2:-4}
MODEL=${HUI_SMOKE_MODEL:-glm-5.3-flash}
LOGCHECK=$ROOT/bin/logcheck-linux-amd64

PASS=0; FAIL=0
ok() { echo "PASS $*"; PASS=$((PASS+1)); }
bad() { echo "FAIL $*"; FAIL=$((FAIL+1)); }

echo "== hui-api bypass smoke @ $BASE (model=$MODEL, expect_schema=$EXPECT_SCHEMA) =="

# a. /health
code=$(curl -s -o /tmp/hui-smoke-health.json -w '%{http_code}' "$BASE/health" || true)
if [ "$code" = "200" ]; then
  ok "a /health 200 body=$(cat /tmp/hui-smoke-health.json 2>/dev/null)"
else
  bad "a /health http=$code"
fi
rm -f /tmp/hui-smoke-health.json

# b. /api/status + schema
body=$(curl -s "$BASE/api/status" || true)
schema=$(printf '%s' "$body" | python3 -c 'import json,sys;print(json.load(sys.stdin)["data"]["schema_version"])' 2>/dev/null || echo "?")
if [ "$schema" = "$EXPECT_SCHEMA" ]; then
  ok "b /api/status 200 schema=$schema"
else
  bad "b /api/status schema=$schema expect=$EXPECT_SCHEMA body=$body"
fi

# c. RSS 基线
pid=$(systemctl show -p MainPID --value "$UNIT_NAME" 2>/dev/null || true)
rss=$(ps -p "$pid" -o rss= 2>/dev/null | tr -d ' ' || true)
if [ -n "$rss" ] && [ "$rss" -gt 0 ] 2>/dev/null; then
  ok "c RSS pid=$pid rss=${rss}KB"
else
  bad "c RSS unknown (pid=$pid)"
fi

# d. 迁移令牌 /v1/models（key_hash 口径锚点）
KEY=$(awk -F '\t' 'NR==2{print $3}' "$TOKEN_FILE" 2>/dev/null || true)
if [ -z "$KEY" ]; then
  bad "d token file has no data row: $TOKEN_FILE"
else
  code=$(curl -s -o /tmp/hui-smoke-models.json -w '%{http_code}' \
    -H "Authorization: Bearer $KEY" "$BASE/v1/models" || true)
  if [ "$code" = "200" ]; then
    n=$(python3 -c 'import json;print(len(json.load(open("/tmp/hui-smoke-models.json"))["data"]))' 2>/dev/null || echo "?")
    ok "d /v1/models (migrated token) 200 models=$n"
  else
    bad "d /v1/models (migrated token) http=$code"
  fi
  rm -f /tmp/hui-smoke-models.json
fi

# e. 真实转发 + 计费复核（迁移渠道之一）
if [ -z "$KEY" ]; then
  bad "e forward skipped (no token)"
else
  t0=$(date +%s)
  code=$(curl -s -o /tmp/hui-smoke-fwd.json -w '%{http_code}' \
    -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
    -d "{\"model\":\"$MODEL\",\"stream\":false,\"max_tokens\":32,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: OK\"}]}" \
    "$BASE/v1/chat/completions" || true)
  if [ "$code" = "200" ]; then
    usage=$(python3 -c 'import json;d=json.load(open("/tmp/hui-smoke-fwd.json"))["usage"];print("p=%s c=%s"%(d.get("prompt_tokens"),d.get("completion_tokens")))' 2>/dev/null || echo "?")
    sleep 3   # 异步日志排空窗口
    chk=$("$LOGCHECK" -target "$ROOT/data/hui-api.db" -since "$t0" 2>&1) || chk="{\"found\":false,\"checked\":0,\"quota\":0}"
    found=$(printf '%s' "$chk" | python3 -c 'import json,sys;print(json.load(sys.stdin)["found"])' 2>/dev/null || echo "?")
    quota=$(printf '%s' "$chk" | python3 -c 'import json,sys;print(json.load(sys.stdin)["quota"])' 2>/dev/null || echo 0)
    if [ "$found" = "True" ] && [ "$quota" -gt 0 ] 2>/dev/null; then
      ok "e forward+billing usage($usage) log_quota=$quota"
    else
      bad "e forward 200 but billing log missing (usage=$usage chk=$chk)"
    fi
  else
    bad "e forward http=$code (上游不可达/网络失败——如实降级，不伪造通过)"
  fi
  rm -f /tmp/hui-smoke-fwd.json
fi

# f. journalctl 无 panic/fatal/异常退出（关键字口径，见文件头注记）
errs=$(sudo journalctl -u "$UNIT_NAME" --since "-10min" --no-pager 2>/dev/null | grep -ciE 'panic|fatal|异常退出' || true)
if [ "$errs" = "0" ]; then
  ok "f journalctl no panic/fatal (10min)"
else
  bad "f journalctl $errs error-ish lines:"
  sudo journalctl -u "$UNIT_NAME" --since "-10min" --no-pager 2>/dev/null | grep -iE 'panic|fatal|异常退出' | tail -5
fi

# g. 双进程并存内存复核（free + RSS 排行，hui-api 与生产旧网关进程自然可见）
echo "-- free -m --"
free -m
echo "-- top RSS processes --"
ps -eo rss,pid,comm --sort=-rss --no-headers | head -6
avail=$(free -m | awk '/^Mem:/{print $7}')
if [ "$avail" -gt 100 ] 2>/dev/null; then
  ok "g memory available=${avail}MB (>100MB)"
else
  bad "g memory available=${avail}MB low"
fi

echo "== smoke result: PASS=$PASS FAIL=$FAIL =="
[ "$FAIL" -eq 0 ]
