package handler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"regexp"
	"time"

	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// mobileRedirect is the ONLY place the callback may send a mobile login to.
const mobileRedirect = "mypersonal://oauth"

// mobileAuthBodyLimit caps the JSON bodies of the unauthenticated mobile auth endpoints.
const mobileAuthBodyLimit = 8 << 10

var (
	pkceChallengeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)        // base64url(SHA-256), no padding
	pkceVerifierRe  = regexp.MustCompile(`^[A-Za-z0-9\-._~]{43,128}$`) // RFC 7636
)

// OnceStore marks a key as used; ok is false when it already was.
type OnceStore interface {
	Once(ctx context.Context, key string, ttl time.Duration) (ok bool, err error)
}

type redisOnce struct{ rdb *redis.Client }

// NewRedisOnce backs OnceStore with Redis SET NX EX.
func NewRedisOnce(addr, password string) OnceStore {
	return redisOnce{redis.NewClient(&redis.Options{Addr: addr, Password: password})}
}

func (r redisOnce) Once(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return r.rdb.SetNX(ctx, key, 1, ttl).Result()
}

// SetMobile wires the single-use store for mobile codes; nil (no Redis)
// makes /auth/mobile/exchange answer 503.
func (h *AuthHandler) SetMobile(once OnceStore) { h.once = once }

type exchangeRequest struct {
	Code         string `json:"code"`
	CodeVerifier string `json:"code_verifier"`
}

// MobileExchange trades the one-time code from the deep link for tokens (PKCE).
func (h *AuthHandler) MobileExchange(c *gin.Context) {
	invalid := func() { response.Unauthorized(c, "invalid_code") }
	if h.google == nil {
		response.Error(c, http.StatusServiceUnavailable, "google_not_configured")
		return
	}
	var req exchangeRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, mobileAuthBodyLimit)
	if c.ShouldBindJSON(&req) != nil || !pkceVerifierRe.MatchString(req.CodeVerifier) {
		invalid()
		return
	}
	mc, err := h.google.VerifyMobileCode(req.Code)
	if err != nil {
		invalid()
		return
	}
	sum := sha256.Sum256([]byte(req.CodeVerifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(got), []byte(mc.Challenge)) != 1 {
		invalid()
		return
	}
	if h.once == nil {
		response.Error(c, http.StatusServiceUnavailable, "mobile_login_unavailable")
		return
	}
	ok, err := h.once.Once(c, "mobile_code:"+mc.JTI, 120*time.Second)
	if err != nil {
		response.Error(c, http.StatusServiceUnavailable, "mobile_login_unavailable")
		return
	}
	if !ok {
		invalid()
		return
	}
	access, err := h.jwtManager.GenerateAccess(mc.UserID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	refresh, err := h.jwtManager.GenerateRefresh(mc.UserID)
	if err != nil {
		response.Internal(c, err)
		return
	}
	response.OK(c, struct {
		tokenResponse
		RefreshToken string `json:"refresh_token"`
	}{tokenResponse{access, time.Now().Add(15 * time.Minute)}, refresh})
}
