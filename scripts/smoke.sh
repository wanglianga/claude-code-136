#!/usr/bin/env bash
# 端到端冒烟：自动拉起临时 redis 与服务，验证核心业务闭环后清理。
# 依赖：bash、curl、jq、redis-server、go。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REDIS_PORT=6390
HTTP_PORT=18080
BASE="http://127.0.0.1:${HTTP_PORT}"
ADMIN="admin123"; STAFF="staff123"; CUSTOMER="customer123"
PASS_N=0; FAIL_N=0
TMPDIR="$(mktemp -d)"

REDIS_PID=""; SERVER_PID=""
cleanup() {
  [ -n "$SERVER_PID" ] && kill "$SERVER_PID" 2>/dev/null || true
  [ -n "$REDIS_PID" ]  && kill "$REDIS_PID" 2>/dev/null || true
  rm -rf "$TMPDIR"
}
trap cleanup EXIT

echo ">> 构建服务二进制"
( cd "$ROOT" && go build -o "$TMPDIR/server" ./cmd/server )

echo ">> redis-server $(redis-server --version | awk '{print $3}')"
redis-server --port "$REDIS_PORT" --save "" --appendonly "no" --daemonize no >"$TMPDIR/redis.log" 2>&1 &
REDIS_PID=$!
for _ in $(seq 1 50); do
  redis-cli -p "$REDIS_PORT" ping 2>/dev/null | grep -q PONG && break
  sleep 0.1
done

HTTP_ADDR=":${HTTP_PORT}" REDIS_ADDR="127.0.0.1:${REDIS_PORT}" REDIS_DB=15 \
  "$TMPDIR/server" >"$TMPDIR/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 100); do
  curl -sf "$BASE/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -sf "$BASE/healthz" >/dev/null

# api METHOD PATH KEY [JSON_BODY] [EXTRA_HEADER...]
api() {
  local method="$1" path="$2" key="$3" body="${4:-}"
  shift 4 || true
  local args=(-sS -X "$method" -H "X-API-Key: $key" "$BASE$path")
  [ -n "$body" ] && args+=(-H "Content-Type: application/json" -d "$body")
  for h in "$@"; do args+=(-H "$h"); done
  curl "${args[@]}"
}

check() { # desc jq_filter json
  local desc="$1" filter="$2" json="$3"
  if echo "$json" | jq -e "$filter" >/dev/null 2>&1; then
    echo "   PASS: $desc"; PASS_N=$((PASS_N+1))
  else
    echo "   FAIL: $desc"; echo "$json" | jq . 2>/dev/null || echo "$json"; FAIL_N=$((FAIL_N+1))
  fi
}

echo ">> 1. 健康检查与鉴权"
check "健康检查 200" '.status=="ok"' "$(curl -sS "$BASE/healthz")"
check "无 Key 访问运营统计返回 401" '. == 401' "$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/api/v1/ops/stats")"
check "游客角色访问运营统计返回 403" '. == 403' "$(curl -sS -o /dev/null -w '%{http_code}' -H "X-API-Key: $CUSTOMER" "$BASE/api/v1/ops/stats")"

echo ">> 2. 景区时段调整为全天（便于冒烟）"
api POST /api/v1/ops/parks "$ADMIN" '{"id":"P001","name":"主峰景区","daily_cap":5000,"slot_start":"00:00","slot_end":"23:59"}' >/dev/null

echo ">> 3. 注册闸机（多入口）"
api POST /api/v1/devices/GATE-E "$ADMIN" '{"park_id":"P001","entrance":"东门"}' | jq -e '.id=="GATE-E"' >/dev/null
api POST /api/v1/devices/GATE-W "$ADMIN" '{"park_id":"P001","entrance":"西门"}' | jq -e '.id=="GATE-W"' >/dev/null
api POST /api/v1/devices/GATE-E/heartbeat "$STAFF" >/dev/null
api POST /api/v1/devices/GATE-W/heartbeat "$STAFF" >/dev/null

echo ">> 4. 购买单人年卡（实名+人脸+证件）"
CARD_JSON=$(api POST /api/v1/cards "$CUSTOMER" '{
  "type":"single","holder_name":"张三","phone":"13800000000","parks":["P001"],
  "members":[{"name":"张三","id_type":"idcard","id_number":"ID-ZS","id_expire_at":"2099-01-01",
              "phone":"13800000000","photo_ref":"photo/zs","photo_quality":"ok","role":"holder"}]}')
CID=$(echo "$CARD_JSON" | jq -r '.id')
MID=$(api GET "/api/v1/cards/$CID" "$STAFF" | jq -r '.members[0].id')
check "年卡创建成功且额度=60" '.annual_quota==60 and .status=="active"' "$CARD_JSON"

