# 05-API契约

> 状态：**活文档**。转发面契约已于 M1-wave2 落地并细化（见第四节）；管理面契约已于 M2-wave1 落地并细化（见第五节，登录会话 + 六组端点）；公开注册体系契约已于 M3-wave1 落地并细化（见 5.7）；在线充值与邀请返利契约已于 M3-wave3 落地并细化（见 5.10）；后续波次增量更新。

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
| `/api/user/stats` | 今日自服务统计（登录态，当前用户请求/消耗/tokens 与模型分布） | ✅ M2 收官 |
| `GET /api/setup` | 注册能力发现（公开）：注册/邮箱验证开关、Turnstile site key、OAuth 可用性 | ✅ M3-wave1 |
| `POST /api/user/register` | 用户注册（公开，开关 + IP 限频 + 人机校验 + 邮箱验证码 + aff 邀请奖励） | ✅ M3-wave1 |
| `POST /api/verification_code` | 发送邮箱验证码（公开，SMTP 就绪性门控 + 重发限频） | ✅ M3-wave1 |
| `POST /api/user/reset_password` | 邮箱验证码重置密码（公开，验码 + bcrypt + auth_version++） | ✅ M3-wave1 |
| `/api/status` | 服务状态、版本、schema 版本、features 特性开关块（无鉴权） | ✅ M0（features M3-wave1） |
| `POST /api/user/topup/order` | 在线下单（登录态）：网关开关 + 金额区间校验 → 汇率快照换算额度 → pending 订单 + 支付跳转 URL | ✅ M3-wave3 |
| `GET /api/user/topup/orders` | 本人充值订单分页（登录态，所有权作用域） | ✅ M3-wave3 |
| `GET /api/pay/epay/notify` `return` | EPay 异步通知（验签 + 幂等结算，纯文本 success/fail）/ 同步回跳 302（公开） | ✅ M3-wave3 |
| `POST /api/pay/stripe/webhook` | Stripe webhook（验签 + checkout.session.completed 幂等结算，公开） | ✅ M3-wave3 |
| `GET /api/user/aff` | 邀请信息：邀请码/邀请人数/累计返利/返利比例（登录态） | ✅ M3-wave3 |

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
- [x] M2 收官：自服务统计端点 `/api/user/stats` 契约细化 + 看板按角色取数注记（2026-09-03）
- [x] M3-wave1：公开注册体系契约细化——setup/register/verification_code/reset_password
  四端点 + /api/status features 块 + options 商业化前缀与脱敏哨兵（2026-09-03）
- [x] M3-wave3：在线充值订单与支付回调契约——下单/订单列表/epay notify+return/
  stripe webhook + 邀请信息端点（2026-09-03）
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

- **鉴权**：除 `/api/user/login`、`/api/user/logout`、`/api/status` 外全部要求 root 会话（签名 cookie，缺失/篡改/过期/禁用/auth_version 不匹配 → 401/403）；例外：用户自服务组（`/api/user/topup`、`/api/user/self`、`/api/user/stats`、`/api/token/:id/assign`、`/api/token/mine`，及 M3-wave2 的 `/api/user/password`、`/api/user/email`、`/api/user/totp/*`、`/api/user/identities*`、`/api/oauth/:provider/bind`，及 M3-wave3 的 `/api/user/topup/order`、`/api/user/topup/orders`、`/api/user/aff`，及 M3-wave4 的 `/api/log/mine`）仅要求登录态（RequireAuth，普通用户可访）；公开组（M3-wave1：`/api/setup`、`/api/user/register`、`/api/verification_code`、`/api/user/reset_password`；M3-wave2：`/api/oauth/:provider`、`/api/oauth/:provider/callback`、`/api/user/login/2fa`）无会话要求，安全边界由开关门控 + IP 限频 + 人机校验/验证码/state cookie 承担（见 5.7/5.8）；M3-wave3 支付回调（`/api/pay/epay/notify`、`/api/pay/epay/return`、`/api/pay/stripe/webhook`）无会话要求，安全边界由签名验签 + 金额逐位校验承担（见 5.10）。**半登录态（M3-wave2）**：TOTP 启用用户的 stage1 会话（Stage≠0）只能访问 `/api/user/login/2fa`，访问其他 RequireAuth 端点一律 401 `totp_required`（防半登录提权，见 5.9）。
- **响应包裹**：成功 `{"success":true,"message":"","data":...}`，失败 `{"success":false,"message":"...","code":"语义码"}`；创建类返回 201，其余 200；连通测试结果语义不落库不影响熔断（HTTP 恒 200）。
- **幂等写**：PUT 为整对象幂等替换——显式字段含零值全部生效，同 body 重复 PUT 响应体一致；缺省归一化：status 0→启用、token.expired_time 0→永久（`-1`）、group 空→`default`、user.role 0→普通用户；channel.key 空=保留旧值（唯一例外，防回显脱敏值覆盖明文）；token 的 key/key_hash/user_id 与 root 自身 role/status 不可经 PUT 修改。
- **分页**：`?page=1&page_size=20`（page_size 上限 100），响应 `data.items/total/page/page_size`；列表排序：channel/option 按 id 升序或 key 字典序，token/redemption/log 按 id 降序。
- **写后失效**：渠道写后复位熔断（`Breaker.Reset`）；令牌写后失效鉴权缓存（`Auth().Invalidate`）；options 写后 `Runtime.Reload()` 热生效（响应含新 `version`）。

