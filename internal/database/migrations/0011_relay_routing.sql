DO $$
DECLARE
    conflicting_model_id TEXT;
BEGIN
    SELECT model_id
    INTO conflicting_model_id
    FROM relay_models
    GROUP BY model_id
    HAVING COUNT(DISTINCT COALESCE(NULLIF(BTRIM(category), ''), 'chat')) > 1
    ORDER BY model_id
    LIMIT 1;

    IF conflicting_model_id IS NOT NULL THEN
        RAISE EXCEPTION 'relay_models contains conflicting normalized categories for model_id %', conflicting_model_id;
    END IF;
END $$;

ALTER TABLE relay_models RENAME TO relay_models_legacy;
ALTER INDEX relay_models_pkey RENAME TO relay_models_legacy_pkey;
ALTER INDEX idx_relay_model_provider_name RENAME TO idx_relay_models_legacy_provider_name;
ALTER INDEX idx_relay_models_category RENAME TO idx_relay_models_legacy_category;
ALTER INDEX idx_relay_models_enabled RENAME TO idx_relay_models_legacy_enabled;
ALTER SEQUENCE relay_models_id_seq RENAME TO relay_models_legacy_id_seq;

CREATE TABLE relay_provider_credentials (
    id BIGSERIAL PRIMARY KEY,
    provider_id BIGINT NOT NULL REFERENCES relay_providers(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    api_key TEXT NOT NULL,
    priority INTEGER NOT NULL DEFAULT 0,
    config TEXT NOT NULL DEFAULT '{}',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);
CREATE INDEX idx_relay_provider_credentials_provider_id ON relay_provider_credentials (provider_id);
CREATE INDEX idx_relay_provider_credentials_priority ON relay_provider_credentials (priority);
CREATE INDEX idx_relay_provider_credentials_enabled ON relay_provider_credentials (enabled);

INSERT INTO relay_provider_credentials (
    provider_id, name, api_key, priority, config, enabled, created_at, updated_at
)
SELECT id, 'Default', api_key, 0, '{}', TRUE, created_at, updated_at
FROM relay_providers
ORDER BY id;

CREATE TABLE relay_models (
    id BIGSERIAL PRIMARY KEY,
    model_id TEXT NOT NULL UNIQUE,
    display_name TEXT,
    description TEXT,
    category TEXT NOT NULL DEFAULT 'chat',
    capabilities TEXT,
    advanced_params TEXT,
    enabled BOOLEAN DEFAULT TRUE,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);
CREATE INDEX idx_relay_models_category ON relay_models (category);
CREATE INDEX idx_relay_models_enabled ON relay_models (enabled);

INSERT INTO relay_models (
    id, model_id, display_name, description, category, capabilities,
    advanced_params, enabled, created_at, updated_at
)
SELECT DISTINCT ON (model_id)
    id,
    model_id,
    display_name,
    description,
    COALESCE(NULLIF(BTRIM(category), ''), 'chat'),
    capabilities,
    advanced_params,
    BOOL_OR(enabled) OVER (PARTITION BY model_id),
    created_at,
    updated_at
FROM relay_models_legacy
ORDER BY model_id, id;

SELECT setval(
    pg_get_serial_sequence('relay_models', 'id'),
    COALESCE((SELECT MAX(id) FROM relay_models), 1),
    EXISTS (SELECT 1 FROM relay_models)
);

CREATE TABLE relay_model_bindings (
    id BIGSERIAL PRIMARY KEY,
    relay_model_id BIGINT NOT NULL REFERENCES relay_models(id) ON DELETE CASCADE,
    provider_id BIGINT NOT NULL REFERENCES relay_providers(id) ON DELETE CASCADE,
    upstream_model TEXT NOT NULL,
    weight INTEGER NOT NULL DEFAULT 1 CHECK (weight >= 1),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    UNIQUE (relay_model_id, provider_id)
);
CREATE INDEX idx_relay_model_bindings_relay_model_id ON relay_model_bindings (relay_model_id);
CREATE INDEX idx_relay_model_bindings_provider_id ON relay_model_bindings (provider_id);
CREATE INDEX idx_relay_model_bindings_enabled ON relay_model_bindings (enabled);

INSERT INTO relay_model_bindings (
    relay_model_id, provider_id, upstream_model, weight, enabled, created_at, updated_at
)
SELECT
    model.id,
    legacy.provider_id,
    legacy.model_id,
    1,
    legacy.enabled,
    legacy.created_at,
    legacy.updated_at
FROM relay_models_legacy AS legacy
JOIN relay_models AS model ON model.model_id = legacy.model_id
ORDER BY legacy.id;

ALTER TABLE relay_speech_sessions
    ADD COLUMN binding_id BIGINT,
    ADD COLUMN credential_id BIGINT,
    ADD COLUMN upstream_model TEXT,
    ADD COLUMN endpoint TEXT,
    ADD COLUMN api_format TEXT,
    ADD COLUMN config_snapshot TEXT;

UPDATE relay_speech_sessions AS session
SET binding_id = binding.id,
    credential_id = credential.id,
    upstream_model = binding.upstream_model,
    endpoint = provider.endpoint,
    api_format = provider.api_format,
    config_snapshot = COALESCE(provider.config, '{}')
FROM relay_providers AS provider
JOIN relay_provider_credentials AS credential ON credential.provider_id = provider.id
JOIN relay_model_bindings AS binding ON binding.provider_id = provider.id
JOIN relay_models AS model ON model.id = binding.relay_model_id
WHERE provider.id = session.provider_id
  AND credential.name = 'Default'
  AND model.model_id = session.model_id;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM relay_speech_sessions
        WHERE binding_id IS NULL OR credential_id IS NULL OR upstream_model IS NULL
           OR endpoint IS NULL OR api_format IS NULL OR config_snapshot IS NULL
    ) THEN
        RAISE EXCEPTION 'relay speech session routing migration failed';
    END IF;
