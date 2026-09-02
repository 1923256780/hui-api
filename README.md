# hui-api

> 为个人开发者与小团队打造的轻量 LLM API 网关——单二进制 + SQLite 零依赖、可靠多渠道路由与熔断、令牌预算与兑换码体系。

## 项目简介

hui-api 是一个以「自托管、零依赖、开箱即用」为核心理念的 LLM API 网关：

- **单二进制交付**：前端资源经 `go:embed` 嵌入，配合 SQLite 存储与纯 Go 驱动（免 CGO），一个可执行文件 + 一个数据文件即可跑起来，无任何外部服务依赖。
- **可靠多渠道路由**：多上游渠道按优先级与权重调度，失败自动熔断、半开恢复，请求链路可观测、可对账。
- **令牌预算与兑换码体系**：令牌支持哈希存储、预算周期、速率限制与标签分组；兑换码支持批量生成与核销，适合个人商业化场景。
- **宽松许可**：Apache-2.0，可自由自用、修改与二次分发。

## 技术栈

| 层级 | 选型 | 说明 |
| --- | --- | --- |
| 后端语言 | Go 1.23+ | 单二进制、交叉编译友好 |
| Web 框架 | Gin | 路由与中间件 |
| ORM | GORM | 数据访问与迁移 |
| 数据库 | SQLite（glebarez 纯 Go 驱动） | 零依赖、免 CGO |
| 前端 | React 18 + Vite | 函数组件 + hooks |
| 嵌入方式 | go:embed | 前端产物打进二进制 |
| CI | GitHub Actions | vet / test / 竞品词扫描 |

## 快速开始

> 占位：M1 里程碑落地存储层与转发链路后，此处将补充二进制下载、首次启动、初始化管理员账号与接入第一个渠道的完整步骤。

```bash
# M0 阶段仅提供最小入口与健康检查
go build -o bin/hui-api ./cmd/hui-api
./bin/hui-api -addr :3100
curl http://127.0.0.1:3100/health
```

## 特性清单

- [x] 最小可运行入口与 `/health` 健康检查（M0）
- [ ] 双协议转发：OpenAI 兼容 `/v1/chat/completions` 与 Anthropic Messages `/v1/messages`（M1）
- [ ] 多渠道路由：优先级/权重调度、失败熔断、半开恢复（M1）
- [ ] 三模式计费：classic_ratio / tiered_expr 表达式 / 按次，预扣费 + 多退少补（M2）
- [ ] 令牌体系：key_hash 哈希存储、预算周期、TPM/RPM 限流、标签分组（M3）
- [ ] 兑换码与用户体系（M3）
- [ ] React 管理台与配置热更新（M3-M4）
- [ ] 生产切换预案：旁路验证 → 端口接管 → 两条命令回滚（M4）

进度与交接见 [docs/10-里程碑与进度.md](docs/10-里程碑与进度.md)。

## 文档索引

所有文档入口见 [docs/INDEX.md](docs/INDEX.md)，冷启动阅读顺序：

1. [AGENTS.md](AGENTS.md) — 协作规范（AI 会话与人类贡献者必读）
2. [docs/INDEX.md](docs/INDEX.md) — 文档地图
3. [docs/10-里程碑与进度.md](docs/10-里程碑与进度.md) — 当前状态
4. 按任务阅读主题文档（架构 / 数据模型 / 计费 / API / 部署）

## License

[Apache-2.0](LICENSE)
