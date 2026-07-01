package server

import (
	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/admin"
	"github.com/lynai/backend/internal/auth"
	"github.com/lynai/backend/internal/market"
	"github.com/lynai/backend/internal/sync"
)

// Setup creates the Gin engine with all routes and middleware registered.
func Setup(
	authHandler *auth.Handler,
	jwtMgr *auth.JWTManager,
	marketHandler *market.Handler,
	syncHandler *sync.Handler,
	adminHandler *admin.Handler,
) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// CORS
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// Health check
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// --- Auth API ---
	authGrp := r.Group("/auth")
	{
		authGrp.POST("/register", authHandler.Register)
		authGrp.POST("/login", authHandler.Login)
		authGrp.POST("/send-otp", authHandler.SendOTP)
		authGrp.POST("/verify-otp", authHandler.VerifyOTP)
		authGrp.POST("/refresh", authHandler.Refresh)
		authGrp.POST("/logout", auth.AuthMiddleware(jwtMgr), authHandler.Logout)
		authGrp.GET("/me", auth.AuthMiddleware(jwtMgr), authHandler.Me)
	}

	// --- Market API (public) ---
	marketGrp := r.Group("/market")
	{
		marketGrp.GET("/plugins", marketHandler.ListPlugins)
		marketGrp.GET("/plugins/:id", marketHandler.GetPluginDetail)
		marketGrp.GET("/plugins/:id/download", marketHandler.DownloadPlugin)
	}

	// --- Market API (authenticated) ---
	marketAuth := r.Group("/market")
	marketAuth.Use(auth.AuthMiddleware(jwtMgr))
	{
		marketAuth.POST("/plugins/submit", marketHandler.SubmitPlugin)
		marketAuth.GET("/submissions/mine", marketHandler.MySubmissions)
		marketAuth.POST("/updates", marketHandler.CheckUpdates)
	}

	// --- Market API (admin) ---
	marketAdmin := r.Group("/market")
	marketAdmin.Use(auth.AuthMiddleware(jwtMgr), auth.AdminMiddleware())
	{
		marketAdmin.GET("/plugins/pending", marketHandler.ListPending)
		marketAdmin.POST("/plugins/:id/approve", marketHandler.ApprovePlugin)
		marketAdmin.POST("/plugins/:id/reject", marketHandler.RejectPlugin)
	}

	// --- Sync API (authenticated) ---
	syncAuth := r.Group("/sync")
	syncAuth.Use(auth.AuthMiddleware(jwtMgr))
	{
		syncAuth.GET("/status", syncHandler.Status)
		syncAuth.POST("/changes", syncHandler.UploadChanges)
		syncAuth.GET("/changes", syncHandler.GetChanges)
		syncAuth.GET("/blobs", syncHandler.ListBlobs)
		syncAuth.POST("/blobs/:sha256", syncHandler.UploadBlob)
		syncAuth.GET("/blobs/:sha256", syncHandler.DownloadBlob)
	}

	// --- Admin Web Panel ---
	if adminHandler != nil {
		adminHandler.RegisterRoutes(r, jwtMgr)
	}

	return r
}