### 5.2 端点明细

| 端点 | 请求要点 | 响应 data 要点 / 特殊语义 |
| --- | --- | --- |
| `POST /api/user/login` | `{username,password}` | TOTP 未启用：`{id,username,display_name,role}` + Set-Cookie 完整会话；TOTP 启用：`{require_2fa:true}` + Set-Cookie stage1 会话（TTL 5min，仅可用于 /api/user/login/2fa，见 5.9）；用户名不存在与密码错误同一错误（401 `invalid_credentials`） |
| `POST /api/user/login/2fa` | `{code}`（公开；凭 stage1 会话） | 验码通过 → 重签完整会话 `{id,username,display_name,role}`；错码 400 `totp_code_invalid`；与密码登录共用 `login\|<IP>` 限频（见 5.9） |
| `GET /api/oauth/:provider` | -（公开；provider∈github/linuxdo/oidc） | 302 authorize + Set-Cookie 一次性 state cookie（5min HttpOnly）；未配置 404 `oauth_not_configured`（见 5.8） |
| `GET /api/oauth/:provider/callback` | `?code&state`（公开） | 登录模式成功 302 /console（命中身份或自动建户）；bind 模式成功 302 /console/profile；任一失败 302 `/login?oauth_failed=1`（见 5.8） |
| `GET /api/oauth/:provider/bind` | -（登录态） | 同 authorize 发起，但 state cookie 标记 bind+当前 uid；回调成功后绑定当前用户（见 5.8） |
| `GET /api/user/identities` | -（登录态） | `{items:[{id,user_id,provider,provider_uid,created_time}]}` 本人身份列表 |
| `DELETE /api/user/identities/:id` | -（登录态） | 解绑（归属校验，非本人 404）；无口令且最后一个身份 400 `identity_last`（防锁死） |
| `POST /api/user/totp/setup` | -（登录态） | `{secret, otpauth_uri}`（issuer="Hui Api"，account=用户名）；secret 落库 enabled=0 待确认（见 5.9） |
| `POST /api/user/totp/enable` | `{code}`（登录态） | 验码通过 → enabled=1；未 setup 400 `totp_not_setup`、已启用 400 `totp_already_enabled`、错码 400 `totp_code_invalid` |
| `POST /api/user/totp/disable` | `{code}`（登录态） | 验码通过 → secret/enabled 双列清空；未启用 400 `totp_not_enabled`、错码 400（密钥保留可重试） |
| `POST /api/user/password` | `{old_password?,new_password}`（登录态） | 有口令账号必验旧口令（400 `old_password_mismatch`）；OAuth 建户无口令账号首次设置免验（留空）；≥6 位；成功后 auth_version++（其他会话全失效）并重签当前会话 |
| `POST /api/user/email` | `{email}`（登录态） | 格式校验 + 查重（409 `email_conflict`）；成功 `{email}` |
| `POST /api/user/logout` | - | 清除会话 cookie |
| `GET/POST/PUT/DELETE /api/user` | 创建必填 username+password（bcrypt）；改密非空即重置并递增 auth_version | 用户名重复 409 `username_conflict`；root 自改 role/status 400 `self_lockout`；删管理员 400 `delete_admin_forbidden`；删除用户级联删其令牌 |
| `GET/POST/PUT/DELETE /api/channel` | name/base_url 必填，type 1=OpenAI 兼容 2=Anthropic | 响应 key 恒脱敏（首 3+`***`+末 4），明文不序列化输出；写后熔断复位 |
| `POST /api/channel/test/:id` | - | `{success,status_code,time_ms,message}`；按渠道类型置鉴权头（Anthropic x-api-key，其余 Bearer），10s 超时 |
| `GET/POST/PUT/DELETE /api/token` | user_id 必填且存在；quota/remain/unlimited/budget_duration/tpm_rpm/tags/group/model_limits/allow_ips/expired_time | 创建响应 `data.key` 为明文（`sk-`+32hex）**仅此一次**；remain 缺省=quota；group 缺省取用户分组再退 default；写后鉴权缓存失效 |
| `GET/POST/DELETE /api/redemption` | 批量生成 `{count:1..100,name,quota>0,expired_time}` | `data.keys` 明文数组（`redd-`+24hex）仅此一次；key 冲突自动重试，重试穷尽整批拒绝 |
| `GET /api/log` | 过滤：`user_id/token_id/channel_id/model_name/start_timestamp/end_timestamp`（Unix 秒闭区间） | id 降序；channel_id 已生效（M2-wave3 日志回填实际服务渠道） |
| `GET /api/log/mine` | `?page&page_size&model_name&start_timestamp&end_timestamp`（登录态） | 当前用户本人请求日志分页（id 降序）；所有权作用域强制取会话用户（user_id 查询参数被忽略）；响应为白名单字段（logMineView）——不含 user_id（会话即归属）与 channel_id（管理面路由语义），也不含令牌名/key 等密钥材料；M3-wave4 |
| `GET/PUT /api/option` | PUT `{"options":{k:v}}` | 键白名单：`relay.*`/`billing_setting.*`/`hooks.*`/`smtp.*`/`register.*`/`oauth.*`/`turnstile.*`/`epay.*`/`stripe.*`/`aff.*`/`topup.*` 前缀 + `ModelRatio`/`CompletionRatio`/`GroupRatio`/`ModelRequestRateLimitEnabled/DurationMinutes/Count/SuccessCount/Group` 精确键（拒 `schema_version`）；值长 ≤2048；任一非法整体拒绝；写后返回新 `version` 且配置热生效；GET 响应中键名含 password/secret（不区分大小写）的值恒脱敏为 `******`（库内明文不变）；PUT 收到值 `******` 视为哨兵「保持旧值」跳过不覆盖（脱敏回显幂等；哨兵键仍须过白名单与长度校验） |
| `POST /api/user/topup` | `{key}`（登录态） | `{quota_added,user_quota}`；事务原子核销（条件 UPDATE 未用→已用防并发重复）→ 面额入账 → topup 日志；过期码惰性标记 status=4 并 400 `redemption_expired` |
| `GET /api/user/self` | -（登录态） | 当前用户对象（同用户视图字段，password_hash 序列化豁免） |
| `GET /api/user/stats` | -（登录态） | 当前用户今日统计 `{start_timestamp,end_timestamp,requests,prompt_tokens,completion_tokens,tokens,quota,models[]}`；服务端 SQL 聚合 logs，作用域恒为会话用户（user_id 查询参数被忽略）；models 按 quota 降序上限 100 行（`model_name/requests/prompt_tokens/completion_tokens/quota`）；时间口径=服务器本地今日 [0 点，当前]；无日志返回全零空态（200 非错误） |
| `POST /api/token/:id/assign` | `{quota>0}`（登录态；归属者或管理员） | `{quota_assigned,remain_quota}`；用户余额 → 令牌 remain+quota 同步增加的转移事务；余额不足 400 `insufficient_quota`；unlimited 令牌 400 拒绝 |
| `GET /api/token/mine` | -（登录态） | 当前用户名下令牌分页列表（形状同管理列表 items/total/page/page_size，id 降序）；所有权作用域强制取会话用户（user_id 查询参数被忽略）；响应为白名单字段——不含 key/key_hash（密钥材料）与 tpm_rpm/tags/allow_ips（管理配置字段） |
| `POST /api/user/topup/order` | `{gateway:"epay"\|"stripe",amount_cents}`（登录态） | 网关开关校验 → min/max 金额校验 → 配置完整性 → 汇率快照换算 quota（口径见 5.10/docs/04）→ pending 订单 → `{order_no,pay_url,quota}`；stripe 先建 Checkout Session 后落库（防孤儿单），创建失败 502 `gateway_create_failed` |
| `GET /api/user/topup/orders` | `?page&page_size`（登录态） | 本人订单分页（id 降序，模型字段直出）；状态 1 待支付 2 已支付 3 失败 4 已过期 |
| `GET /api/pay/epay/notify` | EPay GET 回调（公开） | 纯文本应答：验签 → 金额逐位校验 → 事务【条件 UPDATE pending→paid（RowsAffected=0 幂等跳过）→ 买家入账 → topup 日志 → aff 返利】→ `success`；任一步失败 `fail` 不动账（见 5.10） |
| `GET /api/pay/epay/return` | EPay 同步回跳（公开） | 302 `/console/topup?order=<order_no>`（仅导航用，不参与安全边界） |
| `POST /api/pay/stripe/webhook` | Stripe webhook（公开，raw body ≤1MB） | Stripe-Signature 验签（t/v1 + HMAC-SHA256 恒时比较 + ±5min）→ `checkout.session.completed` → metadata.order_no → 同上幂等结算 → 200；其他事件 200 忽略；订单不符 400 `order_mismatch` |
| `GET /api/user/aff` | -（登录态） | `{aff_code,invited_count,aff_history_quota,rebate_percent}`；aff_code 空时惰性生成落库；返利入账只发生在结算事务内，本端点只读 |

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
| 403 | `register_disabled` | 注册开关关闭（`register.enabled` 非 true，M3-wave1） |
| 400 | `turnstile_failed` | Turnstile 人机校验未通过（含 siteverify 调用失败，M3-wave1） |
| 400 | `code_invalid_or_expired` / `code_mismatch` | 邮箱验证码不存在/已过期 / 验证码不匹配（不匹配不消费正确码，M3-wave1） |
| 409 | `email_conflict` | 注册邮箱已被占用（M3-wave1；username 冲突见 `username_conflict`） |
| 404 | `not_found` | 重置密码时邮箱未绑定任何账号（码已消费；不区分账号存在性防枚举，M3-wave1） |
| 503 | `smtp_not_configured` | `smtp.enabled` 未开启时请求发送验证码（M3-wave1） |
| 500 | `mail_send_failed` | SMTP 发信失败（错误信息不含服务端内部细节，M3-wave1） |
| 429 | `rate_limited`（转发面） | 限流触发，响应带 `Retry-After`（见 4.1/限流语义；公开组 IP 限频见 5.7） |
| 500 | `*_failed` | 存储层写失败（create/update/delete/query_failed） |
| 404 | `oauth_not_configured` | OAuth provider 未配置 client_id/secret（oidc 另需 issuer），M3-wave2 |
| 401 | `totp_required` | stage1 半登录会话访问 RequireAuth 端点（需先完成两步验证，M3-wave2） |
| 400 | `totp_code_invalid` | TOTP 动态码错误（enable/disable/login/2fa 共用；不消费密钥可重试，M3-wave2） |
| 400 | `totp_not_setup` / `totp_already_enabled` | 未 setup 先 enable / 已启用再 setup（M3-wave2） |
| 400 | `totp_not_enabled` | 未启用两步验证时调 disable（M3-wave2） |
| 400 | `identity_last` | 无口令用户解绑最后一个第三方身份（防锁死，M3-wave2） |
| 400 | `old_password_mismatch` | 修改口令时旧口令不正确（M3-wave2） |
| 403 | `gateway_disabled` | 请求的支付网关未启用（`epay.enabled`/`stripe.enabled` 非 true，M3-wave3） |
| 400 | `gateway_not_configured` | 网关已启用但密钥/网关地址未配置（M3-wave3） |
| 400 | `amount_out_of_range` | 充值金额超出 `topup.min_amount_cents`/`max_amount_cents` 区间（M3-wave3） |
| 502 | `gateway_create_failed` | Stripe Checkout Session 创建失败（不落库防孤儿单，M3-wave3） |
| 500 | `topup_order_failed` | 充值订单落库失败（M3-wave3） |
| 400 | `invalid_signature` | Stripe webhook 验签失败（恒时比较失败或时间戳超 ±5min；epay notify 同场景应答纯文本 fail，M3-wave3） |
| 400 | `order_mismatch` | 回调与订单不符：网关不匹配或金额不一致，拒绝结算（M3-wave3） |
| 500 | `settle_failed` | 订单结算事务失败（买家缺失或存储错误；Stripe 侧会重试，M3-wave3） |

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
  allowedOptionKey 对齐（`relay.*`/`billing_setting.*`/`hooks.*` + M3-wave1 商业化前缀
  `smtp.*`/`register.*`/`oauth.*`/`turnstile.*`/`epay.*`/`stripe.*`/`aff.*`/`topup.*`
  + ModelRatio/CompletionRatio/GroupRatio/ModelRequestRateLimit* 五精确键）；新增可写键
  必须前后端同步，否则保存即 400 `option_forbidden`；换皮类键（SystemName 等）后端不支持，
  前端不提供编辑。**敏感键脱敏与哨兵（M3-wave1）**：键名含 password/secret（不区分
  大小写）的键 GET 回显恒为 `******`，前端 Settings 对这类键标注「已脱敏」并提示回写
  `******` = 保持旧值（PUT 哨兵跳过语义）；Settings KEY_GROUPS 已同步八个新分组
  （注册/邮件 SMTP/Turnstile/OAuth/支付 EPay/Stripe/邀请 aff/充值 topup）。
