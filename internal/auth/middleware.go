package auth

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// AuthMiddleware validates a Bearer JWT and injects user info into context.
// Only access tokens are accepted — refresh tokens are rejected.
func AuthMiddleware(jwtMgr *JWTManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
			return
		}

		claims, err := jwtMgr.Verify(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		// Reject refresh tokens used as access tokens.
		if claims.TokenType != TokenTypeAccess {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token type"})
			return
		}

		c.Set("userID", claims.UserID)
		c.Set("username", claims.Username)
		c.Set("isAdmin", claims.IsAdmin)
		c.Next()
	}
}

// AdminMiddleware requires the authenticated user to be an admin.
// Must be used after AuthMiddleware.
func AdminMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		isAdmin, _ := c.Get("isAdmin")
		if !isAdmin.(bool) {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin access required"})
			return
		}
		c.Next()
	}
}
