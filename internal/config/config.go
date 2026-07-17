package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration loaded from environment.
type Config struct {
	Port                      string
	DBDSN                     string
	JWTSecret                 string
	StorageDir                string
	MachineID                 int64
	SnowflakeRollbackTimeout  time.Duration
	RelayPrivateHostAllowlist []string
	SyncClockSkew             time.Duration
	SyncReplayRetention       time.Duration
	AdminSessionTTL           time.Duration
	SessionCleanupInterval    time.Duration
	RelaySpeechSessionTTL     time.Duration
	RelaySpeechPerUser        int
	RelaySpeechGlobal         int
	RelayNonStreamTimeout     time.Duration
	RelayStreamIdleTimeout    time.Duration
	RelayStreamMaxDuration    time.Duration
}

// Load reads configuration from environment variables with sensible defaults.
func Load() (*Config, error) {
	machineIDRaw := os.Getenv("MACHINE_ID")
	if machineIDRaw == "" {
		return nil, fmt.Errorf("MACHINE_ID environment variable is required")
	}
	machineID, err := strconv.ParseInt(machineIDRaw, 10, 64)
	if err != nil || machineID < 0 || machineID > 1023 {
		return nil, fmt.Errorf("MACHINE_ID must be an integer between 0 and 1023")
	}
	rollbackTimeout, err := positiveDuration("SNOWFLAKE_ROLLBACK_TIMEOUT", "5s")
	if err != nil {
		return nil, err
	}
	clockSkew, err := time.ParseDuration(envOr("SYNC_CLOCK_SKEW", "5m"))
	if err != nil || clockSkew <= 0 || clockSkew > time.Hour {
		return nil, fmt.Errorf("SYNC_CLOCK_SKEW must be a duration greater than zero and at most 1h")
	}
	replayRetention, err := time.ParseDuration(envOr("SYNC_REPLAY_RETENTION", "24h"))
	if err != nil || replayRetention < clockSkew || replayRetention > 30*24*time.Hour {
		return nil, fmt.Errorf("SYNC_REPLAY_RETENTION must be a duration between SYNC_CLOCK_SKEW and 720h")
	}
	adminSessionTTL, err := positiveDuration("ADMIN_SESSION_TTL", "720h")
	if err != nil {
		return nil, err
	}
	cleanupInterval, err := positiveDuration("SESSION_CLEANUP_INTERVAL", "1h")
	if err != nil {
		return nil, err
	}
	speechTTL, err := positiveDuration("RELAY_SPEECH_SESSION_TTL", "2h")
	if err != nil {
		return nil, err
	}
	nonStreamTimeout, err := positiveDuration("RELAY_NON_STREAM_TIMEOUT", "2m")
	if err != nil {
		return nil, err
	}
	streamIdleTimeout, err := positiveDuration("RELAY_STREAM_IDLE_TIMEOUT", "45s")
	if err != nil {
		return nil, err
	}
	streamMaxDuration, err := positiveDuration("RELAY_STREAM_MAX_DURATION", "30m")
	if err != nil {
		return nil, err
	}
	perUser, err := positiveInt("RELAY_SPEECH_PER_USER_CAPACITY", "5")
	if err != nil {
		return nil, err
	}
	global, err := positiveInt("RELAY_SPEECH_GLOBAL_CAPACITY", "500")
	if err != nil {
		return nil, err
	}
	if global < perUser {
		return nil, fmt.Errorf("RELAY_SPEECH_GLOBAL_CAPACITY must be at least RELAY_SPEECH_PER_USER_CAPACITY")
	}

	cfg := &Config{
		Port:                      envOr("PORT", "8080"),
		DBDSN:                     envOr("DB_DSN", ""),
		JWTSecret:                 envOr("JWT_SECRET", ""),
		StorageDir:                envOr("STORAGE_DIR", "./storage"),
		MachineID:                 machineID,
		SnowflakeRollbackTimeout:  rollbackTimeout,
		RelayPrivateHostAllowlist: splitList(os.Getenv("RELAY_PRIVATE_HOST_ALLOWLIST")),
		SyncClockSkew:             clockSkew,
		SyncReplayRetention:       replayRetention,
		AdminSessionTTL:           adminSessionTTL,
		SessionCleanupInterval:    cleanupInterval,
		RelaySpeechSessionTTL:     speechTTL,
		RelaySpeechPerUser:        perUser,
		RelaySpeechGlobal:         global,
		RelayNonStreamTimeout:     nonStreamTimeout,
		RelayStreamIdleTimeout:    streamIdleTimeout,
		RelayStreamMaxDuration:    streamMaxDuration,
	}

	if cfg.DBDSN == "" {
		return nil, fmt.Errorf("DB_DSN environment variable is required")
	}
	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("JWT_SECRET environment variable is required")
	}

	return cfg, nil
}

func positiveDuration(key, fallback string) (time.Duration, error) {
	value, err := time.ParseDuration(envOr(key, fallback))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a duration greater than zero", key)
	}
	return value, nil
}

func positiveInt(key, fallback string) (int, error) {
	value, err := strconv.Atoi(envOr(key, fallback))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be an integer greater than zero", key)
	}
	return value, nil
}

func splitList(raw string) []string {
	var values []string
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
