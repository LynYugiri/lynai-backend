-- This release intentionally starts the cloud sync domain empty. A projection
-- cannot be reconstructed safely from an arbitrary partially retained log.
TRUNCATE TABLE sync_request_replays, sync_changes, sync_blobs, sync_metadata RESTART IDENTITY;

ALTER TABLE sync_metadata
    ADD COLUMN generation BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN index_revision BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN min_available_seq BIGINT NOT NULL DEFAULT 0;

CREATE TABLE sync_records (
    user_id BIGINT NOT NULL,
    table_name TEXT NOT NULL,
    record_id TEXT NOT NULL,
    data JSONB NOT NULL,
    seq BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, table_name, record_id)
);
CREATE INDEX idx_sync_records_user_seq ON sync_records (user_id, seq);

CREATE TABLE sync_record_blobs (
    user_id BIGINT NOT NULL,
    table_name TEXT NOT NULL,
    record_id TEXT NOT NULL,
    sha256 VARCHAR(64) NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (user_id, table_name, record_id, sha256),
    FOREIGN KEY (user_id, table_name, record_id)
        REFERENCES sync_records (user_id, table_name, record_id)
        ON DELETE CASCADE
);
CREATE INDEX idx_sync_record_blobs_user_sha256 ON sync_record_blobs (user_id, sha256);
