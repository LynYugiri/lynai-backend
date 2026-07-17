CREATE TABLE IF NOT EXISTS users (
    id BIGINT PRIMARY KEY,
    phone TEXT NOT NULL,
    password_hash TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL,
    email TEXT,
    phone_verified BOOLEAN DEFAULT TRUE,
    avatar_url TEXT,
    is_admin BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_phone ON users (phone);

CREATE TABLE IF NOT EXISTS user_sessions (
    id VARCHAR(64) PRIMARY KEY,
    user_id BIGINT NOT NULL,
    refresh_jti VARCHAR(64) NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_user_sessions_user_id ON user_sessions (user_id);
CREATE INDEX IF NOT EXISTS idx_user_sessions_expires_at ON user_sessions (expires_at);
CREATE INDEX IF NOT EXISTS idx_user_sessions_revoked_at ON user_sessions (revoked_at);

CREATE TABLE IF NOT EXISTS plugins (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    author TEXT NOT NULL,
    description TEXT NOT NULL,
    version TEXT NOT NULL,
    icon_url TEXT,
    category TEXT,
    zip_path TEXT NOT NULL,
    sha256 TEXT,
    download_count BIGINT DEFAULT 0,
    status TEXT DEFAULT 'pending',
    submitted_by BIGINT NOT NULL,
    reviewed_by BIGINT,
    review_note TEXT,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_plugins_status ON plugins (status);
CREATE INDEX IF NOT EXISTS idx_plugins_submitted_by ON plugins (submitted_by);

CREATE TABLE IF NOT EXISTS plugin_screenshots (
    id BIGSERIAL PRIMARY KEY,
    plugin_id TEXT NOT NULL REFERENCES plugins(id),
    url TEXT NOT NULL,
    sort_order BIGINT DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_plugin_screenshots_plugin_id ON plugin_screenshots (plugin_id);

CREATE TABLE IF NOT EXISTS plugin_permissions (
    id BIGSERIAL PRIMARY KEY,
    plugin_id TEXT NOT NULL REFERENCES plugins(id),
    permission TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_plugin_permissions_plugin_id ON plugin_permissions (plugin_id);

CREATE TABLE IF NOT EXISTS sync_metadata (
    user_id BIGINT PRIMARY KEY,
    last_seq BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS sync_changes (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    seq BIGINT NOT NULL,
    table_name TEXT NOT NULL,
    op TEXT NOT NULL,
    record_id TEXT NOT NULL,
    data TEXT,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_seq ON sync_changes (user_id, seq);

CREATE TABLE IF NOT EXISTS sync_blobs (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    sha256 TEXT NOT NULL,
    size BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_blob ON sync_blobs (user_id, sha256);

CREATE TABLE IF NOT EXISTS relay_providers (
    id BIGINT PRIMARY KEY,
    name TEXT NOT NULL,
    endpoint TEXT NOT NULL,
    api_key TEXT NOT NULL,
    api_format TEXT NOT NULL,
    config TEXT,
    models TEXT,
    enabled BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_relay_providers_api_format ON relay_providers (api_format);
CREATE INDEX IF NOT EXISTS idx_relay_providers_enabled ON relay_providers (enabled);

CREATE TABLE IF NOT EXISTS relay_models (
    id BIGSERIAL PRIMARY KEY,
    provider_id BIGINT NOT NULL REFERENCES relay_providers(id) ON DELETE CASCADE,
    model_id TEXT NOT NULL,
    display_name TEXT,
    description TEXT,
    category TEXT NOT NULL DEFAULT 'chat',
    capabilities TEXT,
    advanced_params TEXT,
    enabled BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_relay_model_provider_name ON relay_models (provider_id, model_id);
CREATE INDEX IF NOT EXISTS idx_relay_models_category ON relay_models (category);
CREATE INDEX IF NOT EXISTS idx_relay_models_enabled ON relay_models (enabled);

CREATE TABLE IF NOT EXISTS relay_request_logs (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    username TEXT NOT NULL,
    provider_id BIGINT,
    provider_name TEXT,
    api_type TEXT,
    model_id TEXT,
    category TEXT,
    operation TEXT NOT NULL,
    route TEXT NOT NULL,
    protocol TEXT NOT NULL,
    http_status BIGINT NOT NULL,
    upstream_status BIGINT,
    duration_ms BIGINT NOT NULL,
    request_bytes BIGINT NOT NULL,
    response_bytes BIGINT NOT NULL,
    error_type TEXT,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_relay_request_logs_user_id ON relay_request_logs (user_id);
CREATE INDEX IF NOT EXISTS idx_relay_request_logs_provider_id ON relay_request_logs (provider_id);
CREATE INDEX IF NOT EXISTS idx_relay_request_logs_api_type ON relay_request_logs (api_type);
CREATE INDEX IF NOT EXISTS idx_relay_request_logs_model_id ON relay_request_logs (model_id);
CREATE INDEX IF NOT EXISTS idx_relay_request_logs_operation ON relay_request_logs (operation);
CREATE INDEX IF NOT EXISTS idx_relay_request_logs_protocol ON relay_request_logs (protocol);
CREATE INDEX IF NOT EXISTS idx_relay_request_logs_http_status ON relay_request_logs (http_status);
CREATE INDEX IF NOT EXISTS idx_relay_request_logs_error_type ON relay_request_logs (error_type);
CREATE INDEX IF NOT EXISTS idx_relay_request_logs_created_at ON relay_request_logs (created_at);