- **充值页（M2-wave3，M2 缺陷修复更新）**：/console/topup 调 `GET /api/user/self` +
  `GET /api/token/mine`（所有权作用域端点，登录态即可；原对接的管理列表
  `GET /api/token?user_id=` 为 root 专属，普通用户 403）+ `POST /api/user/topup` +
  `POST /api/token/:id/assign`；兑换码状态枚举补 4=已过期（REDEMPTION_STATUS，与后端惰性
  标记语义对齐）。
- **会话探针与角色菜单（M2 缺陷修复）**：ConsoleLayout 会话探针用 `GET /api/user/self`
  （最低权限端点；用管理端点探针会把普通用户 403「需要管理员权限」误判为会话失效，
  详见 docs/11 踩坑）；菜单按角色渲染——渠道/用户/兑换码/系统设置为 root 专属
  （ADMIN_ONLY_KEYS），普通用户见数据看板/令牌/充值/模型广场/日志，直访专属路径回看板。
- **数据看板按角色取数（M2 收官 Task #19）**：/console 数据看板 root 用管理面 `GET /api/log`
  今日区间分页聚合（上限 10 页 × 100 条，超出提示截断口径）；普通用户调登录态
  `GET /api/user/stats`（服务端 SQL 聚合，一次请求返回汇总与模型分布，无截断）；两类
  数据源加载失败均优雅降级空态（0 值卡片 + 空表 + console.warn 留痕），不显示错误横幅——
  普通用户页面禁止直调管理面端点（/api/log 为 root 专属，直调 403）。
