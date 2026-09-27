# 景区年卡实名入园 · 同行权益与异常核验服务

基于 **Go 1.19 + Fiber v2 + Redis（Lua 原子脚本）** 的后端服务，覆盖家庭年卡 / 单人年卡 /
亲子年卡的实名信息、人脸照片、证件、手机号、有效期、同行人数、可入园景点、节假日限制、
权益额度的全生命周期管理，以及多入口多设备并发闸机核验、异常原因归因、现场人工处理与
客服申诉的一致对账。

## 1. 能力一览

- **年卡档案**：实名成员（持卡人/配偶/孩子/老人）、人脸底库照片及质量、证件及有效期、
  绑定手机号（可换绑）、年卡有效期、同行人数上限、可入园景点、节假日是否可用、入园时段、
  年度权益总额度/已用/回补、每成员每日入园次数。
- **三种卡类型售卖规则**（可由运营调整）：
  - `single` 单人年卡：年度 60 人次，每日 1 次，节假日可用，同行 1 人；
  - `family` 家庭年卡：年度 240 人次，每日 2 次，**法定节假日不可用**，最多 4 人同行；
  - `parent` 亲子年卡：年度 120 人次，每日 1 次，1 成人 + 1 儿童。
- **闸机核验结论**：`PASS` 放行 / `MANUAL_CHECK` 人工核验 / `DENY` 权益不足或硬性拒绝 /
  `SUSPECTED_TRANSFER` 疑似转借。
- **明确异常原因码**（客服与现场可直接看到本次失败原因）：
  `CARD_FROZEN` 年卡冻结、`ID_EXPIRED` 证件过期、`PHOTO_UNCLEAR` 照片不清、
  `PHOTO_MISMATCH` 人脸不匹配、`FACE_EXEMPT_REQUIRED` 老人无法刷脸转人工、
  `COMPANION_UNBOUND` 同行人未绑定、`TEMP_COMPANION_INVALID` 临时同行授权无效/过期、
  `COMPANION_OVER_LIMIT` 同行超员、`QUOTA_INSUFFICIENT` 权益不足、
  `HOLIDAY_RESTRICTED` 节假日权益不适用、`PARK_NOT_ALLOWED` 景点不在范围、
  `OUTSIDE_TIME_SLOT` 非入园时段、`PARK_CAPACITY_FULL` 景区容量满、
  `DEVICE_OFFLINE` 设备离线、`HIGH_FREQUENCY` 高频异常、
  `SUSPECTED_TRANSFER` 多设备短时间移动疑似转借、`QR_INVALID` 二维码无效、
  `DUPLICATE_DEDUCTION` 重复请求幂等拦截、`CARD_EXPIRED` / `CARD_REFUNDED` / `CARD_NOT_FOUND`。
- **并发与一致性**：
  - 网络抖动重试以 `request_id` **幂等**，20 路并发同请求只扣 1 次权益（Lua + 事件键双重保障）；
  - 额度扣减、当日次数、跨设备移动检测、景区容量占用在**同一个 Lua 脚本**内原子完成；
  - 同设备当日超次数 → 高频异常转人工；不同设备 1 小时内连续入园 → 疑似转借；
  - 设备离线期间本地放行，联网后 `/gate/offline-batch` 补报，按设备指纹去重，
    **重复整批上报不会重复扣次**；
  - 申诉成立按流水逐条回补，已回补流水打标，杜绝重复回补。
- **人工与客服闭环**：转人工/疑似流水进入队列，现场 `confirm_pass`/`reject` 处理并留痕
  （操作人、备注、是否非游客过错免权益），流水结论变更同步修正运营计数；客服申诉关联
  流水，审批后回补额度，流水记录 `appeal_id`，保证“人工放行后客服可解释”。
- **冻结/退款/升级**：冻结只改卡状态，**已预约未入园订单不受影响**；退款卡作废、
  有效期截止当日、未使用预约取消；升级按新卡类型重算额度/同行人数/节假日权益，
  有效期自升级日顺延一年，已用额度结转。
- **运营看板**：按卡类型统计售卡量、权益使用率，按小时统计入园峰值，按结论/原因统计
  异常核验分布，景区当日入园量、待处理人工工单与申诉数；并可按数据调整售卖规则。

## 2. 目录结构

