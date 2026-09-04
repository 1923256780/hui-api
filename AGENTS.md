# AGENTS.md — hui-api 文档中枢与协作须知

> 所有 AI 会话与人类贡献者的统一入口，也是全部文档的**唯一索引中枢**（docs/INDEX.md 已收敛为指向本文件的指引壳）。
> 开始任何任务前先读完本文件，再按第 2 节索引定位子文档；结束后按第 4 节规则回写。

## 1. 项目概览与铁律

- hui-api：自研轻量 LLM API 网关，Go（后端）+ React（前端），Apache-2.0。
- 核心目标：单二进制 + SQLite 零依赖部署；可靠多渠道路由与熔断；令牌预算与兑换码体系；配置热更新；计费可对账。
- **clean-room 纪律**：本项目为独立设计实现。任何会话不得阅读其他同类网关的源码；功能规格只来自公开文档与黑盒行为观察。
- **竞品匿名纪律**：任何文档/注释/commit 不得出现其他同类项目名称，词表见 `.github/competitor-words.txt`，CI grep 拦截；旧网关一律中性称呼「旧网关（legacy）」。
- **端口决策**：3100 为长期生产端口（用户决策 2026-09-04），不切 3000；启用 runbook 见 docs/06 第七节。

## 2. 文档地图（唯一索引，一跳可达）

### 2.1 主题文档（docs/）

| 文档 | 一行职责 | 何时读 |
| --- | --- | --- |
| [00-项目概览与目标](docs/00-项目概览与目标.md) | 定位、动机、M0-M5 总路线 | 了解项目是什么、为什么做 |
| [01-架构总览](docs/01-架构总览.md) | 目标架构 10 设计点 + 逐点落地注记（活文档） | 动代码前；结构性改动后必须回写 |
| [02-环境搭建与命令](docs/02-环境搭建与命令.md) | Go/Node 安装、GOPROXY、构建测试运行命令 | 配环境、忘命令时 |
| [03-数据模型与迁移](docs/03-数据模型与迁移.md) | 六表设计、quota 单位、迁移规则与迁移工具用法 | 涉及表结构/存储/迁移的任务 |
| [04-计费与配额设计](docs/04-计费与配额设计.md) | 三模式计费、预扣费、部分结算、黄金测试集 | 涉及计费/配额的任务 |
| [05-API契约](docs/05-API契约.md) | 转发面/管理面/商业化端点清单与契约约定 | 涉及 API 面的任务 |
| [06-部署与运维](docs/06-部署与运维.md) | 交叉编译、systemd、备份、3100 直连启用 runbook 与数据分叉警示 | 部署/上线/切换/演练任务 |
| [07-工具与脚本索引](docs/07-工具与脚本索引.md) | scripts/ 全部脚本用途、用法与使用约定 | 用脚本、新增脚本时 |
| [10-里程碑与进度](docs/10-里程碑与进度.md) | ROADMAP 与「当前状态」交接块（进度唯一事实源） | 每次会话开始与结束 |
| [11-踩坑记录](docs/11-踩坑记录.md) | 踩坑模板与历史记录 | 排查问题前先翻；踩坑时追加 |
| [MAINTENANCE.md](docs/MAINTENANCE.md) | 会话收尾检查清单（6 项 + 索引一致性） | 每次会话结束必查 |
| [INDEX.md](docs/INDEX.md) | docs/ 目录指引壳（已收敛指向本文件，不独立维护索引表） | 误入 docs/ 找入口时 |

### 2.2 ADR 决策记录（docs/decisions/）

| ADR | 一行摘要 |
| --- | --- |
| [0001](docs/decisions/0001-技术栈选型-go-react.md) | 技术栈选型：Go + React；零 CGO 交叉编译，glebarez/sqlite 纯 Go 驱动 |
| [0002](docs/decisions/0002-独立实现与兼容边界.md) | 独立实现与兼容边界：clean-room 自研，兼容收敛为数据可迁移 |
| [0003](docs/decisions/0003-存储层落地-单写多读与原子快照热更.md) | 存储层：单写多读连接池 + AutoMigrate 基线 + options 原子快照热更 |
| [0004](docs/decisions/0004-计费引擎落地-expr选型与tier单层语义.md) | 计费引擎：expr 选型、口径唯一化、tier() 单层语义、未配价 503 |
| [0005](docs/decisions/0005-管理面落地-会话鉴权标准化与限流挂接解耦.md) | 管理面：bcrypt + HMAC 签名 cookie、PUT 整对象幂等、限流解耦挂接 |
| [0006](docs/decisions/0006-商业化开关-options白名单与敏感值哨兵策略.md) | 商业化开关：options 白名单、敏感值哨兵脱敏、默认关不落库 |
| [0007](docs/decisions/0007-支付回调-双验签差异与条件更新幂等.md) | 支付回调：双验签差异、金额逐位校验、条件更新幂等结算 |
| [0008](docs/decisions/0008-旧库只读纪律与幂等覆盖式同步.md) | 迁移工具：旧库只读 + 自然键 upsert 幂等同步 + 强对账确定性报告 |
| [0009](docs/decisions/0009-生产旁路部署拓扑与资源限额.md) | 生产旁路拓扑（127.0.0.1:3100）与双层内存限额 |
| [_template](docs/decisions/_template.md) | 新 ADR 模板（编号递增；已接受 ADR 不改决策正文，可追加补记） |