- **在线充值与邀请返利页（M3-wave3）**：/console/topup 的「在线充值」区块按
  `GET /api/setup` 的 `topup` 网关开关渲染（epay/stripe Radio），下单
  `POST /api/user/topup/order` 成功后 `window.location.assign(pay_url)` 整页跳转；
  支付回跳 `/console/topup?order=<order_no>` 时从订单列表首页回查该单展示支付状态
  （order_no 全局唯一，回跳单必为最新订单；未命中提示刷新，不轮询）；订单列表
  `GET /api/user/topup/orders`（TOPUP_ORDER_STATUS 状态映射）。/console/invite 调
  `GET /api/user/aff`：邀请码与邀请链接（`{origin}/register?aff={code}`，Register 页
  ?aff= 预填复用）复制、累计返利、邀请人数、返利比例。
- **日志页按角色取数与 bundle 分包（M3-wave4）**：/console/logs root 走管理面
  `GET /api/log`（用户/渠道筛选与列保留，行为不变）；普通用户走登录态 `GET /api/log/mine`
  （服务端会话作用域 + logMineView 白名单，前端隐藏用户/渠道筛选与列、不请求管理面下拉
  数据源）。App 全部页面组件 React.lazy 按路由分包（Suspense + Spin fallback），vite
  manualChunks 拆 vendor（react/react-dom/react-router-dom/dayjs）与 antd（antd +
  @ant-design/icons）——主 chunk 由 1,321.39 kB（gzip 418.81）降至 15.95 kB（gzip 7.01），
  页面 chunk 按需加载，AntD 独立 chunk 缓存友好（业务迭代不再触发库重下载）。

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

