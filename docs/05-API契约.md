# 05-API契约

> 状态：**活文档**。转发面契约已于 M1-wave2 落地并细化（见第四节）；管理面契约已于 M2-wave1 落地并细化（见第五节，登录会话 + 六组端点）；后续波次增量更新。

## 一、端点清单（规划）

### 转发面（客户端调用，令牌鉴权）

| 端点 | 协议 | 说明 |
| --- | --- | --- |
| `POST /v1/chat/completions` | OpenAI 兼容 | 对话补全，支持 `stream: true` |
| `POST /v1/messages` | Anthropic Messages | 消息协议，支持流式 |
| `POST /v1/messages/count_tokens` | Anthropic Messages | 输入 tokens 计数 |
| `GET /v1/models` | OpenAI 兼容 | 模型清单（聚合各渠道可用模型） |

### 管理面（`/api/*`，管理员鉴权）

| 端点组 | 说明 | 落地 |
| --- | --- | --- |
| `/api/user/login` `logout` | 登录（bcrypt）/登出，HMAC 签名会话 cookie | ✅ M2-wave1 |
| `/api/user` `user/:id` | 用户 CRUD（root 管理，改密失效旧会话，防自锁） | ✅ M2-wave1 |
| `/api/channel` `channel/:id` | 渠道 CRUD（key 脱敏，整对象幂等写，写后熔断复位） | ✅ M2-wave1 |
| `/api/channel/test/:id` | 渠道连通测试（最小请求 + response_time） | ✅ M2-wave1 |
| `/api/token` `token/:id` | 令牌 CRUD（明文仅返回一次；tpm_rpm/分组/预算周期字段） | ✅ M2-wave1 |
| `/api/redemption` | 兑换码批量生成/列表/删除（核销状态机 wave3） | ✅ M2-wave1 |
| `/api/log` | 请求日志分页查询（user/token/channel/model/时间过滤） | ✅ M2-wave1 |
| `/api/option` | options 读写（键白名单，写后 Reload() 热生效） | ✅ M2-wave1 |
| `/api/user/topup` `/api/user/self` | 兑换码核销 / 当前用户信息（登录态即可，非 root） | ✅ M2-wave3 |
| `/api/token/:id/assign` | 额度划转（用户余额 → 令牌余额，登录态即可） | ✅ M2-wave3 |
| `/api/token/mine` | 名下令牌列表（登录态，所有权作用域，白名单字段） | ✅ M2 缺陷修复 |
| `/api/status` | 服务状态、版本、schema 版本（无鉴权） | ✅ M0 |

## 二、契约约定（总体）

1. **鉴权**：转发面 `Authorization: Bearer sk-xxx`，服务端 SHA-256 后查 `key_hash`；管理面 HMAC-SHA256 签名会话 cookie（`session`，HttpOnly+SameSite=Lax，TTL 7d），`users.auth_version` 纳入中间件比对（改密递增即失效旧会话），管理端点要求 root（role=100）；
2. **错误结构**：转发面错误沿用入口协议的错误形状（OpenAI error object / Anthropic error object），保证客户端 SDK 无感；管理面统一 `{success, message, data}` 包裹；
3. **流式**：SSE 转发，逐块透传，计费在流结束时结算；
4. **幂等**：管理面写操作支持 `Idempotency-Key` 头（M3 落地）；
5. **版本化**：转发面路径固定为 `/v1`，破坏性变更走 `/v2`；
6. **契约测试**：每个端点落地时必须附请求/响应示例与错误码表（写入本文对应小节）。

## 三、细化计划

- [x] M1-wave2：`/v1/chat/completions`、`/v1/models` 契约细化 + 示例（2026-09-02）
- [x] M1-wave2：`/v1/messages`、`/v1/messages/count_tokens` 契约细化 + 示例（2026-09-02）
- [x] M1-wave3：计费错误码（403/503）与计费运行轨键补充（2026-09-02）
- [x] M2-wave1：管理面契约细化——登录/会话 + 六组端点 + 错误码表（2026-09-03）
- [x] M2-wave3：用户自服务端点契约细化——topup/self/assign + hooks 运行轨键与事件投递
  （核销入口落地为 `POST /api/user/topup` 而非设计稿的 `/api/redemption/redeem`：核销是
  用户侧动作，归入用户自服务组）（2026-09-03）