```
cmd/server/            服务入口
internal/config/       环境变量配置
internal/model/        领域模型、常量（决策/原因码）、请求 DTO
internal/store/        Redis 存储层 + Lua 原子扣减脚本 + 统计计数
internal/service/      卡生命周期、闸机核验、设备/预约/人工/申诉、运营统计
internal/middleware/   X-API-Key 角色鉴权
internal/api/          Fiber 路由与 HTTP 处理（含端到端测试）
scripts/smoke.sh       自包含端到端冒烟脚本
```

## 3. 本地启动

前置：Go ≥ 1.19、Redis ≥ 6（本机已验证 redis-server 7.0.15）、curl、jq（仅冒烟脚本需要）。

```bash
# 1) 启动 Redis（任选其一）
redis-server --port 6379            # 或使用系统服务

# 2) 启动服务
go run ./cmd/server
# 监听 :8080，默认连接 127.0.0.1:6379 DB0
```

可配置环境变量（均有默认值）：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `HTTP_ADDR` | `:8080` | HTTP 监听地址 |
| `REDIS_ADDR` | `127.0.0.1:6379` | Redis 地址 |
| `REDIS_DB` | `0` | Redis DB 编号 |
| `ADMIN_API_KEY` | `admin123` | 运营角色 Key |
| `STAFF_API_KEY` | `staff123` | 现场/闸机角色 Key |
| `CUSTOMER_API_KEY` | `customer123` | 游客角色 Key |
| `DEVICE_OFFLINE_SEC` | `90` | 心跳超时秒数，超时判定设备离线 |

服务首次启动会自动写入 3 个示例景区（P001 主峰/P002 峡谷/P003 亲子乐园）、
2026 国庆节假日和三类卡的默认售卖规则。

> 说明：人脸比对由闸机侧设备完成，接口以 `face.match = match|mismatch|unknown`、
> `face.quality = ok|unclear` 接收比对结论；`photo_ref` 为底库/现场照片在对象存储中的引用，
> 本服务聚焦核验决策与权益一致性，不内置人脸算法。

## 4. 测试账号（API Key）

| 角色 | Header | 用途 |
| --- | --- | --- |
| 运营 admin | `X-API-Key: admin123` | 冻结/解冻、退款、景区/节假日、售卖规则、运营统计、设备注册 |
| 现场/闸机 staff | `X-API-Key: staff123` | 闸机核验、离线补报、心跳、人工队列与处理、申诉审批 |
| 游客 customer | `X-API-Key: customer123` | 购卡、换绑手机号、换照片、临时同行、预约、升级、提交申诉 |

所有接口前缀 `/api/v1`，除 `/healthz` 外都需要 `X-API-Key`。

## 5. 快速验证

### 5.1 一键端到端冒烟（推荐）

脚本会自行编译、在独立端口拉起临时 Redis（6390）与服务（18080，DB15），
执行 25 项断言后自动清理：

```bash
./scripts/smoke.sh
# 期望结尾：================ 冒烟结果: PASS=25 FAIL=0 ================
```

覆盖：鉴权、正常扣次、幂等防重、高频、跨设备转借、证件过期、照片不清人工放行留痕、
冻结不影响预约、节假日限制、离线补报去重+申诉回补、升级重算、运营看板。

### 5.2 自动化测试（真实 Redis，含并发/竞态检测）

```bash
go test ./...                      # 用例会自行启动临时 redis-server
go test -race -count=3 ./...       # 竞态检测 + 重复 3 轮
go vet ./...
```

### 5.3 手动调用示例

```bash
# 注册设备 + 心跳
curl -s -XPOST localhost:8080/api/v1/devices/D1 -H 'X-API-Key: admin123' \
  -H 'Content-Type: application/json' -d '{"park_id":"P001","entrance":"东门"}'
curl -s -XPOST localhost:8080/api/v1/devices/D1/heartbeat -H 'X-API-Key: staff123'

# 购买单人年卡
curl -s -XPOST localhost:8080/api/v1/cards -H 'X-API-Key: customer123' \
  -H 'Content-Type: application/json' -d '{
    "type":"single","holder_name":"张三","phone":"13800000000","parks":["P001"],
    "members":[{"name":"张三","id_number":"ID-ZS","id_expire_at":"2099-01-01",
                "photo_ref":"photo/zs","photo_quality":"ok","role":"holder"}]}'

# 闸机核验（card_id/member_id 用上一步返回值替换）
curl -s -XPOST localhost:8080/api/v1/gate/verify -H 'X-API-Key: staff123' \
  -H 'Content-Type: application/json' -d '{
    "request_id":"req-1","device_id":"D1","park_id":"P001",
    "card_id":"<cardId>","qr_code":"<cardId>","member_id":"<memberId>",
    "face":{"photo_ref":"live","quality":"ok","match":"match"}}'

# 用完全相同的 request_id 再请求一次 -> replayed=true，不重复扣权益

# 运营看板
curl -s localhost:8080/api/v1/ops/stats -H 'X-API-Key: admin123' | jq .
```