### 2.3 关键脚本（scripts/，用法详见 docs/07）

| 脚本 | 用途 |
| --- | --- |
| [build.ps1](scripts/build.ps1) | 一键 build/test/vet/run/verify/web |
| [check-competitor-words.ps1](scripts/check-competitor-words.ps1) | 本地竞品词扫描（与 CI 同逻辑，提交前必跑） |
| [migrate-drill.ps1](scripts/migrate-drill.ps1) | 本地迁移演练编排（2 遍迁移 + 隔离冒烟 + 确定性 diff） |
| [e2e-run.ps1](scripts/e2e-run.ps1) / [e2e-smoke.ps1](scripts/e2e-smoke.ps1) | 隔离 e2e 环境启动（3100 隔离库）/ 22 项业务链路冒烟 |
| [deploy-smoke-server.sh](scripts/deploy-smoke-server.sh) | 服务器部署冒烟 7 项（HUI_BASE 可覆盖目标端口） |

## 3. 文档层级与修改权限

| 层级 | 载体 | 修改权限与触发条件 |
| --- | --- | --- |
| L1 中枢 | 本文件 + docs/INDEX.md（指引壳） | 只承载索引与规则；文档/ADR 增删改名、命令、纪律变化时更新，禁止堆砌子文档内容 |
| L2 主题 | docs/NN-*.md | 领域事实（架构/契约/流程/运维）唯一定义处；随代码落地或变更时回写，冲突时以代码为准 |
| L3 决策 | docs/decisions/ADR | 选型/结构性决策定案时新增（编号递增 + 登记 L1 索引）；正文只追加补记不回改 |
| L4 代码 | 包注释与 godoc | 实现细节自洽随代码提交；与 L2 冲突时触发 L2 回写 |

## 4. 自回归维护规则（文档系统如何维护自己）

1. **登记义务**：新增/删除/改名任何文档或 ADR，必须在同一改动内登记/注销进第 2 节索引（含一行摘要），否则视为**交付未完成**。
2. **代码为准**：文档与代码冲突时以代码为准并立即回写修正；无法立即修的记入 docs/10 风险与未决。
3. **冷启动流程**：新会话 → 读本文件 → 读 docs/10「当前状态」块 → 按第 2 节索引定位本次任务相关子文档 → 遵守其契约。
4. **收尾检查**：按 docs/MAINTENANCE.md 清单逐项自查（含索引一致性、CHANGELOG [Unreleased]、docs/10 状态块与交接记录）。
5. **禁止复制**：子文档内容不得整段搬入本文件；本文件只做索引、规则与速查。

## 5. 环境与命令（速查，细节见 docs/02）

- 构建：`go build ./...`；测试：`go test ./...`；静态检查：`go vet ./...`
- 本地运行：`go run ./cmd/hui-api -addr :3100`；健康检查 `curl http://127.0.0.1:3100/health`
- 一键脚本：`powershell -ExecutionPolicy Bypass -File scripts/build.ps1 verify`
- 前端：`cd web; npm ci; npm run lint; npm run build`（产物经 go:embed 嵌入二进制）

## 6. 代码风格与 DoD

- Go：gofmt 必须通过；错误用 `%w` 包装并附带业务上下文；禁止用 panic 传递业务错误。
- React：一律函数组件 + hooks，不写 class 组件；样式集中管理；文档/脚本/前端文件 kebab-case。
- 提交：Conventional Commits（feat/fix/docs/chore/refactor/test），message 不得出现竞品名。
- DoD：新改动附测试且 `go test ./...` 全绿；schema 改动走迁移并兼容历史数据；计费改动过 docs/04 黄金测试集；`go vet` 零告警；文档未按第 4 节同步视为未完成。

## 7. 安全与禁区

- 禁止提交任何密钥、token、真实数据库文件（`*.db` 已 .gitignore）。
- 禁止未经用户确认操作生产服务器（部署、重启、改库均需逐次确认）。
- 禁止把用户数据、密钥、日志原文粘贴到 issue/commit/文档。
- 提交前跑 `scripts/check-competitor-words.ps1`；`git add -A` 前先 `git status --short` 检查未跟踪文件（防误收遗留脚本）。