- [ ] M4：管理面 `Idempotency-Key` 头语义复核（当前以 PUT 整对象幂等写替代）

## 四、转发面契约（M1-wave2 落地，M1-wave3 补计费）

### 4.1 通用约定

- **鉴权**：OpenAI 面取 `Authorization: Bearer <key>`；Anthropic 面优先 `x-api-key: <key>`，缺省时回退 Bearer。服务端 SHA-256 后查 `tokens.key_hash`（内存缓存 TTL 30s + 负缓存 5s + 写时失效 + singleflight 防击穿）；禁用/过期令牌返回 403，无效返回 401。
- **错误结构**：错误一律按入口协议形状输出（OpenAI error object / Anthropic error object），客户端 SDK 无感；`error.type`/`code` 用语义短横线标识（见 4.5 错误码表）。
- **流式**：`stream: true` 时以 SSE 逐事件转发（事件边界即 flush，不缓冲整段、不改写事件原文）；OpenAI 面流式请求自动注入 `stream_options.include_usage=true` 以取尾 chunk 用量（客户端会多收到一个 usage chunk）。
- **重试与熔断**：上游错误按类重试（Auth 零重试；RateLimit 指数退避；其余立即换点），重试仅限首字节前，排除集上限 5；重试穷尽或不可重试时透传上游错误（Auth 类包装为 502）。熔断只隔离单渠道（进程内存态）。
- **请求体限制**：入口请求体超过上限（默认 32MB，运行轨 `relay.max_body_bytes` 可调）本地快速失败 413。
- **计费**（M1-wave3）：请求前按估算上浮 20% 冻结令牌余额（不足 403 拒绝）；响应完成后按实际 usage 多退少补（补扣允许透支到负数）；上游失败/流中断全额退款；usage 缺失按本地粗估计费并标记 `estimated`（logs.detail 可见）；模型未配价 503 拒绝。
- **运行轨配置键**：`relay.max_body_bytes`（请求体上限）；`relay.virtual_model_groups`（虚拟模型组 JSON `{"组名":["成员",...]}`，`/v1/models` 只返回组名）；计费键 `billing_setting.billing_mode` / `billing_setting.billing_expr` / `billing_setting.billing_price`（模型名 → 模式声明 / 计费表达式 / 按次单价）与 `ModelRatio` / `CompletionRatio` / `GroupRatio`（classic 回退价与组倍率），管理面写后 Reload() 热生效。观测键（M2-wave3）：`hooks.enabled` / `hooks.otlp.endpoint` / `hooks.webhook.url`（见 5.6）。

### 4.2 POST /v1/chat/completions（OpenAI 兼容）

请求（与 OpenAI 兼容，`model` 为路由依据）：

```json
{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "你好"}], "stream": false}
```

非流式响应：上游 2xx 响应体整体透传（`usage` 字段原样保留，服务端另行解析供计费/日志使用）。

流式响应（`Content-Type: text/event-stream`）：逐事件透传上游 SSE，事件间无额外改写；尾 chunk 携带 `usage`，最后以 `data: [DONE]` 结束。

### 4.3 POST /v1/messages 与 POST /v1/messages/count_tokens（Anthropic Messages）

请求头：`x-api-key`（客户端令牌）+ `anthropic-version`（透传；客户端未携带时注入默认 `2023-06-01`）。

非流式响应：上游 2xx 响应体整体透传；`usage.input_tokens` 含 `cache_read_input_tokens`（服务端计费视角已归一）。

流式响应：透传 `message_start` / `content_block_delta` / `message_delta` / `message_stop` 事件序列；用量取 `message_start`（输入侧，含 cache_read）与 `message_delta`（`output_tokens` 为累计值，直接覆盖）。

count_tokens：恒为非流式，整体透传；`input_tokens` 即输入侧用量。

### 4.4 GET /v1/models（OpenAI 兼容形状）

鉴权同转发面（Bearer 或 x-api-key）。响应为启用渠道模型清单并集 + 虚拟模型组名（组内成员不展开、通配渠道不展开），按名称排序去重：

```json
{"object": "list", "data": [{"id": "gpt-4o-mini", "object": "model", "owned_by": "hui-api"}]}
```

