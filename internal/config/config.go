package config

import (
	"fmt"
	"os"
)

// Config holds all application configuration loaded from environment.
type Config struct {
	Port       string
	DBDSN      string
	JWTSecret  string
	StorageDir string
}

// Load reads configuration from environment variables with sensible defaults.
func Load() (*Config, error) {
	cfg := &Config{
		Port:       envOr("PORT", "8080"),
		DBDSN:      envOr("DB_DSN", ""),
		JWTSecret:  envOr("JWT_SECRET", ""),
		StorageDir: envOr("STORAGE_DIR", "./storage"),
	}

	if cfg.DBDSN == "" {
		return nil, fmt.Errorf("DB_DSN environment variable is required")
	}
	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("JWT_SECRET environment variable is required")
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
