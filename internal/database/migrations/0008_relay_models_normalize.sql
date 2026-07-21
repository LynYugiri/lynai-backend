DO $$
DECLARE
    provider RECORD;
    parsed_models JSONB;
    element JSONB;
BEGIN
    FOR provider IN
        SELECT id, models
        FROM relay_providers
        WHERE NULLIF(BTRIM(models), '') IS NOT NULL
    LOOP
        BEGIN
            parsed_models := provider.models::jsonb;
        EXCEPTION WHEN OTHERS THEN
            RAISE EXCEPTION 'relay_providers.models for provider % must be valid JSON', provider.id;
        END;
        IF jsonb_typeof(parsed_models) <> 'array' THEN
            RAISE EXCEPTION 'relay_providers.models for provider % must be a JSON array', provider.id;
        END IF;
        FOR element IN SELECT value FROM jsonb_array_elements(parsed_models) AS item(value)
        LOOP
            IF jsonb_typeof(element) <> 'string' THEN
                RAISE EXCEPTION 'relay_providers.models for provider % must contain only strings', provider.id;
            END IF;
        END LOOP;
    END LOOP;
END $$;

INSERT INTO relay_models (
    provider_id,
    model_id,
    display_name,
    description,
    category,
    capabilities,
    advanced_params,
    enabled,
    created_at,
    updated_at
)
SELECT DISTINCT
    provider.id,
    BTRIM(model.model_id),
    '',
    '',
    'chat',
    '{}',
    '{}',
    TRUE,
    COALESCE(provider.created_at, NOW()),
    COALESCE(provider.updated_at, NOW())
FROM relay_providers AS provider
CROSS JOIN LATERAL jsonb_array_elements_text(
    CASE
        WHEN NULLIF(BTRIM(provider.models), '') IS NULL THEN '[]'::jsonb
        ELSE provider.models::jsonb
    END
) AS model(model_id)
WHERE BTRIM(model.model_id) <> ''
ON CONFLICT (provider_id, model_id) DO NOTHING;

DO $$
DECLARE
    missing_provider_id BIGINT;
BEGIN
    SELECT provider.id
    INTO missing_provider_id
    FROM relay_providers AS provider
    CROSS JOIN LATERAL jsonb_array_elements_text(
        CASE
            WHEN NULLIF(BTRIM(provider.models), '') IS NULL THEN '[]'::jsonb
            ELSE provider.models::jsonb
        END
    ) AS model(model_id)
    WHERE BTRIM(model.model_id) <> ''
      AND NOT EXISTS (
          SELECT 1
          FROM relay_models
          WHERE relay_models.provider_id = provider.id
            AND relay_models.model_id = BTRIM(model.model_id)
      )
    LIMIT 1;

    IF missing_provider_id IS NOT NULL THEN
        RAISE EXCEPTION 'relay model expansion verification failed for provider %', missing_provider_id;
    END IF;
END $$;

ALTER TABLE relay_providers DROP COLUMN models;