### 4.5 错误码表（转发面语义错误）

| HTTP | code/type | 场景 |
| --- | --- | --- |
| 400 | `invalid_request` | 请求体非法 JSON / 缺 `model` 字段 |
| 401 | `missing_api_key` / `invalid_api_key` | 未携带 / 无效令牌 |
| 403 | `token_disabled` / `token_expired` | 令牌禁用 / 过期 |
| 403 | `insufficient_quota` | 令牌余额不足以预扣冻结（无副作用） |
| 413 | `body_too_large` | 请求体超过本地上限 |
| 502 | `upstream_auth_failed` | 渠道密钥错误（上游 401/403，零重试） |
| 502 | `upstream_unreachable` / `upstream_error` | 上游网络不可达 / 重试穷尽后构造请求失败 |
| 503 | `no_available_channel` | 模型无可用渠道（含全部熔断中 / 渠道组不匹配） |
| 503 | `model_not_priced` | 模型未配置价格 / 价格配置不可用（服务端配置问题，错误信息不含内部细节） |
| 透传 | 上游原样 | 重试穷尽或不可重试的非 Auth 上游错误（保持状态码与响应体） |

## 五、管理面契约（M2-wave1 落地）

### 5.1 通用约定

- **鉴权**：除 `/api/user/login`、`/api/user/logout`、`/api/status` 外全部要求 root 会话（签名 cookie，缺失/篡改/过期/禁用/auth_version 不匹配 → 401/403）；例外：用户自服务组（`/api/user/topup`、`/api/user/self`、`/api/token/:id/assign`、`/api/token/mine`）仅要求登录态（RequireAuth，普通用户可访）。
- **响应包裹**：成功 `{"success":true,"message":"","data":...}`，失败 `{"success":false,"message":"...","code":"语义码"}`；创建类返回 201，其余 200；连通测试结果语义不落库不影响熔断（HTTP 恒 200）。
- **幂等写**：PUT 为整对象幂等替换——显式字段含零值全部生效，同 body 重复 PUT 响应体一致；缺省归一化：status 0→启用、token.expired_time 0→永久（`-1`）、group 空→`default`、user.role 0→普通用户；channel.key 空=保留旧值（唯一例外，防回显脱敏值覆盖明文）；token 的 key/key_hash/user_id 与 root 自身 role/status 不可经 PUT 修改。
- **分页**：`?page=1&page_size=20`（page_size 上限 100），响应 `data.items/total/page/page_size`；列表排序：channel/option 按 id 升序或 key 字典序，token/redemption/log 按 id 降序。
- **写后失效**：渠道写后复位熔断（`Breaker.Reset`）；令牌写后失效鉴权缓存（`Auth().Invalidate`）；options 写后 `Runtime.Reload()` 热生效（响应含新 `version`）。

### 5.2 端点明细

