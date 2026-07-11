package testutil

import (
	"os"

	"github.com/lynai/backend/internal/admin"
	"github.com/lynai/backend/internal/auth"
	"github.com/lynai/backend/internal/database"
	"github.com/lynai/backend/internal/market"
	"github.com/lynai/backend/internal/relay"
	"github.com/lynai/backend/internal/server"
	"github.com/lynai/backend/internal/sync"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// SetupTest creates an in-memory SQLite database with all tables migrated,
// seeds an admin user, and returns a fully wired test server.
//
// The admin phone is returned for use in tests that need admin access.
func SetupTest() (adminPhone, adminPassword string, ts *TestServer, cleanup func()) {
	return setupTest(false)
}

// SetupTestWithAdminPanel creates a test server with HTML admin routes enabled.
func SetupTestWithAdminPanel() (adminPhone, adminPassword string, ts *TestServer, cleanup func()) {
	return setupTest(true)
}

func setupTest(withAdminPanel bool) (adminPhone, adminPassword string, ts *TestServer, cleanup func()) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		panic("open sqlite: " + err.Error())
	}
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}

	if err := db.AutoMigrate(database.AllModels()...); err != nil {
		panic("migrate: " + err.Error())
	}

	// Seed admin
	adminPhone = "0000000000"
	adminPassword = "admin-pass-123"
	snowflake := database.NewSnowflakeGenerator(0)
	adminPasswordHash, err := auth.HashPassword(adminPassword)
	if err != nil {
		panic("hash admin password: " + err.Error())
	}
	if err := database.EnsureAdminSeed(db, adminPhone, "测试管理员", adminPasswordHash, snowflake); err != nil {
		panic("seed admin: " + err.Error())
	}

	// 每个测试使用独立目录，避免插件 ZIP 和同步 blob 在用例之间串数据。
	tmpDir, err := os.MkdirTemp("", "lynai-backend-test-storage-")
	if err != nil {
		panic("create temp storage: " + err.Error())
	}
	storage, err := market.NewStorage(tmpDir)
	if err != nil {
		panic("storage: " + err.Error())
	}

	blobStorage, err := sync.NewBlobStorage(tmpDir)
	if err != nil {
		panic("blob storage: " + err.Error())
	}

	jwtMgr := auth.NewJWTManager("test-secret-key-for-testing")
	authSvc := auth.NewService(db, jwtMgr, snowflake)
	marketSvc := market.NewService(db, storage)
	syncSvc := sync.NewService(db, blobStorage)
	relaySvc := relay.NewService(db)

	authHandler := auth.NewHandler(authSvc)
	marketHandler := market.NewHandler(marketSvc)
	syncHandler := sync.NewHandler(syncSvc)
	relayHandler := relay.NewHandler(relaySvc)

	var adminHandler *admin.Handler
	if withAdminPanel {
		adminHandler, err = admin.NewHandler(db, authSvc, marketSvc, jwtMgr)
		if err != nil {
			panic("admin templates: " + err.Error())
		}
	}

	r := server.Setup(authHandler, jwtMgr, marketHandler, syncHandler, relayHandler, adminHandler)
	ts = NewTestServer(r)

	cleanup = func() {
		ts.Close()
		_ = os.RemoveAll(tmpDir)
	}

	return adminPhone, adminPassword, ts, cleanup
}
