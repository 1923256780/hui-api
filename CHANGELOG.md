# 更新日志

本项目所有显著变更将记录在本文件中。

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

- M1-wave2 双协议转发：`internal/relay` 转发内核（Protocol 适配接口、SSE 逐事件 flush 不缓冲不改写、JoinBaseURL 拼接含 /v1 去重、上游 scheme 白名单、非流式 64MB 读取上限）；`internal/relay/openai`（`POST /v1/chat/completions`：流式注入 `stream_options.include_usage`，usage 提取含 `cached_tokens`）；`internal/relay/anthropic`（`POST /v1/messages` 与 `POST /v1/messages/count_tokens`：x-api-key 与 Bearer 双鉴权、anthropic-version 透传/默认 2023-06-01、message_start 输入侧（含 cache_read_input_tokens）与 message_delta 累计输出侧 usage 归一）。
- M1-wave2 路由编排 `internal/gateway`：tokens.key_hash 鉴权（SHA-256，内存缓存 TTL 30s + 负缓存 5s + 写时失效 + singleflight 防击穿）；Deployment 选择（启用渠道 → 协议/模型/排除集过滤 → priority 降序 → weight 加权随机，全零均匀）；熔断状态机（allowed_fails=3 / cooldown=5s / 429 立即冷却 / 分钟失败率>50% 且样本≥10 冷却，只隔离单渠道，进程内存态）；typed retry 策略表（Auth 零重试包装 502；RateLimit 指数退避 500ms 倍增封顶 4s；其余立即换点；重试仅限首字节前，排除集跨跳累积上限 5）；pre-call 检查（请求体上限 413、无可用渠道 503 语义错误、缺 model/坏 JSON 400）；`GET /v1/models`（启用渠道模型并集 + 虚拟模型组名，组内不展开）。
- M1-wave2 参数改写 `internal/override`：param_override 管道（点分路径含数组下标，操作顺序固定 delete→set→append→replace→regex_replace，JSON 数字精度保留，正则预编译提前失败）；渠道级配置存 `channels.param_override`（schema v2，迁移 `migrations/0002_channel_param_override.sql`，DDL 对账测试升级为全迁移脚本）。
- M1-wave2 测试：relay/openai/anthropic/gateway 单测（SSE 逐事件 flush 计数、双协议 usage 提取、熔断状态机、typed retry 策略表、选择器 priority/weight/排除集、鉴权生命周期/singleflight、override 管道 delete+set）；双协议端到端冒烟（httptest 假上游 + 本地起完整路由，覆盖 OpenAI/Anthropic 非流式与流式、count_tokens、/v1/models）；`go test ./... -race` 九包全绿。
- M1-wave1 存储层：`internal/model` 六表模型（channels/tokens/users/redemptions/options/logs，quota 单位 500000=$1）；`internal/store` 读写分离（写池单连接 + 独立读池，WAL/NORMAL/busy_timeout=5000）；迁移框架（AutoMigrate 基线 + `options.schema_version`）；`migrations/0001_init.sql` 文档源 DDL，测试固化双源一致。
- 配置双轨：`internal/config` 启动轨（env > config.yaml > 默认值，附 `config.example.yaml` 模板）与运行轨 options 原子快照热更骨架（atomic.Value 版本号替换，管理面写 API 于 M2 交付）。
- `internal/hook` 异步旁路：Hook 接口（OnSuccess/OnFailure + Event）+ 注册表 + 有界队列异步投递（队列满丢弃并计数、单 hook panic 隔离），内置 noop/console 实现。
- web/ 管理台外壳：Vite + React 18 + TypeScript + Ant Design，登录页占位与空白布局；`go:embed web/dist` 预留（embed.FS + SPA fallback）。
- 入口装配：main 加载配置 → 初始化 store → 迁移 → 挂路由（`/health`、`/api/status`）→ 优雅停机（signal 监听、Shutdown 带超时 ctx、关闭连接池）。
- 构建链：`scripts/build.ps1` 增加 `web` 目标与 `Ensure-WebDist` 兜底（dist 缺失自动前端构建、失败回退占位页）；CI go job 补 web/dist 占位步骤。
- 测试：store 建表/迁移/读写分离/PRAGMA/DDL 双源等价、config 热更新（含并发）、hook 队列与 panic 隔离用例；`/health`、`/api/status` 契约测试。

### 变更

- `.gitignore` 补充 `config.yaml`、`*.db-shm`、`*.db-wal`。
- 竞品词扫描脚本修复：显式 `git -c core.quotepath=false` 与控制台 UTF-8 编码，消除中文文件名导致的本地崩溃与 CI 静默跳过盲区。

### 文档

- M1-wave2：docs/01 设计点 2/3 落地标注与目录结构同步（internal/channel 落地为 internal/gateway）；docs/05 转发面契约细化（请求/响应示例与错误码表）；docs/11 踩坑追加两条；docs/10 状态块与交接记录更新。
- 新增 ADR 0003（存储层落地：单写多读与原子快照热更）；docs/01、docs/03 同步落地状态与实现细节；docs/02 补充 build.ps1 目标说明；docs/11 踩坑记录追加三条。

### 既有（M0）

- 初始化仓库工程体系：目录骨架、AGENTS.md 协作规范、docs/ 全套设计文档、两篇 ADR。
- 最小可运行入口 `cmd/hui-api`：`-version` 版本信息与 `/health` 健康检查占位实现，附单测。
- CI 流水线：竞品词 grep 扫描、go vet、go test、前端构建占位（web/ 存在后自动启用）。
- 协作模板：PR 模板、bug/feature Issue 表单、release 分类配置、竞品词表与本地扫描脚本。
