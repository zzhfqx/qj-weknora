-- BrowserSkill: local browser automation (v0.8.2 feature)
-- Three tables: browser_devices (authorized browsers), browser_pairings
-- (temporary pairing tokens), browser_task_interruptions (interrupt state).

-- Authorized browser devices. A browser extension pairs once and receives a
-- durable device id + secret; subsequent connections authenticate with it.
CREATE TABLE IF NOT EXISTS browser_devices (
    id VARCHAR(64) PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    user_id VARCHAR(64) NOT NULL,
    device_name VARCHAR(255) NOT NULL DEFAULT '',
    browser_name VARCHAR(128) NOT NULL DEFAULT '',
    browser_version VARCHAR(64) NOT NULL DEFAULT '',
    os_name VARCHAR(64) NOT NULL DEFAULT '',
    last_seen_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMP WITH TIME ZONE
);

CREATE INDEX IF NOT EXISTS idx_browser_devices_tenant ON browser_devices(tenant_id);
CREATE INDEX IF NOT EXISTS idx_browser_devices_user ON browser_devices(tenant_id, user_id);
CREATE INDEX IF NOT EXISTS idx_browser_devices_deleted_at ON browser_devices(deleted_at);

-- Pairing tokens: short-lived tokens generated on the server side that a
-- browser extension can use to register itself. The token is consumed on
-- first use.
CREATE TABLE IF NOT EXISTS browser_pairings (
    token VARCHAR(64) PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    user_id VARCHAR(64) NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    consumed_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_browser_pairings_tenant ON browser_pairings(tenant_id);
CREATE INDEX IF NOT EXISTS idx_browser_pairings_expires ON browser_pairings(expires_at);

-- Task interruptions: when a BrowserSkill task is interrupted (e.g. human
-- requested help, navigation away), the state is persisted so the agent
-- can resume on the next turn without losing context.
CREATE TABLE IF NOT EXISTS browser_task_interruptions (
    id SERIAL PRIMARY KEY,
    tenant_id BIGINT NOT NULL,
    user_id VARCHAR(64) NOT NULL,
    session_id VARCHAR(64) NOT NULL,
    task_id VARCHAR(64) NOT NULL,
    state JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_browser_interruptions_session ON browser_task_interruptions(tenant_id, session_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_browser_interruptions_task ON browser_task_interruptions(tenant_id, session_id, task_id);