## 6. 主要接口

| 方法 & 路径 | 角色 | 说明 |
| --- | --- | --- |
| `POST /cards` | customer | 购卡（可带多名实名成员） |
| `GET /cards/:id` | 全部 | 卡 + 成员 + 临时授权聚合视图 |
| `POST /cards/:id/members` | customer | 追加绑定家庭成员（受同行上限） |
| `PUT /cards/:id/phone` | customer | 换绑手机号 |
| `PUT /cards/:id/members/:mid/photo` | customer | 更新人脸底库（孩子长大等） |
| `POST /cards/:id/temp-grants` | customer | 家庭成员临时更换的限期实名授权 |
| `POST /cards/:id/freeze` `/unfreeze` | admin | 冻结/解冻（不影响已预约订单） |
| `POST /cards/:id/refund` | admin | 退款作废、有效期截止当日 |
| `POST /cards/:id/upgrade` | customer | 升级卡类型并重算权益/有效期 |
| `POST /reservations` `GET /cards/:id/reservations` | customer/全部 | 预约与查询 |
| `POST /gate/verify` | staff | 闸机核验，返回决策 + 原因 + 剩余额度 |
| `POST /gate/offline-batch` | staff | 离线期间流水补报（去重防重复扣次） |
| `GET /manual/queue` `POST /manual/resolve` | staff | 人工队列与现场处理 |
| `POST /devices/:id` `/heartbeat` `GET /devices/:id` | admin/staff | 设备注册、心跳、在线状态 |
| `POST /appeals` `POST /appeals/:id/review` | customer/admin+staff | 申诉与审批回补 |
| `GET /events/:id` `GET /cards/:id/events` `GET /parks/:pid/events` | admin+staff | 核验流水追溯 |
| `GET /ops/stats` | admin | 卡类型/峰值/异常/权益使用率看板 |
| `GET|PUT /ops/sale-rules/:type` | admin | 查询/调整售卖规则 |
| `GET|POST /ops/parks` `GET|POST /ops/holidays` | admin | 景区容量/时段、节假日日历 |

`POST /gate/verify` 支持可选头 `X-Now: RFC3339` 覆盖服务端时钟（用于节假日/时段演练与
自动化测试的确定性验证）。

## 7. 核验决策与扣减规则要点

1. 先做**幂等判重**：同 `request_id` 直接返回首次结论（`replayed=true`），绝不重复扣。
2. 再做**静态校验**：设备在线、卡状态/有效期、景点范围、节假日、入园时段、二维码/预约、
   主核验人与同行人的实名绑定与证件有效期、临时授权有效期、同行人数；老人 `face_exempt`
   或照片不清/无法判定转人工，人脸不匹配判疑似转借。
3. 静态项通过后进入 **Lua 原子提交**，顺序为：幂等 → 卡状态 → 剩余额度 →
   跨设备移动（疑似转借）→ 当日高频次数 → 景区容量 → 一次性写入扣减/计数/容量/移动轨迹。
4. 转人工与疑似流水**预先不扣权益**；现场人工放行时补登在园容量，`refund_quota=true`
   表示非游客过错的免权益放行并在流水留痕。
5. 离线补报以 `设备ID + request_id（缺省用 设备|卡|人|分钟 指纹）` 去重；
   申诉仅对“已扣且未回补”的流水回补，保证卡权益、闸机日志、现场处理、客服申诉四方一致。

## 8. 已执行的验证结论

- `go build ./...`、`go vet ./...`：通过。
- `go test ./...`：全部通过；`go test -race -count=3 ./internal/api/`：通过，无数据竞争。
  测试用例真实拉起独立 redis-server，覆盖并发幂等、高频/转借、冻结预约、证件/照片/同行/
  节假日/容量/额度、离线重复扣次回补、人工放行追溯、退款/升级、运营统计等 19 个测试用例。
- `./scripts/smoke.sh`：连续两次运行均为 `PASS=25 FAIL=0`。
