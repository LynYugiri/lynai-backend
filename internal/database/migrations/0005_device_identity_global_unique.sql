DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM user_devices
        GROUP BY device_id
        HAVING COUNT(*) > 1
    ) OR EXISTS (
        SELECT 1
        FROM user_devices
        GROUP BY public_key
        HAVING COUNT(*) > 1
    ) THEN
        RAISE EXCEPTION 'duplicate device identities exist across accounts; migrate affected clients to account-scoped identities before applying migration 0005';
    END IF;
END
$$;

CREATE UNIQUE INDEX idx_user_devices_device_id_global
    ON user_devices (device_id);

CREATE UNIQUE INDEX idx_user_devices_public_key_global
    ON user_devices (public_key);