verify() { # request_id device [face_match]
  local rid="$1" dev="$2" match="${3:-match}"
  api POST /api/v1/gate/verify "$STAFF" "{\"request_id\":\"$rid\",\"device_id\":\"$dev\",
    \"park_id\":\"P001\",\"card_id\":\"$CID\",\"qr_code\":\"$CID\",\"member_id\":\"$MID\",
    \"face\":{\"photo_ref\":\"live\",\"quality\":\"ok\",\"match\":\"$match\"}}"
}

echo ">> 5. 正常入园，扣 1 人次"
D=$(verify req-1 GATE-E)
check "PASS 且剩余 59" '.decision=="PASS" and .deducted_count==1 and .quota_remaining==59' "$D"

echo ">> 6. 网络抖动：同一 request_id 重试不重复扣"
D=$(verify req-1 GATE-E)
check "幂等命中 replayed=true，不重复扣" '.replayed==true and .deducted_count==0' "$D"
check "卡已用额度仍为 1" '.card.quota_used==1' "$(api GET "/api/v1/cards/$CID" "$STAFF")"

echo ">> 7. 高频异常（同设备当日再次入园，单人年限次1）"
D=$(verify req-2 GATE-E)
check "转人工 MANUAL_CHECK + HIGH_FREQUENCY" '.decision=="MANUAL_CHECK" and (.reasons|index("HIGH_FREQUENCY"))' "$D"

echo ">> 8. 跨设备疑似转借（西门紧接着刷脸）"
D=$(verify req-3 GATE-W)
SUS_EID=$(echo "$D" | jq -r '.event_id')
check "SUSPECTED_TRANSFER" '.decision=="SUSPECTED_TRANSFER" and (.reasons|index("SUSPECTED_TRANSFER"))' "$D"
check "疑似转借流水进入人工队列" "map(.id) | index(\"$SUS_EID\") != null" "$(api GET /api/v1/manual/queue "$STAFF")"

echo ">> 9. 证件过期拒绝（新卡）"
EXPIRED=$(api POST /api/v1/cards "$CUSTOMER" '{"type":"single","holder_name":"李四","parks":["P001"],
  "members":[{"name":"李四","id_number":"ID-LS","id_expire_at":"2020-01-01","photo_ref":"p","role":"holder"}]}')
EID=$(echo "$EXPIRED" | jq -r '.id'); EMID=$(api GET "/api/v1/cards/$EID" "$STAFF" | jq -r '.members[0].id')
D=$(api POST /api/v1/gate/verify "$STAFF" "{\"request_id\":\"e1\",\"device_id\":\"GATE-E\",\"park_id\":\"P001\",
  \"card_id\":\"$EID\",\"qr_code\":\"$EID\",\"member_id\":\"$EMID\",\"face\":{\"quality\":\"ok\",\"match\":\"match\"}}")
check "DENY + ID_EXPIRED，不扣额度" '.decision=="DENY" and (.reasons|index("ID_EXPIRED")) and .deducted_count==0' "$D"

echo ">> 10. 照片不清 → 人工放行可追溯"
D=$(api POST /api/v1/gate/verify "$STAFF" "{\"request_id\":\"p1\",\"device_id\":\"GATE-E\",\"park_id\":\"P001\",
  \"card_id\":\"$CID\",\"member_id\":\"$MID\",\"face\":{\"quality\":\"unclear\",\"match\":\"unknown\"}}")
TICKET=$(echo "$D" | jq -r '.event_id')
check "转人工并生成工单" '.decision=="MANUAL_CHECK" and .manual_ticket!=""' "$D"
R=$(api POST /api/v1/manual/resolve "$STAFF" "{\"event_id\":\"$TICKET\",\"action\":\"confirm_pass\",
  \"operator\":\"王工\",\"note\":\"现场核对证件放行\",\"refund_quota\":true}")
check "人工放行返回 confirm_pass" '.action=="confirm_pass"' "$R"
EV=$(api GET "/api/v1/events/$TICKET" "$STAFF")
check "流水留痕（处理人/结论）" '.resolved==true and .verdict=="pass" and .resolution.operator=="王工"' "$EV"

echo ">> 11. 冻结不影响已预约订单"
TODAY=$(date +%F)
R=$(api POST /api/v1/reservations "$CUSTOMER" "{\"card_id\":\"$CID\",\"park_id\":\"P001\",\"date\":\"$TODAY\",
  \"slot\":\"AM\",\"member_ids\":[\"$MID\"]}")
check "预约成功" '.status=="booked"' "$R"
RID=$(echo "$R" | jq -r '.id')
api POST "/api/v1/cards/$CID/freeze" "$ADMIN" >/dev/null
D=$(verify fz-1 GATE-E)
check "冻结期间核验拒绝 CARD_FROZEN" '.decision=="DENY" and (.reasons|index("CARD_FROZEN"))' "$D"
check "冻结后预约仍为 booked" "map(select(.id==\"$RID\"))[0].status==\"booked\"" "$(api GET "/api/v1/cards/$CID/reservations" "$STAFF")"
api POST "/api/v1/cards/$CID/unfreeze" "$ADMIN" >/dev/null

