package middleware

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

type client struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

var (
	mu      sync.Mutex
	clients = make(map[string]*client)
)

func init() {
	go func() {
		for {
			time.Sleep(time.Minute)
			mu.Lock()
			for ip, client := range clients {
				if time.Since(client.lastSeen) > 3*time.Minute {
					delete(clients, ip)
				}
			}
			mu.Unlock()
		}
	}()
}

// RateLimit limits requests per client IP (used for public file serving).
func RateLimit(r rate.Limit, b int) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()

		mu.Lock()
		if _, found := clients[ip]; !found {
			clients[ip] = &client{limiter: rate.NewLimiter(r, b)}
		}
		clients[ip].lastSeen = time.Now()
		limiter := clients[ip].limiter
		mu.Unlock()

		if !limiter.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "too many requests",
			})
			return
		}

		c.Next()
	}
}

// UserRateLimit limits expensive authenticated endpoints (generation) per user.
// Falls back to client IP when the user id is unavailable.
func UserRateLimit(requestsPerMinute int, burst int) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.ClientIP()
		if v, ok := c.Get("userID"); ok {
			if id, ok := v.(int64); ok {
				key = fmt.Sprintf("user:%d", id)
			}
		}

		mu.Lock()
		cl, found := clients[key]
		if !found {
			cl = &client{limiter: rate.NewLimiter(rate.Limit(float64(requestsPerMinute)/60.0), burst)}
			clients[key] = cl
		}
		cl.lastSeen = time.Now()
		limiter := cl.limiter
		mu.Unlock()

		if !limiter.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded, slow down",
			})
			return
		}

		c.Next()
	}
}
