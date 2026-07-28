package config

import (
	"strings"
	"testing"
	"time"
)

const validTestJWTSecret = "test-jwt-secret-with-at-least-32-bytes"

func TestLoadDefaultsAndMachineID(t *testing.T) {
	t.Setenv("DB_DSN", "test-dsn")
	t.Setenv("JWT_SECRET", validTestJWTSecret)
	t.Setenv("PORT", "")
	t.Setenv("STORAGE_DIR", "")
	t.Setenv("MACHINE_ID", "0")
	t.Setenv("RELAY_PRIVATE_HOST_ALLOWLIST", " localhost:11434, ollama.internal ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Port != "8080" || cfg.StorageDir != "./storage" || cfg.MachineID != 0 || cfg.SnowflakeRollbackTimeout != 5*time.Second {
		t.Fatalf("Load() defaults = port %q, storage %q, machine %d", cfg.Port, cfg.StorageDir, cfg.MachineID)
	}
	if cfg.SyncClockSkew.String() != "5m0s" || cfg.SyncReplayRetention.String() != "24h0m0s" {
		t.Fatalf("sync defaults = skew %s, retention %s", cfg.SyncClockSkew, cfg.SyncReplayRetention)
	}
	if len(cfg.RelayPrivateHostAllowlist) != 2 || cfg.RelayPrivateHostAllowlist[0] != "localhost:11434" || cfg.RelayPrivateHostAllowlist[1] != "ollama.internal" {
		t.Fatalf("relay allowlist = %#v", cfg.RelayPrivateHostAllowlist)
	}
	if cfg.AdminSessionTTL != 30*24*time.Hour || cfg.RelaySpeechSessionTTL != 2*time.Hour || cfg.RelaySpeechPerUser != 5 || cfg.RelaySpeechGlobal != 500 {
		t.Fatalf("session defaults = admin %s, speech %s, per-user %d, global %d", cfg.AdminSessionTTL, cfg.RelaySpeechSessionTTL, cfg.RelaySpeechPerUser, cfg.RelaySpeechGlobal)
	}
	if cfg.SearchTavilyOrigin != "https://api.tavily.com" || cfg.SearchTimeout != 15*time.Second {
		t.Fatalf("search defaults = tavily origin %q, timeout %s", cfg.SearchTavilyOrigin, cfg.SearchTimeout)
	}

	for _, machineID := range []string{"0", "1023"} {
		t.Run(machineID, func(t *testing.T) {
			t.Setenv("MACHINE_ID", machineID)
			if _, err := Load(); err != nil {
				t.Fatalf("Load() error = %v", err)
			}
		})
	}
}

func TestLoadValidatesSyncTimingConfiguration(t *testing.T) {
	t.Setenv("DB_DSN", "test-dsn")
	t.Setenv("JWT_SECRET", validTestJWTSecret)
	t.Setenv("MACHINE_ID", "0")
	for _, tc := range []struct {
		name, key, value string
	}{
		{name: "clock skew", key: "SYNC_CLOCK_SKEW", value: "0s"},
		{name: "retention", key: "SYNC_REPLAY_RETENTION", value: "1m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("invalid sync configuration was accepted")
			}
		})
	}
}

func TestLoadRejectsInvalidMachineID(t *testing.T) {
	t.Setenv("DB_DSN", "test-dsn")
	t.Setenv("JWT_SECRET", validTestJWTSecret)

	for _, machineID := range []string{"", "-1", "1024", "invalid"} {
		t.Run(machineID, func(t *testing.T) {
			t.Setenv("MACHINE_ID", machineID)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), "MACHINE_ID") {
				t.Fatalf("Load() error = %v, want MACHINE_ID validation error", err)
			}
		})
	}
}

func TestLoadValidatesSessionAndRelayLimits(t *testing.T) {
	t.Setenv("DB_DSN", "test-dsn")
	t.Setenv("JWT_SECRET", validTestJWTSecret)
	t.Setenv("MACHINE_ID", "0")
	for _, tc := range []struct {
		key, value string
	}{
		{key: "ADMIN_SESSION_TTL", value: "0s"},
		{key: "SESSION_CLEANUP_INTERVAL", value: "invalid"},
		{key: "RELAY_SPEECH_PER_USER_CAPACITY", value: "0"},
		{key: "RELAY_NON_STREAM_TIMEOUT", value: "-1s"},
		{key: "SNOWFLAKE_ROLLBACK_TIMEOUT", value: "0s"},
		{key: "SEARCH_TIMEOUT", value: "0s"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("Load() error = %v, want %s validation", err, tc.key)
			}
		})
	}

	t.Setenv("RELAY_SPEECH_PER_USER_CAPACITY", "10")
	t.Setenv("RELAY_SPEECH_GLOBAL_CAPACITY", "5")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "RELAY_SPEECH_GLOBAL_CAPACITY") {
		t.Fatalf("Load() error = %v, want global capacity validation", err)
	}
}

func TestLoadSearchConfiguration(t *testing.T) {
	t.Setenv("DB_DSN", "test-dsn")
	t.Setenv("JWT_SECRET", validTestJWTSecret)
	t.Setenv("MACHINE_ID", "0")
	t.Setenv("TAVILY_API_KEY", " tavily-secret ")
	t.Setenv("TAVILY_ORIGIN", "https://tavily.example")
	t.Setenv("SEARXNG_ORIGIN", "https://search.example")
	t.Setenv("SEARCH_PRIVATE_HOST_ALLOWLIST", "localhost:8888, search.internal")
	t.Setenv("SEARCH_TIMEOUT", "20s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.SearchTavilyAPIKey != "tavily-secret" || cfg.SearchTavilyOrigin != "https://tavily.example" || cfg.SearchSearXNGOrigin != "https://search.example" {
		t.Fatalf("search configuration = %#v", cfg)
	}
	if cfg.SearchTimeout != 20*time.Second || len(cfg.SearchPrivateAllowlist) != 2 {
		t.Fatalf("search limits = timeout %s, allowlist %#v", cfg.SearchTimeout, cfg.SearchPrivateAllowlist)
	}

	t.Setenv("SEARCH_TIMEOUT", "61s")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SEARCH_TIMEOUT") {
		t.Fatalf("Load() error = %v, want SEARCH_TIMEOUT validation", err)
	}
}

func TestLoadRequiresDatabaseAndJWTConfiguration(t *testing.T) {
	t.Setenv("MACHINE_ID", "0")
	t.Setenv("DB_DSN", "")
	t.Setenv("JWT_SECRET", validTestJWTSecret)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "DB_DSN") {
		t.Fatalf("Load() error = %v, want DB_DSN error", err)
	}

	t.Setenv("DB_DSN", "test-dsn")
	t.Setenv("JWT_SECRET", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Fatalf("Load() error = %v, want JWT_SECRET error", err)
	}
}

func TestLoadRejectsWeakJWTSecrets(t *testing.T) {
	t.Setenv("DB_DSN", "test-dsn")
	t.Setenv("MACHINE_ID", "0")
	for _, secret := range []string{
		"short-secret",
		"REPLACE_WITH_A_RANDOM_SECRET_THAT_IS_LONG_ENOUGH",
		"your-example-secret-that-is-long-enough-for-hmac",
	} {
		t.Run(secret, func(t *testing.T) {
			t.Setenv("JWT_SECRET", secret)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
				t.Fatalf("Load() error = %v, want JWT_SECRET validation error", err)
			}
		})
	}
}
