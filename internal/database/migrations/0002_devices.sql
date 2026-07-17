CREATE TABLE IF NOT EXISTS user_devices (
    user_id BIGINT NOT NULL,
    device_id VARCHAR(52) NOT NULL CHECK (device_id ~ '^[a-z2-7]{52}$'),
    session_id VARCHAR(64) NOT NULL,
    name VARCHAR(64) NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 64),
    platform VARCHAR(32) NOT NULL CHECK (platform ~ '^[a-z0-9._-]{1,32}$'),
    protocol SMALLINT NOT NULL CHECK (protocol = 1),
    public_key BYTEA NOT NULL CHECK (octet_length(public_key) = 32),
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    PRIMARY KEY (user_id, device_id),
    UNIQUE (user_id, public_key)
);
CREATE INDEX IF NOT EXISTS idx_user_devices_session_id ON user_devices (user_id, session_id);
CREATE INDEX IF NOT EXISTS idx_user_devices_revoked_at ON user_devices (user_id, revoked_at);

CREATE TABLE IF NOT EXISTS device_challenges (
    id VARCHAR(32) PRIMARY KEY CHECK (id ~ '^[A-Za-z0-9_-]{32}$'),
    user_id BIGINT NOT NULL,
    session_id VARCHAR(64) NOT NULL,
    device_id VARCHAR(52) NOT NULL CHECK (device_id ~ '^[a-z2-7]{52}$'),
    public_key BYTEA NOT NULL CHECK (octet_length(public_key) = 32),
    name VARCHAR(64) NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 64),
    platform VARCHAR(32) NOT NULL CHECK (platform ~ '^[a-z0-9._-]{1,32}$'),
    protocol SMALLINT NOT NULL CHECK (protocol = 1),
    challenge_hash BYTEA NOT NULL CHECK (octet_length(challenge_hash) = 32),
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_device_challenges_user_id ON device_challenges (user_id);
CREATE INDEX IF NOT EXISTS idx_device_challenges_session_id ON device_challenges (session_id);
CREATE INDEX IF NOT EXISTS idx_device_challenges_expires_at ON device_challenges (expires_at);
CREATE INDEX IF NOT EXISTS idx_device_challenges_consumed_at ON device_challenges (consumed_at);
