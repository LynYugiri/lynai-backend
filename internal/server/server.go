package server

import (
	"github.com/gin-gonic/gin"
	"github.com/lynai/backend/internal/admin"
	"github.com/lynai/backend/internal/auth"
	"github.com/lynai/backend/internal/community"
	"github.com/lynai/backend/internal/device"
	"github.com/lynai/backend/internal/market"
	"github.com/lynai/backend/internal/relay"
	"github.com/lynai/backend/internal/sync"
)

// Setup creates the Gin engine with all routes and middleware registered.
func Setup(
	authHandler *auth.Handler,
	jwtMgr *auth.JWTManager,
	deviceHandler *device.Handler,
	marketHandler *market.Handler,
	communityHandler *community.Handler,
	syncHandler *sync.Handler,
	relayHandler *relay.Handler,
	adminHandler *admin.Handler,
) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// CORS
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, X-LynAI-Protocol, X-LynAI-Device-ID, X-LynAI-Timestamp, X-LynAI-Request-ID, X-LynAI-Body-SHA256, X-LynAI-Signature")
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
		authGrp.POST("/revoke", authHandler.Revoke)
		authGrp.POST("/logout", auth.AuthMiddleware(jwtMgr, authHandler.Service()), authHandler.Logout)
		authGrp.GET("/me", auth.AuthMiddleware(jwtMgr, authHandler.Service()), authHandler.Me)
	}

	if deviceHandler != nil {
		deviceGrp := r.Group("/devices")
		deviceGrp.Use(auth.AuthMiddleware(jwtMgr, authHandler.Service()))
		{
			deviceGrp.POST("/challenge", deviceHandler.Challenge)
			deviceGrp.POST("/enroll", deviceHandler.Enroll)
			deviceGrp.GET("", deviceHandler.List)
			deviceGrp.GET("/current", deviceHandler.Current)
			deviceGrp.PATCH("/:id", deviceHandler.Rename)
			deviceGrp.DELETE("/:id", deviceHandler.Revoke)
		}
	}

	if communityHandler != nil {
		communityPublic := r.Group("/community")
		communityPublic.Use(auth.OptionalAuthMiddleware(jwtMgr, authHandler.Service()))
		{
			communityPublic.GET("/posts", communityHandler.Feed)
			communityPublic.GET("/posts/:id", communityHandler.PostDetail)
			communityPublic.GET("/posts/:id/comments", communityHandler.Comments)
			communityPublic.GET("/users/:id", communityHandler.User)
			communityPublic.GET("/users/:id/posts", communityHandler.UserPosts)
			communityPublic.GET("/profiles/:id", communityHandler.User)
			communityPublic.GET("/media/:id", communityHandler.Media)
		}

		communityAuth := r.Group("/community")
		communityAuth.Use(auth.AuthMiddleware(jwtMgr, authHandler.Service()))
		{
			communityAuth.POST("/media", communityHandler.UploadMedia)
			communityAuth.POST("/posts", communityHandler.CreatePost)
			communityAuth.PATCH("/posts/:id", communityHandler.UpdatePost)
			communityAuth.DELETE("/posts/:id", communityHandler.DeletePost)
			communityAuth.PUT("/me/pinned-post/:id", communityHandler.PinPost)
			communityAuth.DELETE("/me/pinned-post/:id", communityHandler.UnpinPost)
			communityAuth.PUT("/posts/:id/pin", communityHandler.PinPost)
			communityAuth.DELETE("/posts/:id/pin", communityHandler.UnpinPost)
			communityAuth.PUT("/posts/:id/like", communityHandler.LikePost)
			communityAuth.DELETE("/posts/:id/like", communityHandler.UnlikePost)
			communityAuth.PUT("/posts/:id/favorite", communityHandler.FavoritePost)
			communityAuth.DELETE("/posts/:id/favorite", communityHandler.UnfavoritePost)
			communityAuth.GET("/me/favorites", communityHandler.Favorites)
			communityAuth.GET("/favorites", communityHandler.Favorites)
			communityAuth.POST("/posts/:id/comments", communityHandler.CreateComment)
			communityAuth.PATCH("/comments/:id", communityHandler.UpdateComment)
			communityAuth.DELETE("/comments/:id", communityHandler.DeleteComment)
			communityAuth.PATCH("/me/profile", communityHandler.UpdateMyProfile)
		}

		communityAdmin := r.Group("/community/admin")
		communityAdmin.Use(auth.AuthMiddleware(jwtMgr, authHandler.Service()), auth.AdminMiddleware())
		{
			communityAdmin.GET("/audit", communityHandler.Audit)
			communityAdmin.POST("/posts/:id/restore", communityHandler.RestorePost)
			communityAdmin.DELETE("/posts/:id/purge", communityHandler.PurgePost)
			communityAdmin.POST("/comments/:id/restore", communityHandler.RestoreComment)
			communityAdmin.DELETE("/comments/:id/purge", communityHandler.PurgeComment)
		}
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
	marketAuth.Use(auth.AuthMiddleware(jwtMgr, authHandler.Service()))
	{
		marketAuth.POST("/plugins/submit", marketHandler.SubmitPlugin)
		marketAuth.GET("/submissions/mine", marketHandler.MySubmissions)
		marketAuth.POST("/updates", marketHandler.CheckUpdates)
	}

	// --- Market API (admin) ---
	marketAdmin := r.Group("/market")
	marketAdmin.Use(auth.AuthMiddleware(jwtMgr, authHandler.Service()), auth.AdminMiddleware())
	{
		marketAdmin.GET("/plugins/pending", marketHandler.ListPending)
		marketAdmin.POST("/plugins/:id/approve", marketHandler.ApprovePlugin)
		marketAdmin.POST("/plugins/:id/reject", marketHandler.RejectPlugin)
	}

	// --- Sync API (authenticated) ---
	syncAuth := r.Group("/sync")
	syncAuth.Use(auth.AuthMiddleware(jwtMgr, authHandler.Service()))
	{
		syncAuth.GET("/status", syncHandler.Status)
		syncAuth.POST("/changes", syncHandler.UploadChanges)
		syncAuth.POST("/v1/changes", syncHandler.UploadChanges)
		syncAuth.GET("/changes", syncHandler.GetChanges)
		syncAuth.GET("/blobs", syncHandler.ListBlobs)
		syncAuth.POST("/blobs/:sha256", syncHandler.UploadBlob)
		syncAuth.GET("/blobs/:sha256", syncHandler.DownloadBlob)
	}

	// --- Relay API (authenticated) ---
	if relayHandler != nil {
		relayAuth := r.Group("/relay")
		relayAuth.Use(auth.AuthMiddleware(jwtMgr, authHandler.Service()), relayHandler.LoggingMiddleware())
		{
			relayAuth.GET("/v2/config", relayHandler.ConfigV2)
			relayAuth.POST("/v2/chat", relayHandler.ChatV2)
			relayAuth.POST("/v2/transcribe", relayHandler.Transcribe)
			relayAuth.POST("/v2/ocr", relayHandler.OCR)
			relayAuth.POST("/v2/speech/create", relayHandler.SpeechCreate)
			relayAuth.POST("/v2/speech/:audioId/upload", relayHandler.SpeechUpload)
			relayAuth.POST("/v2/speech/:audioId/run", relayHandler.SpeechRun)
			relayAuth.GET("/v2/speech/:audioId/progress", relayHandler.SpeechProgress)
			relayAuth.GET("/v2/speech/:audioId/result", relayHandler.SpeechResult)
			relayAuth.POST("/v2/images/generations", relayHandler.ImageGenerations)
			relayAuth.POST("/chat", relayHandler.Chat)
			relayAuth.POST("/messages", relayHandler.Messages)
			relayAuth.POST("/api/chat", relayHandler.OllamaChat)
			relayAuth.POST("/transcribe", relayHandler.Transcribe)
			relayAuth.POST("/ocr", relayHandler.OCR)
			relayAuth.POST("/speech/create", relayHandler.SpeechCreate)
			relayAuth.POST("/speech/:audioId/upload", relayHandler.SpeechUpload)
			relayAuth.POST("/speech/:audioId/run", relayHandler.SpeechRun)
			relayAuth.GET("/speech/:audioId/progress", relayHandler.SpeechProgress)
			relayAuth.GET("/speech/:audioId/result", relayHandler.SpeechResult)
			relayAuth.POST("/images/generations", relayHandler.ImageGenerations)
			relayAuth.GET("/models", relayHandler.Models)
			relayAuth.GET("/config", relayHandler.Config)
		}
	}

	// --- Admin Web Panel ---
	if adminHandler != nil {
		adminHandler.RegisterRoutes(r, jwtMgr)
	}

	return r
}