| 端点 | 请求要点 | 响应 data 要点 / 特殊语义 |
| --- | --- | --- |
| `POST /api/user/login` | `{username,password}` | `{id,username,display_name,role}` + Set-Cookie；用户名不存在与密码错误同一错误（401 `invalid_credentials`） |
| `POST /api/user/logout` | - | 清除会话 cookie |
| `GET/POST/PUT/DELETE /api/user` | 创建必填 username+password（bcrypt）；改密非空即重置并递增 auth_version | 用户名重复 409 `username_conflict`；root 自改 role/status 400 `self_lockout`；删管理员 400 `delete_admin_forbidden`；删除用户级联删其令牌 |
| `GET/POST/PUT/DELETE /api/channel` | name/base_url 必填，type 1=OpenAI 兼容 2=Anthropic | 响应 key 恒脱敏（首 3+`***`+末 4），明文不序列化输出；写后熔断复位 |
| `POST /api/channel/test/:id` | - | `{success,status_code,time_ms,message}`；按渠道类型置鉴权头（Anthropic x-api-key，其余 Bearer），10s 超时 |
| `GET/POST/PUT/DELETE /api/token` | user_id 必填且存在；quota/remain/unlimited/budget_duration/tpm_rpm/tags/group/model_limits/allow_ips/expired_time | 创建响应 `data.key` 为明文（`sk-`+32hex）**仅此一次**；remain 缺省=quota；group 缺省取用户分组再退 default；写后鉴权缓存失效 |
| `GET/POST/DELETE /api/redemption` | 批量生成 `{count:1..100,name,quota>0,expired_time}` | `data.keys` 明文数组（`redd-`+24hex）仅此一次；key 冲突自动重试，重试穷尽整批拒绝 |
| `GET /api/log` | 过滤：`user_id/token_id/channel_id/model_name/start_timestamp/end_timestamp`（Unix 秒闭区间） | id 降序；channel_id 已生效（M2-wave3 日志回填实际服务渠道） |
| `GET/PUT /api/option` | PUT `{"options":{k:v}}` | 键白名单：`relay.*`/`billing_setting.*`/`hooks.*` 前缀 + `ModelRatio`/`CompletionRatio`/`GroupRatio`/`ModelRequestRateLimitEnabled/DurationMinutes/Count/SuccessCount/Group` 精确键（拒 `schema_version`）；值长 ≤2048；任一非法整体拒绝；写后返回新 `version` 且配置热生效 |
| `POST /api/user/topup` | `{key}`（登录态） | `{quota_added,user_quota}`；事务原子核销（条件 UPDATE 未用→已用防并发重复）→ 面额入账 → topup 日志；过期码惰性标记 status=4 并 400 `redemption_expired` |
| `GET /api/user/self` | -（登录态） | 当前用户对象（同用户视图字段，password_hash 序列化豁免） |
| `POST /api/token/:id/assign` | `{quota>0}`（登录态；归属者或管理员） | `{quota_assigned,remain_quota}`；用户余额 → 令牌 remain+quota 同步增加的转移事务；余额不足 400 `insufficient_quota`；unlimited 令牌 400 拒绝 |
| `GET /api/token/mine` | -（登录态） | 当前用户名下令牌分页列表（形状同管理列表 items/total/page/page_size，id 降序）；所有权作用域强制取会话用户（user_id 查询参数被忽略）；响应为白名单字段——不含 key/key_hash（密钥材料）与 tpm_rpm/tags/allow_ips（管理配置字段） |

### 5.3 错误码表（管理面语义错误）

| HTTP | code | 场景 |
| --- | --- | --- |
| 400 | `invalid_request` | 请求体非法 / 必填缺失 / count 越界 / self_lockout 触发条件不满足等参数问题 |
| 401 | `invalid_credentials` | 登录口令错误（与账号不存在同文案） |
| 401 | `unauthorized` | 未登录 / 会话无效 / 篡改 / 过期 / auth_version 不匹配 |
| 403 | `forbidden` | 非 root 访问管理端点；划转操作非令牌归属者且非管理员 |
| 403 | `user_disabled` | 已禁用账号登录 |
| 404 | `not_found` | 路径 id 不存在（或已删除）；兑换码不存在 |
| 409 | `username_conflict` | 用户名重复 |
| 409 | `redemption_used` | 兑换码已被使用（含并发竞争失败） |
| 400 | `redemption_expired` / `redemption_voided` | 兑换码已过期 / 已作废 |
| 400 | `insufficient_quota` | 划转时用户余额不足（管理面语义，与转发面 403 区分归因） |
| 429 | `rate_limited`（转发面） | 限流触发，响应带 `Retry-After`（见 4.1/限流语义） |
| 500 | `*_failed` | 存储层写失败（create/update/delete/query_failed） |

### 5.4 限流契约（M2-wave1，转发面）

- **配置键**（options，写后热生效）：`ModelRequestRateLimitEnabled`（bool）、`ModelRequestRateLimitDurationMinutes`（int）、`ModelRequestRateLimitCount`（int，周期内最大请求数）、`ModelRequestRateLimitSuccessCount`（int，周期内最大成功数）、`ModelRequestRateLimitGroup`（JSON `{"组名":[最大请求数,最大成功数]}`，按令牌 group 覆盖全局，共用周期）。
- **身份键**：全局 `g|<ClientIP>`；分组 `grp:<组名>|<ClientIP>`；令牌 TPM/RPM `tok:<token_id>`（滑动窗口，`tpm_rpm` JSON 如 `{"tpm":100000,"rpm":30}`）。
- **行为**：超限返回 429 + `Retry-After`（秒）；被拒请求不记录、不消耗配额、不计入上游熔断失败计数；成功数在 Respond 成功后记录（`RecordSuccess`）。

