CREATE TABLE admin_sessions (
    token_hash BYTEA PRIMARY KEY CHECK (octet_length(token_hash) = 32),
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_admin_sessions_user_id ON admin_sessions (user_id);
CREATE INDEX idx_admin_sessions_expires_at ON admin_sessions (expires_at);

CREATE TABLE relay_speech_sessions (
    id VARCHAR(32) PRIMARY KEY CHECK (id ~ '^[A-Za-z0-9_-]{32}$'),
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider_id BIGINT NOT NULL REFERENCES relay_providers(id) ON DELETE CASCADE,
    model_id TEXT NOT NULL,
    app_id TEXT NOT NULL,
    upstream_audio_id TEXT NOT NULL DEFAULT '',
    task_id TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_relay_speech_sessions_expires_at ON relay_speech_sessions (expires_at);
CREATE INDEX idx_relay_speech_user_expires ON relay_speech_sessions (user_id, expires_at);