### 5.7 公开注册体系契约（M3-wave1）

公开组四端点（无会话要求），安全边界 = 开关门控 + IP 限频 + 人机校验/邮箱验证码。
配置键见 5.5 键表（`register.*`/`smtp.*`/`turnstile.*` 前缀）；OAuth 三项自 M3-wave2 起
按配置真实探测（client_id/secret 配齐才可用，oidc 另需 issuer，契约见 5.8）；Turnstile/支付
网关为行业通用服务，可达性由配置方自担。

- **GET /api/setup**（公开）：返回 FeatureFlags——
  `{register_enabled, email_verification, turnstile_site_key, oauth:{github,linuxdo,oidc}}`；
  `turnstile_site_key` 仅在 `turnstile.enabled=true` 时非空（site_key 为公开公钥，
  明文下发前端渲染 widget 用；secret_key 永不下发且经 options GET 脱敏）。
  `/api/status` 的 `data.features` 块复用同一 FeatureFlags 结构（M3-wave1 起）。
- **POST /api/user/register**（公开）：`{username, password, email, code?, aff_code?,
  turnstile_token?}` → 201 `{id, username, aff_code}`（aff_code 为新用户 8 位随机邀请码，
  去易混淆字符集）。流程与拒绝点：register.enabled 关 → 403 `register_disabled`；
  IP 限频（1h×5，超限 429+Retry-After）；参数校验（三必填、email 含 @、password ≥6 位）
  → 400 `invalid_request`；Turnstile 开 → 服务端 siteverify（5s 超时）失败 → 400
  `turnstile_failed`；邮箱验证开 → 码校验（见下）失败 → 400；username/email 查重 → 409
  `username_conflict`/`email_conflict`；bcrypt 建户（role=普通用户、group=default、
  auth_version=1、quota=`register.quota_for_new_user`）。
  **邀请奖励**：aff_code 查到邀请人 → 同一事务内双向入账（邀请人 quota 与
  aff_history_quota 累加 `aff.register_reward_inviter`；新用户 quota 累加
  `aff.register_reward_invitee`）+ 两条 protocol="aff" 日志（model_name=
  `register_reward_inviter`/`register_reward_invitee`，detail 含 event/ref_id/quota）；
  奖励值 ≤0 不入账；aff_code 无效/自引用不阻断注册（仅无奖励）。