echo ">> 12. 节假日限制（家庭年卡 2026-10-01 国庆不可用）"
FAM=$(api POST /api/v1/cards "$CUSTOMER" '{"type":"family","holder_name":"王五","parks":["P001"],
  "members":[{"name":"王五","id_number":"ID-WW","id_expire_at":"2099-01-01","photo_ref":"p","role":"holder"}]}')
FID=$(echo "$FAM" | jq -r '.id'); FMID=$(api GET "/api/v1/cards/$FID" "$STAFF" | jq -r '.members[0].id')
D=$(api POST /api/v1/gate/verify "$STAFF" "{\"request_id\":\"h1\",\"device_id\":\"GATE-E\",\"park_id\":\"P001\",
  \"card_id\":\"$FID\",\"member_id\":\"$FMID\",\"at\":\"2026-10-01T10:00:00+08:00\",
  \"face\":{\"quality\":\"ok\",\"match\":\"match\"}}" "X-Now: 2026-10-01T10:00:00+08:00")
check "DENY + HOLIDAY_RESTRICTED" '.decision=="DENY" and (.reasons|index("HOLIDAY_RESTRICTED"))' "$D"

echo ">> 13. 离线补报：去重且申诉可回补"
OFF=$(api POST /api/v1/cards "$CUSTOMER" '{"type":"single","holder_name":"赵六","parks":["P001"],
  "members":[{"name":"赵六","id_number":"ID-ZL","id_expire_at":"2099-01-01","photo_ref":"p","role":"holder"}]}')
OID=$(echo "$OFF" | jq -r '.id'); OMID=$(api GET "/api/v1/cards/$OID" "$STAFF" | jq -r '.members[0].id')
BATCH="{\"device_id\":\"GATE-OFF\",\"attempts\":[{\"request_id\":\"off-1\",\"device_id\":\"GATE-OFF\",
  \"park_id\":\"P001\",\"card_id\":\"$OID\",\"qr_code\":\"$OID\",\"member_id\":\"$OMID\",
  \"face\":{\"quality\":\"ok\",\"match\":\"match\"}}]}"
B=$(api POST /api/v1/gate/offline-batch "$STAFF" "$BATCH")
check "离线补报首次扣减" '.count==1 and .results[0].decision=="PASS"' "$B"
OFF_EID=$(echo "$B" | jq -r '.results[0].event_id')
B=$(api POST /api/v1/gate/offline-batch "$STAFF" "$BATCH")
check "整批重复上报被幂等拦截" '.results[0].replayed==true' "$B"
check "额度只扣一次" '.card.quota_used==1' "$(api GET "/api/v1/cards/$OID" "$STAFF")"
AP=$(api POST /api/v1/appeals "$CUSTOMER" "{\"card_id\":\"$OID\",\"event_ids\":[\"$OFF_EID\"],
  \"reason\":\"离线重复扣次\",\"detail\":\"闸机离线\"}")
APID=$(echo "$AP" | jq -r '.id')
AR=$(api POST "/api/v1/appeals/$APID/review" "$STAFF" '{"status":"approved","handler":"客服小李","note":"核实回补"}')
check "申诉成立回补 1 人次" '.status=="approved" and .refund_quota==1' "$AR"
check "卡额度恢复、回补累计=1" '.card.quota_used==0 and .card.quota_refunded==1' "$(api GET "/api/v1/cards/$OID" "$STAFF")"

echo ">> 14. 升级年卡重算权益与有效期"
UP=$(api POST /api/v1/cards "$CUSTOMER" '{"type":"single","holder_name":"孙七","parks":["P001"],
  "members":[{"name":"孙七","id_number":"ID-SQ","id_expire_at":"2099-01-01","photo_ref":"p","role":"holder"}]}')
UID_=$(echo "$UP" | jq -r '.id'); UMID=$(api GET "/api/v1/cards/$UID_" "$STAFF" | jq -r '.members[0].id')
api POST /api/v1/gate/verify "$STAFF" "{\"request_id\":\"u0\",\"device_id\":\"GATE-E\",\"park_id\":\"P001\",
  \"card_id\":\"$UID_\",\"member_id\":\"$UMID\",\"face\":{\"quality\":\"ok\",\"match\":\"match\"}}" >/dev/null
U=$(api POST "/api/v1/cards/$UID_/upgrade" "$CUSTOMER" '{"new_type":"family"}')
check "升级后额度240/已用1结转/同行4/节假日不可用" \
  '.annual_quota==240 and .quota_used==1 and .companion_limit==4 and .holiday_allowed==false' "$U"

echo ">> 15. 运营看板"
S=$(api GET /api/v1/ops/stats "$ADMIN")
check "统计含核验总量、峰值、权益使用率" '.total_events>0 and (.peak_by_hour|length>0) and (.quota_usage_by_card_type|length>0)' "$S"

echo
echo "================ 冒烟结果: PASS=$PASS_N FAIL=$FAIL_N ================"
[ "$FAIL_N" -eq 0 ]
