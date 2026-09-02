-- hui-api 六表基线 DDL（schema 版本 1）。
-- 与 internal/model 的 GORM 模型等价：AutoMigrate（代码源）与本文（文档源）双源一致，
-- 供人工核对、外部 SQL 工具导入与故障重建使用。
-- 迁移规则见 docs/03-数据模型与迁移.md 第四节：只前进、禁止修改历史迁移。
-- quota 单位：500000 quota = $1；时间列统一 unix 秒；expired_time = -1 表示永不过期。

CREATE TABLE IF NOT EXISTS `channels` (
    `id`            integer PRIMARY KEY AUTOINCREMENT,
    `name`          text    NOT NULL DEFAULT '',
    `type`          integer NOT NULL DEFAULT 1,
    `base_url`      text    NOT NULL DEFAULT '',
    `key`           text    NOT NULL DEFAULT '',
    `models`        text    NOT NULL DEFAULT '',
    `priority`      integer NOT NULL DEFAULT 0,
    `weight`        integer NOT NULL DEFAULT 0,
    `status`        integer NOT NULL DEFAULT 1,
    `created_time`  integer NOT NULL DEFAULT 0,
    `updated_time`  integer NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS `tokens` (
    `id`               integer PRIMARY KEY AUTOINCREMENT,
    `user_id`          integer NOT NULL DEFAULT 0,
    `name`             text    NOT NULL DEFAULT '',
    `key`              text,
    `key_hash`         text    NOT NULL,
    `status`           integer NOT NULL DEFAULT 1,
    `quota`            integer NOT NULL DEFAULT 0,
    `remain_quota`     integer NOT NULL DEFAULT 0,
    `unlimited_quota`  numeric NOT NULL DEFAULT false,
    `budget_duration`  text    NOT NULL DEFAULT '',
    `budget_reset_at`  integer NOT NULL DEFAULT 0,
    `tpm_rpm`          text    NOT NULL DEFAULT '',
    `tags`             text    NOT NULL DEFAULT '',
    `expired_time`     integer NOT NULL DEFAULT -1,
    `created_time`     integer NOT NULL DEFAULT 0,
    `accessed_time`    integer NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS `idx_tokens_key` ON `tokens`(`key`);
CREATE UNIQUE INDEX IF NOT EXISTS `idx_tokens_key_hash` ON `tokens`(`key_hash`);
CREATE INDEX IF NOT EXISTS `idx_tokens_user_id` ON `tokens`(`user_id`);

CREATE TABLE IF NOT EXISTS `users` (
    `id`               integer PRIMARY KEY AUTOINCREMENT,
    `username`         text    NOT NULL,
    `password_hash`    text    NOT NULL DEFAULT '',
    `display_name`     text    NOT NULL DEFAULT '',
    `role`             integer NOT NULL DEFAULT 1,
    `status`           integer NOT NULL DEFAULT 1,
    `quota`            integer NOT NULL DEFAULT 0,
    `used_quota`       integer NOT NULL DEFAULT 0,
    `email`            text    NOT NULL DEFAULT '',
    `created_time`     integer NOT NULL DEFAULT 0,
    `last_login_time`  integer NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS `idx_users_username` ON `users`(`username`);
CREATE INDEX IF NOT EXISTS `idx_users_email` ON `users`(`email`);

CREATE TABLE IF NOT EXISTS `redemptions` (
    `id`            integer PRIMARY KEY AUTOINCREMENT,
    `key`           text    NOT NULL,
    `name`          text    NOT NULL DEFAULT '',
    `status`        integer NOT NULL DEFAULT 1,
    `quota`         integer NOT NULL DEFAULT 0,
    `created_by`    integer NOT NULL DEFAULT 0,
    `used_by`       integer NOT NULL DEFAULT 0,
    `used_time`     integer NOT NULL DEFAULT 0,
    `expired_time`  integer NOT NULL DEFAULT -1,
    `created_time`  integer NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS `idx_redemptions_key` ON `redemptions`(`key`);

CREATE TABLE IF NOT EXISTS `options` (
    `key`    text    PRIMARY KEY,
    `value`  text    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS `logs` (
    `id`                 integer PRIMARY KEY AUTOINCREMENT,
    `user_id`            integer NOT NULL DEFAULT 0,
    `token_id`           integer NOT NULL DEFAULT 0,
    `channel_id`         integer NOT NULL DEFAULT 0,
    `protocol`           text    NOT NULL DEFAULT '',
    `model_name`         text    NOT NULL DEFAULT '',
    `prompt_tokens`      integer NOT NULL DEFAULT 0,
    `completion_tokens`  integer NOT NULL DEFAULT 0,
    `quota`              integer NOT NULL DEFAULT 0,
    `use_time`           integer NOT NULL DEFAULT 0,
    `is_stream`          numeric NOT NULL DEFAULT false,
    `detail`             text    NOT NULL DEFAULT '',
    `created_time`       integer NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS `idx_logs_user_id` ON `logs`(`user_id`);
CREATE INDEX IF NOT EXISTS `idx_logs_token_id` ON `logs`(`token_id`);
CREATE INDEX IF NOT EXISTS `idx_logs_channel_id` ON `logs`(`channel_id`);
CREATE INDEX IF NOT EXISTS `idx_logs_model_name` ON `logs`(`model_name`);
CREATE INDEX IF NOT EXISTS `idx_logs_created_time` ON `logs`(`created_time`);