- **POST /api/verification_code**（公开）：`{email, purpose∈{register,reset}}` →
  `{sent:true}`；purpose 白名单外 400；`smtp.enabled` 关 → 503 `smtp_not_configured`；
  发信失败 → 500 `mail_send_failed`。验证码 6 位数字，TTL 10 分钟、同邮箱重发间隔
  60 秒、每日上限 20 封（均 429 `rate_limited`）；一次性消费——成功校验即失效，
  不匹配不消费正确码（可重试至过期）。码不回显、不落日志。
- **POST /api/user/reset_password**（公开）：`{email, code, new_password}` →
  `{reset:true}`；IP 限频（1h×5）；验码（一次性消费，purpose=reset）→ 查邮箱
  （未绑定 404 `not_found`，此时码已消费——枚举探测需先获取有效码，泄露面可控）→
  bcrypt 新口令 → auth_version++（既有会话全部失效）；new_password ≥6 位。
- **IP 限频参数表**（复用 internal/ratelimit 滑动窗口，与管理面/转发面限流器隔离；
  被拒请求无副作用、响应带 Retry-After 秒数）：

  | 限频键 | 窗口 | 上限 |
  | --- | --- | --- |
  | `register\|<IP>` | 1h | 5 |
  | `login\|<IP>` | 1h | 10 |
  | `reset\|<IP>` | 1h | 5 |

  Login 的 IP 限频先于凭据校验（含账号不存在场景）；`POST /api/user/login/2fa` 与密码登录
  共用 `login\|<IP>` 限频键（stage1 会话重放爆破动态码受同一窗口约束）；发码限频由
  verification.Store 按（邮箱+purpose）维度承担（60s/20 每日），与上表 IP 维度正交。

### 5.8 OAuth 通用 provider 契约（M3-wave2）

公开组三端点 + 登录态绑定/解绑/列表，涉及文件 `internal/api/oauth.go`；provider 名单
硬编码 `github`/`linuxdo`/`oidc`（名单外 404 `oauth_not_configured`）。

- **配置键**（options 白名单 `oauth.*`，写后热生效；secret 键 GET 回显脱敏）：
  `oauth.github.client_id/client_secret`、`oauth.linuxdo.client_id/client_secret`、
  `oauth.oidc.client_id/client_secret`、`oauth.oidc.issuer`。三项各自配齐才可用
  （`/api/setup` 的 oauth 块与 FeatureFlags 真实探测）。
- **GET /api/oauth/:provider**（公开，登录发起）：provider 已配置校验（否则 404
  `oauth_not_configured`）→ 生成 32hex state（crypto/rand）→ 写一次性 HttpOnly cookie
  `oauth_state`（值 `state|mode|uid`，mode=login/bind；TTL 5min，SameSite=Lax，防 CSRF）→
  302 authorize。GitHub scope=`read:user`，authorize 端点固定；linuxdo/oidc 走
  `{issuer}/.well-known/openid-configuration` 发现（包级缓存 1h，authorize/token/userinfo
  三端点）。
- **GET /api/oauth/:provider/callback**（公开）：state 恒时比较 + cookie 一次性清除
  （缺失/篡改/过期一律 302 `/login?oauth_failed=1`，不向 URL 泄露细节，无副作用）→
  form POST（code+redirect_uri+client_id/secret+grant_type）换 access_token → 拉 userinfo
  （GitHub `api.github.com/user` 取 `id`；oidc 取 userinfo 的 `sub`——**信任 TLS 通道
  不验 ID token 签名**，权衡注记 docs/11）→ 查 `user_identities(provider,provider_uid)`：
  命中且用户启用 → 签发完整会话 302 `/console`；命中但禁用 → 失败路径；未命中且
  `register.enabled` → **自动建户**（username=`<provider>_<uid>`、空 password_hash 密码
  登录不可用、quota=`register.quota_for_new_user`、email 冲突置空，事务内建户+身份绑定
  原子，撞名整体回滚）302 `/console`；否则失败路径。
