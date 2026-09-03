-- 0004: M3-wave1 商业化基础列与新表（2026-09-03）。
--
-- users 新增列（邀请与两步验证）：
--   aff_code           本用户的邀请码（注册携带他人的 aff_code 建立邀请关系）；
--   inviter_id         邀请人用户 ID（0 = 非邀请注册）；
--   aff_history_quota  邀请返利累计入账（quota）；
--   totp_secret        TOTP 密钥（wave1 仅建列，绑定流程 wave2 落地）；
--   totp_enabled       两步验证开关。
--
-- 新表 user_identities：第三方身份绑定（(provider, provider_uid) 复合唯一）。
-- 新表 topup_orders：在线充值订单（order_no 唯一，状态 1待支付/2已支付/3失败/4过期）。
--
-- 规则：up-only 迁移，只前进不回退；禁止修改本文件（有误时新增迁移修正）。
-- 说明：实际 schema 由 AutoMigrate 以 internal/model 为源自动补齐；
-- 本目录全部脚本按文件名顺序执行后必须与 AutoMigrate 产物逐表列集合一致
-- （由 internal/store 的 TestDDLEquivalence 固化）。

ALTER TABLE users ADD COLUMN aff_code TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN inviter_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN aff_history_quota INTEGER NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN totp_secret TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN totp_enabled NUMERIC NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS idx_users_aff_code ON users(aff_code);
CREATE INDEX IF NOT EXISTS idx_users_inviter_id ON users(inviter_id);

CREATE TABLE IF NOT EXISTS `user_identities` (
    `id`            integer PRIMARY KEY AUTOINCREMENT,
    `user_id`       integer NOT NULL DEFAULT 0,
    `provider`      text    NOT NULL DEFAULT '',
    `provider_uid`  text    NOT NULL DEFAULT '',
    `created_time`  integer NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS `idx_user_identities_provider_uid` ON `user_identities`(`provider`, `provider_uid`);
CREATE INDEX IF NOT EXISTS `idx_user_identities_user_id` ON `user_identities`(`user_id`);

CREATE TABLE IF NOT EXISTS `topup_orders` (
    `id`            integer PRIMARY KEY AUTOINCREMENT,
    `order_no`      text    NOT NULL,
    `user_id`       integer NOT NULL DEFAULT 0,
    `gateway`       text    NOT NULL DEFAULT '',
    `amount_cents`  integer NOT NULL DEFAULT 0,
    `currency`      text    NOT NULL DEFAULT 'CNY',
    `quota`         integer NOT NULL DEFAULT 0,
    `rate`          integer NOT NULL DEFAULT 0,
    `status`        integer NOT NULL DEFAULT 1,
    `trade_no`      text    NOT NULL DEFAULT '',
    `detail`        text    NOT NULL DEFAULT '',
    `paid_time`     integer NOT NULL DEFAULT 0,
    `created_time`  integer NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS `idx_topup_orders_order_no` ON `topup_orders`(`order_no`);
CREATE INDEX IF NOT EXISTS `idx_topup_orders_user_id` ON `topup_orders`(`user_id`);
CREATE INDEX IF NOT EXISTS `idx_topup_orders_trade_no` ON `topup_orders`(`trade_no`);
