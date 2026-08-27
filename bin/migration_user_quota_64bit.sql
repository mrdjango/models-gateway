-- 用户额度列升级为 64 位。
--
-- 背景
-- ----
-- 上游把额度上限拆成两个域：单次请求扣费仍是 int32（common.MaxQuota），
-- 钱包余额/充值/兑换改用 common.MaxWalletQuota（2^53-1）。同时 InitDB 里
-- 新增了 ensureUserQuotaColumns 启动校验：MySQL/PostgreSQL 上如果 users 的
-- quota / used_quota / aff_quota / aff_history 仍是 32 位就直接拒绝启动。
--
-- 上游没有配套迁移，模型标签也仍写着 `type:int`——建表时会照抄这个标签，
-- 于是全新部署第一次建表得到 32 位列（此时表不存在，校验被跳过），
-- 第二次启动就会卡在自己的校验上。本仓库因此把这四列的标签改成
-- `type:bigint`（见 model/user.go），让模型声明与该校验保持一致。
--
-- 适用范围
-- --------
-- 生产环境 models_gateway 库已经是 bigint（2026-08-27 核对），无需执行本脚本。
-- 本脚本用于其他仍是 32 位的环境：旧的 MySQL 部署、按上游标签建过表的实例等。
-- 执行前请先确认当前列类型：
--
--   SELECT column_name, data_type FROM information_schema.columns
--    WHERE table_name = 'users'
--      AND column_name IN ('quota','used_quota','aff_quota','aff_history');
--
-- 四列都已是 bigint / int8 就不用动。ALTER 会锁表，请先备份并挑低峰期执行。

-- PostgreSQL
ALTER TABLE users
    ALTER COLUMN quota TYPE bigint,
    ALTER COLUMN used_quota TYPE bigint,
    ALTER COLUMN aff_quota TYPE bigint,
    ALTER COLUMN aff_history TYPE bigint;

-- MySQL（用 MySQL 时请注释掉上面的 PostgreSQL 语句，改用下面这条）
-- 这里刻意不加 NOT NULL：模型标签只有 default，列本身可空，
-- 加上 NOT NULL 会改变原有约束，存量数据里若有 NULL 还会直接失败。
-- ALTER TABLE users
--     MODIFY COLUMN quota bigint DEFAULT 0,
--     MODIFY COLUMN used_quota bigint DEFAULT 0,
--     MODIFY COLUMN aff_quota bigint DEFAULT 0,
--     MODIFY COLUMN aff_history bigint DEFAULT 0;

-- SQLite 无需迁移：INTEGER 本身就是 64 位，启动校验也会跳过。
