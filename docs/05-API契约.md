# 05-API契约

> 状态：**活文档**。转发面契约已于 M1-wave2 落地并细化（见第四节）；管理面各端点组随 M3 落地时逐项细化。

## 一、端点清单（规划）

### 转发面（客户端调用，令牌鉴权）

| 端点 | 协议 | 说明 |
| --- | --- | --- |
| `POST /v1/chat/completions` | OpenAI 兼容 | 对话补全，支持 `stream: true` |
| `POST /v1/messages` | Anthropic Messages | 消息协议，支持流式 |
| `POST /v1/messages/count_tokens` | Anthropic Messages | 输入 tokens 计数 |
| `GET /v1/models` | OpenAI 兼容 | 模型清单（聚合各渠道可用模型） |

### 管理面（`/api/*`，管理员鉴权）

| 端点组 | 说明 |
| --- | --- |
| `/api/user/*` | 登录、用户与余额管理 |
| `/api/channel/*` | 渠道 CRUD、状态与熔断复位、批量操作 |
| `/api/token/*` | 令牌 CRUD、预算周期、tpm_rpm、tags |
| `/api/redemption/*` | 兑换码生成、查询、作废 |
| `/api/log/*` | 请求日志查询与对账导出 |
| `/api/option/*` | options 运行轨配置读写（热更） |
| `/api/status` | 服务状态、版本、schema 版本 |

## 二、契约约定（总体）

1. **鉴权**：转发面 `Authorization: Bearer sk-xxx`，服务端 SHA-256 后查 `key_hash`；管理面 session/token 双轨（M3 细化）；
2. **错误结构**：转发面错误沿用入口协议的错误形状（OpenAI error object / Anthropic error object），保证客户端 SDK 无感；管理面统一 `{success, message, data}` 包裹；
3. **流式**：SSE 转发，逐块透传，计费在流结束时结算；
4. **幂等**：管理面写操作支持 `Idempotency-Key` 头（M3 落地）；
5. **版本化**：转发面路径固定为 `/v1`，破坏性变更走 `/v2`；
6. **契约测试**：每个端点落地时必须附请求/响应示例与错误码表（写入本文对应小节）。

## 三、细化计划

- [x] M1-wave2：`/v1/chat/completions`、`/v1/models` 契约细化 + 示例（2026-09-02）
- [x] M1-wave2：`/v1/messages`、`/v1/messages/count_tokens` 契约细化 + 示例（2026-09-02）
- [x] M1-wave3：计费错误码（403/503）与计费运行轨键补充（2026-09-02）
- [ ] M3：管理面各端点组契约细化 + 错误码表

## 四、转发面契约（M1-wave2 落地，M1-wave3 补计费）

### 4.1 通用约定

- **鉴权**：OpenAI 面取 `Authorization: Bearer <key>`；Anthropic 面优先 `x-api-key: <key>`，缺省时回退 Bearer。服务端 SHA-256 后查 `tokens.key_hash`（内存缓存 TTL 30s + 负缓存 5s + 写时失效 + singleflight 防击穿）；禁用/过期令牌返回 403，无效返回 401。
- **错误结构**：错误一律按入口协议形状输出（OpenAI error object / Anthropic error object），客户端 SDK 无感；`error.type`/`code` 用语义短横线标识（见 4.5 错误码表）。
- **流式**：`stream: true` 时以 SSE 逐事件转发（事件边界即 flush，不缓冲整段、不改写事件原文）；OpenAI 面流式请求自动注入 `stream_options.include_usage=true` 以取尾 chunk 用量（客户端会多收到一个 usage chunk）。
- **重试与熔断**：上游错误按类重试（Auth 零重试；RateLimit 指数退避；其余立即换点），重试仅限首字节前，排除集上限 5；重试穷尽或不可重试时透传上游错误（Auth 类包装为 502）。熔断只隔离单渠道（进程内存态）。
- **请求体限制**：入口请求体超过上限（默认 32MB，运行轨 `relay.max_body_bytes` 可调）本地快速失败 413。
- **计费**（M1-wave3）：请求前按估算上浮 20% 冻结令牌余额（不足 403 拒绝）；响应完成后按实际 usage 多退少补（补扣允许透支到负数）；上游失败/流中断全额退款；usage 缺失按本地粗估计费并标记 `estimated`（logs.detail 可见）；模型未配价 503 拒绝。
- **运行轨配置键**：`relay.max_body_bytes`（请求体上限）；`relay.virtual_model_groups`（虚拟模型组 JSON `{"组名":["成员",...]}`，`/v1/models` 只返回组名）；计费键 `billing_setting.billing_mode` / `billing_setting.billing_expr` / `billing_setting.billing_price`（模型名 → 模式声明 / 计费表达式 / 按次单价）与 `ModelRatio` / `CompletionRatio` / `GroupRatio`（classic 回退价与组倍率），管理面写后 Reload() 热生效。

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
