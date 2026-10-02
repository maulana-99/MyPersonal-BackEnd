package middleware

import (
	"net/http"
	"strings"

	"github.com/chronaxis/daily-planner-backend/pkg/jwt"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const UserIDKey = "user_id"

// Auth extracts and validates Bearer token from Authorization header.
// 401 causes: missing header, wrong prefix, invalid/expired token, wrong token type.
func Auth(jwtManager *jwt.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing Authorization header"})
			return
		}

		// Must be exactly "Bearer <token>"
		parts := strings.SplitN(header, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authorization header must be: Bearer <token>"})
			return
		}

		claims, err := jwtManager.Verify(parts[1])
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		if claims.TokenType != jwt.AccessToken {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "refresh token not accepted here"})
			return
		}

		c.Set(UserIDKey, claims.UserID)
		c.Next()
	}
}

// GetUserID pulls authenticated user ID from context. Panics if Auth middleware not applied.
func GetUserID(c *gin.Context) uuid.UUID {
	return c.MustGet(UserIDKey).(uuid.UUID)
}