### 5.5 前端控制台对接注记（M2-wave2）

- **会话与 401**：管理面请求一律携带同源签名 cookie（fetch `credentials: 'same-origin'`）；
  前端不在 localStorage 存会话凭据，仅存展示用元数据（用户名/角色）。api 客户端对 401 统一
  跳转 /login（登录接口自身豁免，防口令错误误跳）。
- **鉴权双体系**：`/v1/models` 等转发面端点走 Bearer 令牌鉴权，与管理台会话无关——模型广场
  页需手工输入令牌（仅存 localStorage）。「管理员聚合查询」端点属新契约，本版未实现。
- **PUT 整对象写的前端义务**：PUT 全量覆盖，前端表单必须显式携带全部业务字段（含零值）——
  渠道 key 留空=保旧（脱敏回显 `sk-***XXXX` 不可回填）；令牌编辑显式携带 remain_quota 防
  误重置；用户编辑自己时 role/status 不渲染表单项，payload 必须原值回传，否则 self_lockout
  检查拦截任何编辑（含改邮箱）。
- **options 键白名单双侧同步**：系统设置页可编辑键集合与 `internal/api/option.go`
  allowedOptionKey 对齐（`relay.*`/`billing_setting.*`/`hooks.*` 前缀 + ModelRatio/CompletionRatio/
  GroupRatio/ModelRequestRateLimit* 五精确键）；新增可写键必须前后端同步，否则保存即 400
  `option_forbidden`；换皮类键（SystemName 等）后端不支持，前端不提供编辑。
- **充值页（M2-wave3，M2 缺陷修复更新）**：/console/topup 调 `GET /api/user/self` +
  `GET /api/token/mine`（所有权作用域端点，登录态即可；原对接的管理列表
  `GET /api/token?user_id=` 为 root 专属，普通用户 403）+ `POST /api/user/topup` +
  `POST /api/token/:id/assign`；兑换码状态枚举补 4=已过期（REDEMPTION_STATUS，与后端惰性
  标记语义对齐）。
- **会话探针与角色菜单（M2 缺陷修复）**：ConsoleLayout 会话探针用 `GET /api/user/self`
  （最低权限端点；用管理端点探针会把普通用户 403「需要管理员权限」误判为会话失效，
  详见 docs/11 踩坑）；菜单按角色渲染——渠道/用户/兑换码/系统设置为 root 专属
  （ADMIN_ONLY_KEYS），普通用户见数据看板/令牌/充值/模型广场/日志，直访专属路径回看板。

### 5.6 hooks 事件投递契约（M2-wave3）

- **总开关与键**（options，热生效）：`hooks.enabled`（"true" 启用）；`hooks.otlp.endpoint`
  （如 `http://127.0.0.1:4318`，导出 POST `<endpoint>/v1/metrics`）；`hooks.webhook.url`
  （事件 POST 推送地址）。
- **事件模型**：转发面每请求恰投递一事件——`completed`（Data 含 quota/prompt_tokens/
  completion_tokens/duration_ms/stream，粗估时附 estimated）或 `failed`（Err 非空，如
  `no_available_channel`/`request_not_completed` 兕底）；公共字段 type/request_id/token_id/
  channel_id/model/timestamp/idempotency_key（= requestID:eventType，供接收端去重）。
- **webhook**：JSON POST，超时 3s，失败静默丢弃并计数（不重试、不阻塞转发）。
- **OTLP**：OTLP/HTTP JSON，累计 temporality（startTime 固定、全量累计）；指标
  `hui.request.duration_ms`（histogram，ms）/`hui.request.tokens`（histogram，kind=
  prompt|completion）/`hui.request.status`（monotonic sum，model/outcome 维度）；2s 定时
  批量导出，失败丢弃计数，停机冲刷；int64 字段按 protobuf JSON 规范以字符串编码。
- **信任边界**：两 hook 均产生出站请求，目标地址由管理员配置——可达性与安全边界由配置方
  负责（本机 collector/webhook 接收端属正常用法）。
