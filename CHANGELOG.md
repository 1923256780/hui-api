# 更新日志

本项目所有显著变更将记录在本文件中。

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循[语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 新增

- 初始化仓库工程体系：目录骨架、AGENTS.md 协作规范、docs/ 全套设计文档、两篇 ADR。
- 最小可运行入口 `cmd/hui-api`：`-version` 版本信息与 `/health` 健康检查占位实现，附单测。
- CI 流水线：竞品词 grep 扫描、go vet、go test、前端构建占位（web/ 存在后自动启用）。
- 协作模板：PR 模板、bug/feature Issue 表单、release 分类配置、竞品词表与本地扫描脚本。
