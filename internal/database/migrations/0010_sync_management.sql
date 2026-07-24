ALTER TABLE sync_records
    ADD COLUMN category TEXT,
    ADD COLUMN object_id TEXT;

UPDATE sync_records SET
    category = CASE table_name
        WHEN 'conversations' THEN 'conversations'
        WHEN 'messages' THEN 'messages'
        WHEN 'message_attachments' THEN 'attachments'
        WHEN 'resources' THEN 'resources'
        WHEN 'note_folders' THEN 'notes'
        WHEN 'notes' THEN 'notes'
        WHEN 'note_pages' THEN 'notes'
        WHEN 'note_revisions' THEN 'notes'
        WHEN 'note_page_heads' THEN 'notes'
        WHEN 'note_page_tombstones' THEN 'notes'
        WHEN 'tasks' THEN 'tasks'
        WHEN 'task_lists' THEN 'tasks'
        WHEN 'task_list_entries' THEN 'tasks'
        WHEN 'calendar_events' THEN 'calendar'
        WHEN 'anniversaries' THEN 'calendar'
        WHEN 'roleplay_scenarios' THEN 'roleplay'
        WHEN 'roleplay_threads' THEN 'roleplay'
        WHEN 'recycle_bin' THEN 'recycle_bin'
        WHEN 'shared_settings' THEN 'settings'
        WHEN 'synced_model_configs' THEN 'models'
        WHEN 'plugin_files' THEN 'plugins'
        WHEN 'plugin_settings' THEN 'plugins'
        WHEN 'plugin_config' THEN 'plugins'
    END,
    object_id = COALESCE(
        CASE table_name
            WHEN 'messages' THEN data->>'conversationId'
            WHEN 'message_attachments' THEN data->>'messageId'
            WHEN 'note_pages' THEN data->>'noteId'
            WHEN 'note_revisions' THEN data->>'noteId'
            WHEN 'note_page_heads' THEN COALESCE(data->>'noteId', data->>'pageId')
            WHEN 'note_page_tombstones' THEN COALESCE(data->>'noteId', data->>'pageId')
            WHEN 'task_list_entries' THEN COALESCE(data->>'listId', data->>'taskListId')
            WHEN 'roleplay_threads' THEN data->>'scenarioId'
            WHEN 'plugin_files' THEN data->>'pluginId'
            WHEN 'plugin_settings' THEN data->>'pluginId'
            WHEN 'plugin_config' THEN data->>'pluginId'
        END,
        record_id
    );

ALTER TABLE sync_records
    ALTER COLUMN category SET NOT NULL,
    ALTER COLUMN object_id SET NOT NULL;

CREATE INDEX idx_sync_records_user_category_object
    ON sync_records (user_id, category, object_id, table_name, record_id);

ALTER TABLE sync_request_replays
    ADD COLUMN generation BIGINT NOT NULL DEFAULT 1;

CREATE TABLE sync_management_operations (
    id VARCHAR(32) PRIMARY KEY,
    user_id BIGINT NOT NULL,
    kind VARCHAR(16) NOT NULL CHECK (kind IN ('selective', 'full')),
    selector_type VARCHAR(16) NOT NULL CHECK (selector_type IN ('object', 'category', 'all')),
    category TEXT,
    object_id TEXT,
    generation BIGINT NOT NULL,
    index_revision BIGINT NOT NULL,
    released_blob_candidates BIGINT NOT NULL DEFAULT 0,
    deleted_record_count BIGINT NOT NULL DEFAULT 0,
    deleted_change_count BIGINT NOT NULL DEFAULT 0,
    status VARCHAR(16) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'acked')),
    created_by_device_id VARCHAR(52) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    acked_by_device_id VARCHAR(52),
    acked_at TIMESTAMPTZ,
    CHECK ((selector_type = 'object' AND category IS NOT NULL AND object_id IS NOT NULL)
        OR (selector_type = 'category' AND category IS NOT NULL AND object_id IS NULL)
        OR (selector_type = 'all' AND category IS NULL AND object_id IS NULL))
);
CREATE INDEX idx_sync_management_operations_user_pending
    ON sync_management_operations (user_id, status, created_at, id);
