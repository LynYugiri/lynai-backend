CREATE SEQUENCE IF NOT EXISTS relay_providers_id_seq;

WITH sequence_state AS (
    SELECT CASE WHEN is_called THEN last_value + 1 ELSE last_value END AS next_value
    FROM relay_providers_id_seq
), next_sequence_value AS (
    SELECT GREATEST(
        COALESCE((SELECT MAX(id) + 1 FROM relay_providers), 1),
        COALESCE((SELECT MAX(provider_id) + 1 FROM relay_request_logs), 1),
        sequence_state.next_value,
        1
    ) AS value
    FROM sequence_state
)
SELECT setval(
    'relay_providers_id_seq',
    value,
    false
)
FROM next_sequence_value;

ALTER TABLE relay_providers
    ALTER COLUMN id SET DEFAULT nextval('relay_providers_id_seq');

ALTER SEQUENCE relay_providers_id_seq OWNED BY relay_providers.id;
