# 更新日志

本项目所有显著变更将记录在本文件中。

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

- M2-wave1 管理面 API：登录会话（`POST /api/user/login`/`logout`，golang.org/x/crypto/bcrypt 校验，
  HMAC-SHA256 签名 cookie HttpOnly+SameSite=Lax TTL 7d，`users.auth_version` 纳入中间件比对、
  改密递增即失效旧会话；root 引导幂等，口令经 HUI_API_ROOT_PASSWORD 注入或缺省 123456）；
  管理中间件区分 root（role=100）/普通角色，管理端点全部 root 权限；
- M2-wave1 管理 CRUD 六组端点（整对象幂等写，PUT 全量覆盖不丢字段）：`/api/channel`（分页/单个
  key 脱敏回显/创建/更新/删除 + `POST /api/channel/test/:id` 连通测试返回 status_code/time_ms，
  写后 Breaker.Reset 复位熔断）；`/api/token`（明文 sk-+32hex 仅创建响应返回一次，remain 缺省
  =quota，group 缺省取用户分组再退 default，写后 Auth().Invalidate 失效鉴权缓存）；`/api/user`
  （用户名唯一 409；改密 bcrypt+auth_version 递增；root 防自锁不可自改 role/status；删除用户
  级联删令牌并逐个失效缓存）；`/api/redemption`（批量生成 count 1..100、key redd-+24hex 冲突
  重试、列表/删除；核销状态机属 wave3）；`/api/log`（分页 + user/token/channel/model/时间过滤，
  id 降序）；`/api/option`（GET 全部/PUT 批量写，键白名单拒 schema_version，值长 ≤2048，任一
  非法整体拒绝，写后 Runtime.Reload() 热生效免重启并返回新版本号）；
- M2-wave1 限流挂接 `internal/ratelimit`：手写滑动窗口（AllowRequest/RecordSuccess 与
  AllowTokens/RecordTokenUsage 双入口；被拒不消耗配额；sync.Map 分桶 + idle TTL/LRU 淘汰）；
  全局 ModelRequestRateLimit 语义（Enabled/DurationMinutes/Count/SuccessCount 行业通用键名）+
  分组 JSON 覆盖（`{"组名":[最大请求数,最大成功数]}`，共用周期）；令牌 TPM/RPM 接入转发链路
  （tpm_rpm JSON，超限 429 + Retry-After）；本机限流 429 不计入上游熔断失败计数（限流与熔断解耦）；
- M2-wave1 令牌分组接 GroupRatio：token.group → GroupRatio 倍率传入 billing（M1-wave3 遗留项
  清偿），组缺省 default→1.0；
- M2-wave1 装配：main.newRouter 挂 EnsureRootUser（幂等）→ SessionManager →
  api.Handler.Register；管理面与转发面共享同一 Gateway 实例（写后失效直达运行态）；
- M2-wave1 测试：api 包 19 例（bcrypt 往返/会话签发校验/篡改拒绝/过期/引导幂等/登录登出/
  auth_version 失效/403 与禁用/渠道 CRUD 幂等/连通测试/options 热更/令牌 key 一次性与缓存失效
  联动/用户 CRUD 改密失效旧会话/防自锁/级联删除/兑换码批量/日志过滤）、gateway 限流 4 例
  （窗口边界/分组覆盖/TPM/RPM）、cmd 管理面装配 1 例；`go test ./... -race` 全绿，vet 零告警。
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

- M2-wave1：docs/05 新增第五节管理面契约（通用约定/端点明细/错误码表/限流契约）；docs/01 目录结构与落地进度同步（internal/api、internal/ratelimit）；docs/10 ROADMAP 拆 M2 三波、状态块重写与新增交接记录五条；docs/11 踩坑追加三条；新增 ADR 0005（会话鉴权标准化与限流挂接解耦：bcrypt 用 x/crypto、HMAC cookie、手写滑动窗口、整对象幂等写）。
- M1-wave3：docs/01 设计点 4 落地标注与目录结构同步；docs/04 落地标注 + 新增「公式实测」节（四条黄金样例数值对齐）；docs/05 错误码表补 403/503 计费语义与计费运行轨键；docs/10 ROADMAP 重排（M1 完成、M2 重定义管理面 API）与状态块重写；docs/11 踩坑追加；新增 ADR 0004（计费引擎落地：expr 选型与 tier 单层语义）。
- M1-wave2：docs/01 设计点 2/3 落地标注与目录结构同步（internal/channel 落地为 internal/gateway）；docs/05 转发面契约细化（请求/响应示例与错误码表）；docs/11 踩坑追加两条；docs/10 状态块与交接记录更新。
- 新增 ADR 0003（存储层落地：单写多读与原子快照热更）；docs/01、docs/03 同步落地状态与实现细节；docs/02 补充 build.ps1 目标说明；docs/11 踩坑记录追加三条。

### 既有（M0）

- 初始化仓库工程体系：目录骨架、AGENTS.md 协作规范、docs/ 全套设计文档、两篇 ADR。
- 最小可运行入口 `cmd/hui-api`：`-version` 版本信息与 `/health` 健康检查占位实现，附单测。
- CI 流水线：竞品词 grep 扫描、go vet、go test、前端构建占位（web/ 存在后自动启用）。
- 协作模板：PR 模板、bug/feature Issue 表单、release 分类配置、竞品词表与本地扫描脚本。
