# 05-API契约

> 状态：占位。M1-wave2（relay）与 M3（管理面）落地时逐端点细化，本文先行固化端点清单与总体契约约定。

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

- [ ] M1-wave2：`/v1/chat/completions`、`/v1/models` 契约细化 + 示例
- [ ] M1-wave2：`/v1/messages`、`/v1/messages/count_tokens` 契约细化 + 示例
- [ ] M3：管理面各端点组契约细化 + 错误码表
