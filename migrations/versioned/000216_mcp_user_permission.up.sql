-- Migration: 000216_mcp_user_permission
-- fqx 二开：内置 MCP 按用户身份裁决的授权表。
-- 每个工作空间的每个外部（千机）用户有且仅有一行；knowledge_base_ids
-- 为该用户经 MCP 端点可读的知识库 id JSON 数组（与 mcp_endpoints 同款
-- jsonb）。表由千机侧建库/共享授权流程写入，weknora MCP 层只读。
DO $$ BEGIN RAISE NOTICE '[Migration 000216] Creating mcp_user_permission table'; END $$;

CREATE TABLE IF NOT EXISTS mcp_user_permission (
    id VARCHAR(36) PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id BIGINT NOT NULL,
    user_id VARCHAR(128) NOT NULL DEFAULT '',
    knowledge_base_ids JSONB NOT NULL DEFAULT '[]',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- 裁决热路径固定为 (tenant_id, user_id) 单行点查，唯一约束同时保证
-- "一用户一行"语义（千机侧写入用 upsert）。
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_user_permission_user
    ON mcp_user_permission (tenant_id, user_id);

COMMENT ON TABLE mcp_user_permission IS 'fqx 二开：内置 MCP 按外部用户的可读知识库授权（一用户一行），由外部平台写入，MCP 层只读';
COMMENT ON COLUMN mcp_user_permission.user_id IS '外部（千机）用户标识，不建立 weknora 影子用户';
COMMENT ON COLUMN mcp_user_permission.knowledge_base_ids IS '可读知识库 id JSON 数组；空数组或无行=无权使用任何 MCP 知识库工具';
