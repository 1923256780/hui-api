# 更新日志

本项目所有显著变更将记录在本文件中。

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

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

- 新增 ADR 0003（存储层落地：单写多读与原子快照热更）；docs/01、docs/03 同步落地状态与实现细节；docs/02 补充 build.ps1 目标说明；docs/11 踩坑记录追加三条。

### 既有（M0）

- 初始化仓库工程体系：目录骨架、AGENTS.md 协作规范、docs/ 全套设计文档、两篇 ADR。
- 最小可运行入口 `cmd/hui-api`：`-version` 版本信息与 `/health` 健康检查占位实现，附单测。
- CI 流水线：竞品词 grep 扫描、go vet、go test、前端构建占位（web/ 存在后自动启用）。
- 协作模板：PR 模板、bug/feature Issue 表单、release 分类配置、竞品词表与本地扫描脚本。