- **bind 绑定模式**（登录态）：`GET /api/oauth/:provider/bind` 走同一 authorize 流程，
  state cookie 标记 `bind|<uid>`；callback 校验当前会话（完整会话 + uid 与 state 一致
  双保险）后落库绑定，成功 302 `/console/profile`。**bind 模式永不签发/顶替会话**：身份已
  绑本人 → 幂等成功；已绑他人（复合唯一冲突）→ 失败路径。
- **解绑与列表**（登录态）：`GET /api/user/identities` 本人列表；`DELETE
  /api/user/identities/:id` 归属校验（非本人 404）+ 防锁死——无 password_hash 且是最后
  一个身份 → 400 `identity_last`（先在个人中心设置口令即可解绑，首次设密免验旧口令）。
- **redirect_uri 推导**：scheme 信任 `X-Forwarded-Proto` 首段（反代 TLS 终结场景）→
  `r.TLS` → 缺省 http；Host 取请求 Host。信任边界与权衡注记 docs/11。
- **信任边界**：token/userinfo/发现文档为出站请求，目标地址由管理员配置（root 门控）
  ——与 SMTP/Turnstile/hooks 同一信任边界，非终端用户输入，不构成 SSRF 面。

### 5.9 两步验证（TOTP）与个人中心契约（M3-wave2）

涉及文件 `internal/api/totp.go`（三端点）与 `profile.go`（改密/改邮箱）、`session.go`
（Stage claim）。

- **会话 Stage 语义**：sessionClaims 增加 `stage`（0=完整会话，1=待两步验证）；旧 cookie
  无该字段反序列化为 0，向后兼容。`Issue` 支持每调用 TTL；stage1 会话 TTL 固定 5min。
  RequireAuth 校验 `Stage==0`，否则 401 `totp_required`——**stage1 仅可用于
  /api/user/login/2fa**（防半登录态提权）。
- **二段式登录**：Login 对 `totp_enabled && totp_secret != ""` 用户签 stage1 会话返回
  `{require_2fa:true}`；`POST /api/user/login/2fa {code}`（公开）凭 stage1 会话验码 →
  重签 stage0 完整会话并返回用户信息。验码失败 400 `totp_code_invalid`；无/无效 stage1
  会话 401。disable 后登录免验（直接完整会话）。
- **TOTP 三端点**（登录态）：
  - `POST /api/user/totp/setup`：`totp.Generate`（issuer="Hui Api"，account=用户名）→
    secret 落库 enabled=0 待确认 → `{secret, otpauth_uri}`；已启用 400
    `totp_already_enabled`。重复 setup 覆盖未确认的旧 secret。
  - `POST /api/user/totp/enable {code}`：验码（±1 窗口，库 pquerna/otp）→ enabled=1；
    未 setup 400 `totp_not_setup`；错码 400（不启用，可重试）。
  - `POST /api/user/totp/disable {code}`：验码 → secret/enabled 双列清空；未启用 400
    `totp_not_enabled`；错码 400（密钥保留可重试）。**关闭 2FA 必须验码**（非仅登录态）。
  - secret 为敏感材料：options 脱敏规则之外，模型层 `json:"-"` 序列化豁免，任何接口
    不回显已确认的 secret（setup 响应仅在待确认期返回一次）。
- **个人中心自服务**（登录态）：
  - `POST /api/user/password {old_password?, new_password}`：有口令账号必验旧口令（400
    `old_password_mismatch`）；OAuth 建户无口令账号首次设置免验（会话即授权依据，先设密
    才能解绑最后身份防锁死）；≥6 位；成功后 **auth_version++（其他设备会话全失效）并
    重签当前会话**（无缝续用）。
  - `POST /api/user/email {email}`：格式校验 + 查重（409 `email_conflict`）。暂不强制
    邮箱验证码（后续商业化增强项）；改后即可走忘记密码流程找回无口令账号。

### 5.10 在线充值与邀请返利契约（M3-wave3）

涉及文件 `internal/api/order.go`（下单/回调/列表/结算事务）、`aff.go`（邀请信息）、
`internal/payment`（epay.go MD5 签名层 / stripe.go Checkout+Webhook 层，不引
stripe-go SDK）。

- **配置键**（options，写后热生效）：`epay.enabled`/`epay.gateway`/`epay.pid`/
  `epay.secret_key`/`epay.pay_type`；`stripe.enabled`/`stripe.secret_key`/
  `stripe.webhook_secret`；`topup.usd_cny_rate`（缺省 720）、`topup.min_amount_cents`
  （缺省 100，配置 ≤0 兑底 1）、`topup.max_amount_cents`（0=不限）；`aff.rebate_percent`
  （0=返利关闭）。
