-- ============================================================================
-- 阶段 0：为 MCP 侧车创建 PostgreSQL 只读账号
-- ----------------------------------------------------------------------------
-- 本脚本不自动执行：请由数据库管理员（当前由用户本人）核对后手工运行。
--
-- 用法（密码通过 psql 变量传入，避免写进脚本/历史文件）：
--   psql "postgresql://postgres@<host>:5432/WeKnora" \
--     -v sidecar_password='这里换成强密码' \
--     -f deploy/01_readonly_role.sql
--
-- 权限边界：
--   * 只能登录本库；
--   * 仅 mcp_endpoints、mcp_user_permission 两张二开自有表的 SELECT；
--   * 语句级 3 秒超时，防止侧车异常拖慢数据库。
-- 重复执行安全（角色已存在则跳过创建，授权语句幂等）。
-- ============================================================================

-- 未提供密码变量时直接中止，防止误建空密码账号
\if :{?sidecar_password}
\else
\echo '错误：请用 -v sidecar_password=''强密码'' 传入密码'
\set ON_ERROR_STOP on
SELECT 1 / 0;  -- 故意失败以中止脚本
\endif

SELECT '创建/复用只读角色 mcp_sidecar_ro' AS step;
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'mcp_sidecar_ro') THEN
        EXECUTE format('CREATE ROLE mcp_sidecar_ro LOGIN PASSWORD %L', :'sidecar_password');
    ELSE
        RAISE NOTICE '角色已存在，跳过创建（如需改密请另行 ALTER ROLE）';
    END IF;
END $$;

GRANT CONNECT ON DATABASE "WeKnora" TO mcp_sidecar_ro;
GRANT USAGE ON SCHEMA public TO mcp_sidecar_ro;

-- 只授两张表的读权限；侧车 SQL 固定显式列名，不做 SELECT *
GRANT SELECT ON mcp_endpoints TO mcp_sidecar_ro;
GRANT SELECT ON mcp_user_permission TO mcp_sidecar_ro;

-- 只读账号兜底：单条语句超过 3 秒自动取消
ALTER ROLE mcp_sidecar_ro SET statement_timeout = '3s';
