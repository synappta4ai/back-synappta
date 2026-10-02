package middleware

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Auth parses the JWT Bearer token and sets user claims in the context.
// The token carries platform identity (sub, username, role_level) plus the
// selected tenant_id, which the tenancy middleware consumes.
func Auth(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing authorization header"})
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid authorization header format"})
			return
		}

		tokenString := parts[1]
		token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return []byte(jwtSecret), nil
		})

		if err != nil || !token.Valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}

		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token claims"})
			return
		}

		if sub, ok := claims["sub"].(float64); ok {
			c.Set("userID", int64(sub))
		}
		if username, ok := claims["username"].(string); ok {
			c.Set("username", username)
		}
		if tenantID, ok := claims["tenant_id"].(float64); ok {
			c.Set("tenant_id", int64(tenantID))
		}
		if roleLevel, ok := claims["role_level"].(float64); ok {
			c.Set("role_level", roleLevel)
		}
		if roleName, ok := claims["role_name"].(string); ok {
			c.Set("role_name", roleName)
		}
		// Permisos de la membresía activa (tenant-scoped, p.ej. "events.manage").
		if rawPerms, ok := claims["permissions"].([]interface{}); ok {
			perms := make([]string, 0, len(rawPerms))
			for _, p := range rawPerms {
				if s, ok := p.(string); ok {
					perms = append(perms, s)
				}
			}
			c.Set("permissions", perms)
		}
		if name, ok := claims["name"].(string); ok {
			c.Set("user_name", name)
		}
		if email, ok := claims["email"].(string); ok {
			c.Set("user_email", email)
		}

		c.Next()
	}
}

// RequireRole returns a middleware that rejects requests whose role level
// is greater than maxLevel (lower level = more privilege).
func RequireRole(maxLevel int) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, exists := c.Get("role_level")
		if !exists {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "access denied: no role"})
			return
		}

		level, ok := raw.(float64)
		if !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "access denied: invalid role"})
			return
		}

		if int(level) > maxLevel {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "access denied: insufficient role level"})
			return
		}

		c.Next()
	}
}

// RequirePlatformSuperadmin allows only role level 0 (platform superadmin).
func RequirePlatformSuperadmin() gin.HandlerFunc {
	return RequireRole(0)
}
