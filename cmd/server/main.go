package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/lynai/backend/internal/admin"
	"github.com/lynai/backend/internal/auth"
	"github.com/lynai/backend/internal/config"
	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/market"
	"github.com/lynai/backend/internal/relay"
	"github.com/lynai/backend/internal/server"
	"github.com/lynai/backend/internal/sync"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Database
	db, err := database.Connect(cfg.DBDSN)
	if err != nil {
		log.Fatalf("database: %v", err)
	}

	// Snowflake ID generator (machine ID 0 for single-instance; use env for multi-instance)
	snowflake := database.NewSnowflakeGenerator(0)

	// Admin seed.
	adminPhone := envOr("ADMIN_PHONE", "0000000000")
	adminName := envOr("ADMIN_DISPLAY_NAME", "管理员")
	adminPassword := envOr("ADMIN_PASSWORD", "")
	if adminPassword == "" {
		log.Fatalf("ADMIN_PASSWORD environment variable is required")
	}
	adminPasswordHash, err := auth.HashPassword(adminPassword)
	if err != nil {
		log.Fatalf("admin password: %v", err)
	}
	if err := database.EnsureAdminSeed(db, adminPhone, adminName, adminPasswordHash, snowflake); err != nil {
		log.Fatalf("admin seed: %v", err)
	}

	// Services
	jwtMgr := auth.NewJWTManager(cfg.JWTSecret)
	authSvc := auth.NewService(db, jwtMgr, snowflake)

	storage, err := market.NewStorage(cfg.StorageDir)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	marketSvc := market.NewService(db, storage)

	blobStorage, err := sync.NewBlobStorage(cfg.StorageDir)
	if err != nil {
		log.Fatalf("blob storage: %v", err)
	}
	syncSvc := sync.NewService(db, blobStorage)
	relaySvc := relay.NewService(db)
	if err := relay.NewLogService(db).DeleteExpired(time.Now()); err != nil {
		log.Printf("relay log cleanup: %v", err)
	}

	// Handlers
	authHandler := auth.NewHandler(authSvc)
	marketHandler := market.NewHandler(marketSvc)
	syncHandler := sync.NewHandler(syncSvc)
	relayHandler := relay.NewHandler(relaySvc)

	// Admin panel
	adminHandler, err := admin.NewHandler(db, authSvc, marketSvc, jwtMgr)
	if err != nil {
		log.Fatalf("admin templates: %v", err)
	}

	// Server
	r := server.Setup(authHandler, jwtMgr, marketHandler, syncHandler, relayHandler, adminHandler)

	addr := ":" + cfg.Port
	fmt.Printf("LynAI backend listening on %s\n", addr)
	if err := r.Run(addr); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
