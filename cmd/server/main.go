package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lynai/backend/internal/admin"
	"github.com/lynai/backend/internal/auth"
	"github.com/lynai/backend/internal/community"
	"github.com/lynai/backend/internal/config"
	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/device"
	"github.com/lynai/backend/internal/market"
	"github.com/lynai/backend/internal/relay"
	"github.com/lynai/backend/internal/server"
	"github.com/lynai/backend/internal/sync"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Printf("server: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		if len(args) != 1 || args[0] != "migrate" {
			return fmt.Errorf("usage: lynai-backend [migrate]")
		}
		dsn := os.Getenv("DB_DSN")
		if dsn == "" {
			return fmt.Errorf("DB_DSN environment variable is required")
		}
		db, err := database.Connect(dsn)
		if err != nil {
			return fmt.Errorf("database: %w", err)
		}
		sqlDB, err := db.DB()
		if err != nil {
			return fmt.Errorf("database handle: %w", err)
		}
		defer sqlDB.Close()
		if err := database.Migrate(context.Background(), db); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
		log.Printf("database migrations applied")
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Database
	db, err := database.Connect(cfg.DBDSN)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("database handle: %w", err)
	}
	defer sqlDB.Close()
	if err := database.ValidateSchema(context.Background(), db); err != nil {
		return fmt.Errorf("database schema: %w", err)
	}

	// Snowflake machine ID is loaded from MACHINE_ID for multi-instance deployments.
	snowflake := database.NewSnowflakeGenerator(cfg.MachineID, cfg.SnowflakeRollbackTimeout)

	// Admin seed.
	adminPhone := envOr("ADMIN_PHONE", "0000000000")
	adminName := envOr("ADMIN_DISPLAY_NAME", "管理员")
	adminPassword := envOr("ADMIN_PASSWORD", "")
	if adminPassword == "" {
		return fmt.Errorf("ADMIN_PASSWORD environment variable is required")
	}
	adminPasswordHash, err := auth.HashPassword(adminPassword)
	if err != nil {
		return fmt.Errorf("admin password: %w", err)
	}
	if err := database.EnsureAdminSeed(context.Background(), db, adminPhone, adminName, adminPasswordHash, snowflake); err != nil {
		return fmt.Errorf("admin seed: %w", err)
	}

	// Services
	jwtMgr := auth.NewJWTManager(cfg.JWTSecret)
	authSvc := auth.NewService(db, jwtMgr, snowflake)
	deviceSvc := device.NewService(db)

	storage, err := market.NewStorage(cfg.StorageDir)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	marketSvc := market.NewService(db, storage)
	communityStorage, err := community.NewStorage(cfg.StorageDir)
	if err != nil {
		return fmt.Errorf("community storage: %w", err)
	}
	communitySvc := community.NewService(db, communityStorage, snowflake)
	if err := communitySvc.DeleteOrphanMedia(context.Background(), time.Now().Add(-24*time.Hour)); err != nil {
		log.Printf("community orphan media cleanup: %v", err)
	}

	blobStorage, err := sync.NewBlobStorage(cfg.StorageDir)
	if err != nil {
		return fmt.Errorf("blob storage: %w", err)
	}
	syncSvc := sync.NewServiceWithReplayRetention(db, blobStorage, cfg.SyncReplayRetention)
	if err := syncSvc.DeleteExpired(time.Now()); err != nil {
		log.Printf("sync replay/challenge cleanup: %v", err)
	}
	if result, err := syncSvc.ReconcileBlobs(time.Now(), time.Hour); err != nil {
		log.Printf("sync blob reconciliation: %v", err)
	} else if result.StaleTemps+result.OrphanMetadata+result.OrphanFiles+result.CorrectedSizes > 0 {
		log.Printf("sync blob reconciliation: %+v", result)
	}
	endpointPolicy, err := relay.NewEndpointPolicy(cfg.RelayPrivateHostAllowlist)
	if err != nil {
		return fmt.Errorf("relay endpoint policy: %w", err)
	}
	relaySvc := relay.NewServiceWithEndpointPolicy(db, endpointPolicy)
	if err := relay.NewLogService(db).DeleteExpired(time.Now()); err != nil {
		log.Printf("relay log cleanup: %v", err)
	}

	// Handlers
	authHandler := auth.NewHandler(authSvc)
	deviceHandler := device.NewHandler(deviceSvc)
	marketHandler := market.NewHandler(marketSvc)
	communityHandler := community.NewHandler(communitySvc)
	syncHandler := sync.NewHandlerWithClockSkew(syncSvc, cfg.SyncClockSkew)
	relayHandler := relay.NewHandlerWithConfig(relaySvc, cfg.RelaySpeechSessionTTL, cfg.RelaySpeechPerUser, cfg.RelaySpeechGlobal, cfg.RelayNonStreamTimeout, cfg.RelayStreamIdleTimeout, cfg.RelayStreamMaxDuration)
	defer relayHandler.Close()

	// Admin panel
	adminHandler, err := admin.NewHandlerWithConfig(db, authSvc, marketSvc, endpointPolicy, cfg.AdminSessionTTL)
	if err != nil {
		return fmt.Errorf("admin templates: %w", err)
	}
	defer adminHandler.Close()

	// Server
	r := server.Setup(authHandler, jwtMgr, deviceHandler, marketHandler, communityHandler, syncHandler, relayHandler, adminHandler)

	addr := ":" + cfg.Port
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go syncSvc.RunCleanup(ctx, time.Hour, time.Hour, func(err error) {
		log.Printf("sync cleanup: %v", err)
	})
	go runPeriodicCleanup(ctx, time.Hour, func(now time.Time) error {
		return communitySvc.DeleteOrphanMedia(ctx, now.Add(-24*time.Hour))
	}, func(err error) {
		log.Printf("community orphan media cleanup: %v", err)
	})
	go runSessionCleanup(ctx, cfg.SessionCleanupInterval, func(now time.Time) error {
		if err := authSvc.DeleteExpiredSessions(now); err != nil {
			return fmt.Errorf("user sessions: %w", err)
		}
		if err := adminHandler.DeleteExpiredSessions(now); err != nil {
			return fmt.Errorf("admin sessions: %w", err)
		}
		if err := relayHandler.DeleteExpiredSessions(now); err != nil {
			return fmt.Errorf("speech sessions: %w", err)
		}
		return nil
	})

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- httpServer.ListenAndServe()
	}()
	log.Printf("LynAI backend listening on %s", addr)

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serverErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func runSessionCleanup(ctx context.Context, interval time.Duration, cleanup func(time.Time) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	if err := cleanup(time.Now()); err != nil {
		log.Printf("session cleanup: %v", err)
	}
	for {
		select {
		case now := <-ticker.C:
			if err := cleanup(now); err != nil {
				log.Printf("session cleanup: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func runPeriodicCleanup(
	ctx context.Context,
	interval time.Duration,
	cleanup func(time.Time) error,
	onError func(error),
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			if err := cleanup(now); err != nil {
				onError(err)
			}
		case <-ctx.Done():
			return
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
