package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// AuthMiddleware validates a Bearer JWT and injects user info into context.
// Only access tokens are accepted — refresh tokens are rejected.
func AuthMiddleware(jwtMgr *JWTManager, services ...*Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
			return
		}

		var claims *Claims
		var userIsAdmin bool
		var err error
		if len(services) > 0 && services[0] != nil {
			dbUser, authenticatedClaims, authErr := services[0].AuthenticateAccess(token)
			err = authErr
			claims = authenticatedClaims
			if dbUser != nil {
				userIsAdmin = dbUser.IsAdmin
			}
		} else {
			claims, err = jwtMgr.Verify(token)
			if claims != nil {
				userIsAdmin = claims.IsAdmin
			}
		}
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
		c.Set("sessionID", claims.SessionID)
		c.Set("isAdmin", userIsAdmin)
		c.Next()
	}
}

// OptionalAuthMiddleware authenticates a supplied Bearer token while allowing
// requests with no Authorization header to continue as guests.
func OptionalAuthMiddleware(jwtMgr *JWTManager, services ...*Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			c.Next()
			return
		}
		if !strings.HasPrefix(header, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")) == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		var claims *Claims
		var userIsAdmin bool
		var err error
		if len(services) > 0 && services[0] != nil {
			user, authenticatedClaims, authErr := services[0].AuthenticateAccess(token)
			err = authErr
			claims = authenticatedClaims
			if user != nil {
				userIsAdmin = user.IsAdmin
			}
		} else {
			claims, err = jwtMgr.Verify(token)
			if claims != nil {
				userIsAdmin = claims.IsAdmin
			}
		}
		if err != nil || claims == nil || claims.TokenType != TokenTypeAccess {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
		c.Set("userID", claims.UserID)
		c.Set("username", claims.Username)
		c.Set("sessionID", claims.SessionID)
		c.Set("isAdmin", userIsAdmin)
		c.Next()
	}
}

// AdminMiddleware requires the authenticated user to be an admin.
// Must be used after AuthMiddleware.
func AdminMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		isAdmin, ok := c.Get("isAdmin")
		admin, valid := isAdmin.(bool)
		if !ok || !valid || !admin {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "admin access required"})
			return
		}
		c.Next()
	}
}
