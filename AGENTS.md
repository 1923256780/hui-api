# AGENTS.md — hui-api 仓库协作须知

> 所有 AI 会话与人类贡献者的统一入口。开始任何任务前必须先读完本文件。

## 1. 项目概览

- hui-api：自研轻量 LLM API 网关，Go（后端）+ React（前端），Apache-2.0。
- 核心目标：单二进制 + SQLite 零依赖部署；可靠多渠道路由与熔断；令牌预算与兑换码体系；配置热更新；计费可对账。
- **clean-room 纪律**：本项目为独立设计实现。任何会话不得阅读其他同类网关的源码；功能规格只来自公开文档与黑盒行为观察。
- **竞品匿名纪律**：仓库内任何文档/注释/commit 信息不得出现其他同类项目名称，词表见 `.github/competitor-words.txt`，CI 会 grep 拦截。

## 2. 仓库地图

| 目录 | 职责 |
| --- | --- |
| cmd/hui-api/ | 程序入口 main.go 与健康检查 |
| internal/relay/ | 双协议转发核心（M1 规划） |
| internal/channel/ | 渠道路由与熔断（M1 规划） |
| internal/billing/ | 计费与配额内核（M2 规划） |
| internal/store/ | GORM + SQLite 存储层与迁移（M1 规划） |
| web/ | React 管理台（M2-wave2 落地：api 客户端/八页面，go:embed 嵌入） |
| docs/ | 全部设计与运维文档，入口 INDEX.md |
| docs/decisions/ | ADR 架构决策记录 |
| scripts/ | 构建、检查脚本 |
| .github/ | CI、Issue/PR 模板、竞品词表 |

## 3. 环境与命令

- 依赖：Go ≥ 1.23、Node ≥ 20、git 2.40+、gh（可选）。安装细节见 docs/02-环境搭建与命令.md。
- 构建：`go build ./...`
- 测试：`go test ./...`
- 静态检查：`go vet ./...`
- 本地运行：`go run ./cmd/hui-api -addr :3100`，健康检查：`curl http://127.0.0.1:3100/health`
- 一键脚本：`powershell -ExecutionPolicy Bypass -File scripts/build.ps1 verify`（build/test/vet/run/verify）
- 前端（web/ 目录存在后）：`cd web; npm ci; npm run lint; npm run build`，产物经 go:embed 嵌入二进制。

## 4. 代码风格

- Go：gofmt 必须通过；错误必须用 `%w` 包装并附带业务上下文；禁止用 panic 传递业务错误。
- React：一律函数组件 + hooks，不写 class 组件；样式集中管理。
- 文件名：Go 文件遵循标准库惯例，其余（文档/脚本/前端）一律 kebab-case。
- 提交信息：Conventional Commits（feat/fix/docs/chore/refactor/test），message 中不得出现竞品名。

## 5. 测试与验收 DoD

- 新增/修改的代码必须附带测试，`go test ./...` 全绿才算完成。
- 涉及表结构的改动必须编写迁移逻辑并保证既有数据兼容，禁止修改历史迁移。
- 计费相关改动必须通过 docs/04-计费与配额设计.md 定义的黄金测试集。
- `go vet ./...` 无告警；文档未按第 7 章同步，视为未完成。

## 6. 安全与禁区

- 禁止提交任何密钥、token、真实数据库文件（`*.db` 已在 .gitignore）。
- 竞品匿名纪律：任何文档/注释/commit 不得出现其他同类项目名称；扫描范围排除词表文件与检查脚本自身。
- 禁止未经用户确认操作生产服务器（部署、重启、改库均需逐次确认）。
- 禁止把用户数据、密钥、日志原文粘贴到 issue/commit/文档。

## 7. 文档与自迭代规则

- 开始任务前：先读 `docs/INDEX.md` 与 `docs/10-里程碑与进度.md` 的「当前状态」块。
- 收尾时：按 `docs/MAINTENANCE.md` 的 6 项检查清单逐项自查并更新文档。
- 结构性改动（模块边界、目录结构、数据模型）必须同步 `docs/01-架构总览.md` 并新增一条 ADR。
- 踩坑经验随手追加 `docs/11-踩坑记录.md`。
- 没更新文档 = 没完成。

## 8. 会话流程

- 开始：读 AGENTS.md → docs/INDEX.md → docs/10-里程碑与进度.md 当前状态块 → 按任务读主题文档。
- 结束：完成 MAINTENANCE.md 六项检查；在 docs/10-里程碑与进度.md 的「当前状态」块写入交接摘要（已完成 / 下一步 / 风险与未决）。