END $$;

ALTER TABLE relay_speech_sessions
    ALTER COLUMN binding_id SET NOT NULL,
    ALTER COLUMN credential_id SET NOT NULL,
    ALTER COLUMN upstream_model SET NOT NULL,
    ALTER COLUMN endpoint SET NOT NULL,
    ALTER COLUMN api_format SET NOT NULL,
    ALTER COLUMN config_snapshot SET NOT NULL,
    DROP CONSTRAINT relay_speech_sessions_provider_id_fkey,
    ADD CONSTRAINT relay_speech_sessions_provider_id_fkey
        FOREIGN KEY (provider_id) REFERENCES relay_providers(id) ON DELETE RESTRICT,
    ADD CONSTRAINT relay_speech_sessions_binding_id_fkey
        FOREIGN KEY (binding_id) REFERENCES relay_model_bindings(id) ON DELETE RESTRICT,
    ADD CONSTRAINT relay_speech_sessions_credential_id_fkey
        FOREIGN KEY (credential_id) REFERENCES relay_provider_credentials(id) ON DELETE RESTRICT;
CREATE INDEX idx_relay_speech_sessions_binding_id ON relay_speech_sessions (binding_id);
CREATE INDEX idx_relay_speech_sessions_credential_id ON relay_speech_sessions (credential_id);

ALTER TABLE relay_request_logs
    ADD COLUMN binding_id BIGINT,
    ADD COLUMN credential_id BIGINT,
    ADD COLUMN credential_name TEXT,
    ADD COLUMN attempt_count INTEGER NOT NULL DEFAULT 1,
    ADD COLUMN failover_count INTEGER NOT NULL DEFAULT 0,
    ADD CONSTRAINT relay_request_logs_attempt_count_check CHECK (attempt_count >= 1),
    ADD CONSTRAINT relay_request_logs_failover_count_check CHECK (failover_count >= 0 AND failover_count < attempt_count);
CREATE INDEX idx_relay_request_logs_binding_id ON relay_request_logs (binding_id);
CREATE INDEX idx_relay_request_logs_credential_id ON relay_request_logs (credential_id);

ALTER TABLE relay_providers DROP COLUMN api_key;
DROP TABLE relay_models_legacy;
