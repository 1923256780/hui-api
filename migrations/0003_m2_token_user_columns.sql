-- 0003: M2-wave1 管理面基础列（2026-09-03）。
--
-- tokens.group        令牌分组：计费 GroupRatio 组倍率与分组级限流的归属组（缺省 default）；
-- tokens.model_limits 模型白名单（逗号分隔，空=不限）；
-- tokens.allow_ips    IP 白名单（逗号分隔 IP/CIDR，空=不限）；
-- users.group         用户默认分组（管理面创建令牌的缺省归属组）；
-- users.auth_version  会话版本：递增使既有登录会话全部失效（改密后旧签名 cookie 失效）。
--
-- 规则：up-only 迁移，只前进不回退；禁止修改本文件（有误时新增迁移修正）。
-- 说明：实际 schema 由 AutoMigrate 以 internal/model 为源自动补齐该批列；
-- 本目录全部脚本按文件名顺序执行后必须与 AutoMigrate 产物逐表列集合一致
-- （由 internal/store 的 TestDDLEquivalence 固化）。

ALTER TABLE tokens ADD COLUMN "group" TEXT NOT NULL DEFAULT 'default';
ALTER TABLE tokens ADD COLUMN model_limits TEXT NOT NULL DEFAULT '';
ALTER TABLE tokens ADD COLUMN allow_ips TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN "group" TEXT NOT NULL DEFAULT 'default';
ALTER TABLE users ADD COLUMN auth_version INTEGER NOT NULL DEFAULT 0;