- **下单**（`POST /api/user/topup/order`，登录态）：`{gateway, amount_cents}` →
  网关开关校验（403 `gateway_disabled`）→ 金额区间校验（400 `amount_out_of_range`）→
  配置完整性（400 `gateway_not_configured`）→ 汇率换算额度 → 落 pending 订单
  （rate 快照与金额同记）→ `{order_no, pay_url, quota}`。
- **额度换算口径**：`quota = (amount_cents × 500000 + rate/2) / rate`（整数四舍五入）。
  rate 为「每 1 USD 的 CNY 分数 ×100」定点快照——epay 以 CNY 计价，rate=
  `topup.usd_cny_rate`（如 720 表示 ¥7.20/$1）；stripe 以 USD 本位计价，rate=100。
  示例：epay 下单 1000 分（¥10）× rate 720 → quota 694444；stripe 下单 1000 分（$10）
  × rate 100 → quota 5000000。
- **结算事务**（`settleTopupOrder`，两回调共用）：查单 → 网关匹配 → 币种
  EqualFold → **金额逐位校验**（回调 money_minor 与订单 amount_cents 一致）→
  **条件 UPDATE `status 1→2 WHERE id=? AND status=1`**（RowsAffected=0 即重复
  通知，幂等跳过不重复入账）→ 买家 quota 入账 → `protocol="topup"` 日志
  （ModelName=order_paid）→ aff 返利（inviter_id>0 且 rebate_percent>0：
  inviter.quota += (quota×pct+50)/100，aff_history_quota 同步累加，写
  `protocol="aff"` 日志 ModelName=topup_rebate）——全事务原子，任何一步失败整体回滚。
- **EPay 回调**（公开，纯文本应答）：`GET /api/pay/epay/notify` 验签（MD5：参数名
  字典序 `k=v&` 拼接、空值与 sign/sign_type 不参与、末尾接 key、hex 小写、
  恒时比较）→ `trade_status` 为 TRADE_SUCCESS/TRADE_FINISHED 才结算，其他状态
  应答 `success` 不动账（确认已知意防重发）→ 结算成功 `success`，验签/查单/金额
  不符 `fail`（网关侧会重试）；`GET /api/pay/epay/return` 同步回跳 302
  `/console/topup?order=<order_no>`（仅导航用，真实入账以 notify 为准）。
- **Stripe 回调**（公开）：`POST /api/pay/stripe/webhook` 读 raw body（≤1MB）→
  `Stripe-Signature` 头 t/v1 解析 → HMAC-SHA256(`"{t}.{raw_body}"`) 恒时比较
  （多 v1 任一匹配）→ 时间戳 ±5min 防重放 → `checkout.session.completed` 事件取
  `metadata.order_no`/`amount_total`/`currency` → 复用结算事务 → 200；其他事件
  类型 200 忽略；验签失败 400 `invalid_signature`、订单不符 400 `order_mismatch`、
  结算失败 500 `settle_failed`（Stripe 侧重试）。Checkout Session 创建（下单时同步
  调 Stripe API）：form 编码 + Basic `secret_key`，
  `line_items[0][price_data][unit_amount]` 自定义金额，`metadata[order_no]` 回传；
  success_url/cancel_url 均指向 `/console/topup?order=<order_no>`；创建失败
  502 `gateway_create_failed` 且**不落库**（防孤儿单）；APIBase 仅部署方可注入
  （SSRF 信任边界，权衡注记 docs/11）。
- **订单列表**（`GET /api/user/topup/orders`，登录态）：本人作用域分页（id 降序），
  模型字段直出（order_no/gateway/amount_cents/currency/quota/rate/status/trade_no/
  detail/paid_time/created_time）；状态机 1=待支付 2=已支付 3=失败 4=已过期。
- **邀请信息**（`GET /api/user/aff`，登录态）：`{aff_code, invited_count,
  aff_history_quota, rebate_percent}`；aff_code 空时惰性生成落库（31^8 碰撞域）；
  返利入账只发生在结算事务内，本端点只读（口径见 docs/04 第九节）。
- **测试注入点**：`h.stripeHTTP`/`h.stripeAPIBase` 同包小写字段可指向 httptest 假
  Stripe 端点断言请求形态；epay 验签纯函数直接构造 query；并发幂等证据
  TestEpayNotifyIdempotentConcurrent（12 goroutine 重复通知同一订单，恰一次入账含返利）。
- **订单超时关单（M3-wave4）**：internal/worker 每 5min 条件 UPDATE `WHERE status=1 AND
  created_time < cutoff` 置 4=已过期（cutoff = now − `topup.order_timeout_minutes`，缺省
  15min，热生效）；与回调结算的单向状态机互斥关系见 docs/04 第九节第 4 小节。
