-- 0002: channels 新增 param_override 列（M1-wave2，2026-09-02）。
--
-- 用途：渠道级请求参数改写配置，JSON 文本，语义与操作集见 internal/override。
-- 规则：up-only 迁移，只前进不回退；禁止修改本文件（有误时新增 0003 修正）。
-- 说明：实际 schema 由 AutoMigrate 以 internal/model 为源自动补齐该列；
-- 本目录全部脚本按文件名顺序执行后必须与 AutoMigrate 产物逐表列集合一致
-- （由 internal/store 的 TestDDLEquivalence 固化）。
ALTER TABLE channels ADD COLUMN param_override TEXT NOT NULL DEFAULT '';
