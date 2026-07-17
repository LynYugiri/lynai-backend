ALTER TABLE sync_changes ADD COLUMN change_id VARCHAR(128);
ALTER TABLE sync_changes ADD COLUMN device_id VARCHAR(52);
ALTER TABLE sync_changes ADD COLUMN client_created_at TIMESTAMPTZ;

UPDATE sync_changes
SET change_id = 'legacy-' || id::text,
    client_created_at = created_at
WHERE change_id IS NULL;

ALTER TABLE sync_changes ALTER COLUMN change_id SET NOT NULL;
ALTER TABLE sync_changes ALTER COLUMN client_created_at SET NOT NULL;
CREATE UNIQUE INDEX idx_sync_changes_user_change ON sync_changes (user_id, change_id);
CREATE INDEX idx_sync_changes_device_id ON sync_changes (device_id);

CREATE TABLE sync_request_replays (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL,
    request_id VARCHAR(32) NOT NULL,
    operation VARCHAR(128) NOT NULL,
    body_hash BYTEA NOT NULL CHECK (octet_length(body_hash) = 32),
    response_status INTEGER NOT NULL,
    response_content_type VARCHAR(128) NOT NULL,
    response_body BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX idx_sync_request_replays_user_request ON sync_request_replays (user_id, request_id);
CREATE INDEX idx_sync_request_replays_expires_at ON sync_request_replays (expires_at);
