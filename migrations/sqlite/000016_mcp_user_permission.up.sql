-- 迁移 000016：mcp_user_permission，fqx 二开内置 MCP 按用户身份裁决授权表
-- （对应 PG 版 000216）。每个工作空间的每个外部用户一行，knowledge_base_ids
-- 为可读知识库 id 的 JSON 数组（SQLite 上以 TEXT 存储，GORM StringArray
-- 的 Value/Scan 负责 JSON 编解码，与 mcp_endpoints 同款）。
-- 该表由外部平台（千机）建库/共享流程写入，weknora MCP 层只读。

CREATE TABLE IF NOT EXISTS mcp_user_permission (
    id VARCHAR(36) PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    user_id VARCHAR(128) NOT NULL DEFAULT '',
    knowledge_base_ids TEXT NOT NULL DEFAULT '[]',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- 裁决热路径为 (tenant_id, user_id) 点查；唯一索引保证一用户一行。
CREATE UNIQUE INDEX IF NOT EXISTS idx_mcp_user_permission_user
    ON mcp_user_permission (tenant_id, user_id);
