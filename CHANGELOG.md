# 更新日志

本项目所有显著变更将记录在本文件中。

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

- M1-wave3 计费内核 `internal/billing`：三模式计费引擎——classic_ratio 倍率线性（`round((input + completion×completion_ratio) × model_ratio × group_ratio × 500000 / 1e6)`）、tiered_expr 表达式（github.com/expr-lang/expr，`tier("base", expr)` 单层语义，编译结果并发安全缓存，变量 p=纯输入/c=输出/cr=缓存读（原始 tokens 数），表达式值 micro-USD，不叠乘组倍率）、per_call 按次（固定价 × group_ratio × 500000）；quota = round(计费值 × 500000 / 1e6)；未配价模型显式拒绝（HTTP 503 `model_not_priced`，配置声明不完整视为未配价）。
- M1-wave3 价格单源：内置 `internal/billing/prices.json`（go:embed 打包，含版本号）；启动校验 schema（模式枚举、数值非负、表达式编译 + 零用量试求值）；价格查找顺序 options 显式声明 > `ModelRatio` 隐式 classic > 内置价单 > 拒绝服务；LookupPrice 返回不可变 ModelPrice 快照，单请求生命周期复用（防预扣与结算间配置热更导致口径漂移）。
- M1-wave3 预扣费账本 `internal/billing/ledger.go`：Estimate 预扣估算（输入按请求体字节/4 粗估 + max_tokens 缺省 1024，计费后上浮 20%，最低 1 quota）；Freeze 事务内条件 UPDATE `WHERE remain_quota >= delta` 防透支（RowsAffected=0 → `ErrInsufficientQuota`，映射 403）；Settle 多退少补（差额 = 冻结 − 实结，补扣允许透支到负数）；RefundFull 全额退款；users.quota 与 tokens.remain_quota 同步扣减；unlimited 令牌三级跳过（冻结/结算/退款）。
- M1-wave3 结算挂接 `internal/gateway`：Serve 在 relay Respond 成功后按 Usage 结算（relay.Usage.CacheReadTokens 从 PromptTokens 拆分为表达式变量，防缓存重复计费）；失败/流中断全额退款并日志标记 `aborted` + `refund_full`；usage 缺失但有正常内容 → 本地粗估并标记 `estimated`；logs.detail 记 billing_mode/token 明细（frozen/actual/prompt/completion/cache_read）/cache 命中/estimated/unlimited。
- M1-wave3 异步请求日志 `internal/billing/logwriter.go`：强类型 LogRecord/Detail（Request/Token 字段不混用字符串拼 JSON）；有界 channel 非阻塞 Submit + 丢弃原子计数；攒批 flusher（批满或定时触发）；Close 幂等并 drain 在途批次；批量写入失败降级逐条落库。
- M1-wave3 黄金测试集：`internal/billing/testdata/golden/billing_cases.json`（四条实测账单的数值转录，中性字段名 input_tokens/output_tokens/cache_read_tokens/expected_quota/model）；TestGoldenBilling 按 tiered_expr 公式对期望 quota 逐位断言（四舍五入口径一致）；含黄金文件竞品词自检。
- M1-wave3 测试：计费三模式与 rounding 边界、tier() 表达式、GroupRatio 回退语义（未配置组 → default 组值 → 1.0）、编译缓存并发、价单校验（含试求值拒绝档位错）、Estimate、账本并发冻结结算（-race 防透支）、异步日志批量与 drain 与并发、gateway 计费挂接 11 用例（结算/补扣透支/流中断退款/estimated/未配价 503/余额不足 403/unlimited 跳过账本/三模式过线/停机日志排空/失败兜底退款）；`go test ./... -race` 十包全绿。
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
- 竞品词扫描假阴性修复（M1-wave3 追加）：本地扫描清单并入未跟踪文件（`git ls-files --others --exclude-standard`），消除「提交前本地通过、CI 失败」盲区；黄金自检测试改为从词表文件读取禁用词（测试代码内不再硬编码竞品词，词表成为单一事实源）。

### 文档

- M1-wave3：docs/01 设计点 4 落地标注与目录结构同步；docs/04 落地标注 + 新增「公式实测」节（四条黄金样例数值对齐）；docs/05 错误码表补 403/503 计费语义与计费运行轨键；docs/10 ROADMAP 重排（M1 完成、M2 重定义管理面 API）与状态块重写；docs/11 踩坑追加；新增 ADR 0004（计费引擎落地：expr 选型与 tier 单层语义）。
- M1-wave2：docs/01 设计点 2/3 落地标注与目录结构同步（internal/channel 落地为 internal/gateway）；docs/05 转发面契约细化（请求/响应示例与错误码表）；docs/11 踩坑追加两条；docs/10 状态块与交接记录更新。
- 新增 ADR 0003（存储层落地：单写多读与原子快照热更）；docs/01、docs/03 同步落地状态与实现细节；docs/02 补充 build.ps1 目标说明；docs/11 踩坑记录追加三条。

### 既有（M0）

- 初始化仓库工程体系：目录骨架、AGENTS.md 协作规范、docs/ 全套设计文档、两篇 ADR。
- 最小可运行入口 `cmd/hui-api`：`-version` 版本信息与 `/health` 健康检查占位实现，附单测。
- CI 流水线：竞品词 grep 扫描、go vet、go test、前端构建占位（web/ 存在后自动启用）。
- 协作模板：PR 模板、bug/feature Issue 表单、release 分类配置、竞品词表与本地扫描脚本。
