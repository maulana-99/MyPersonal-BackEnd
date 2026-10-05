package handler

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	pkgjwt "github.com/chronaxis/daily-planner-backend/pkg/jwt"
	"github.com/chronaxis/daily-planner-backend/pkg/password"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type AuthHandler struct {
	db         *pgxpool.Pool
	jwtManager *pkgjwt.Manager

	// Google-first login (docs/google-first.md); google is nil when off.
	google        *googlecal.Service
	frontendURL   string
	allowedEmails []string
	allowPassword bool
}

// SetGoogle enables Google login. With it on, password register/login answer
// 403 password_auth_disabled unless allowPassword is set.
func (h *AuthHandler) SetGoogle(svc *googlecal.Service, frontendURL string, allowedEmails []string, allowPassword bool) {
	h.google, h.frontendURL, h.allowedEmails, h.allowPassword = svc, frontendURL, allowedEmails, allowPassword
}

// passwordDisabled writes 403 and returns true when password auth is switched off.
func (h *AuthHandler) passwordDisabled(c *gin.Context) bool {
	if h.google != nil && !h.allowPassword {
		response.Error(c, http.StatusForbidden, "password_auth_disabled")
		return true
	}
	return false
}

func NewAuthHandler(db *pgxpool.Pool, jwtManager *pkgjwt.Manager) *AuthHandler {
	return &AuthHandler{db: db, jwtManager: jwtManager}
}

type registerRequest struct {
	Name     string `json:"name"     binding:"required,min=2,max=100"`
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required,min=8"`
}

type loginRequest struct {
	Email    string `json:"email"    binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

type tokenResponse struct {
	AccessToken string    `json:"access_token"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (h *AuthHandler) Register(c *gin.Context) {
	if h.passwordDisabled(c) {
		return
	}
	var req registerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	hash, err := password.Hash(req.Password)
	if err != nil {
		response.Internal(c, err)
		return
	}

	var userID uuid.UUID
	err = h.db.QueryRow(c, `
		INSERT INTO users (name, email, password_hash)
		VALUES ($1, $2, $3)
		RETURNING id
	`, req.Name, req.Email, hash).Scan(&userID)
	if err != nil {
		response.BadRequest(c, "email already in use or invalid data")
		return
	}

	h.seedCategories(c, userID)

	c.JSON(http.StatusCreated, gin.H{"message": "registered successfully"})
}

// seedCategories adds the default categories (Plan.md §2.8). A seed failure
// must not block sign-up: the user can create categories manually.
func (h *AuthHandler) seedCategories(ctx context.Context, userID uuid.UUID) {
	for _, dc := range defaultCategories {
		if _, err := h.db.Exec(ctx, `
			INSERT INTO categories (user_id, name, color)
			VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING
		`, userID, dc.Name, dc.Color); err != nil {
			break
		}
	}
}

func (h *AuthHandler) Login(c *gin.Context) {
	if h.passwordDisabled(c) {
		return
	}
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	var userID uuid.UUID
	var hash *string // NULL for Google-created users
	err := h.db.QueryRow(c, `
		SELECT id, password_hash FROM users WHERE email = $1
	`, req.Email).Scan(&userID, &hash)
	if err != nil || hash == nil {
		response.Unauthorized(c, "invalid credentials")
		return
	}

	if !password.Verify(req.Password, *hash) {
		response.Unauthorized(c, "invalid credentials")
		return
	}

	accessToken, err := h.jwtManager.GenerateAccess(userID)
	if err != nil {
		response.Internal(c, err)
		return
	}

	if err := h.setRefreshCookie(c, userID); err != nil {
		response.Internal(c, err)
		return
	}

	response.OK(c, tokenResponse{
		AccessToken: accessToken,
		ExpiresAt:   time.Now().Add(15 * time.Minute),
	})
}

// setRefreshCookie issues the refresh token in an HttpOnly Secure cookie (per
// architecture spec). Shared by password and Google login.
func (h *AuthHandler) setRefreshCookie(c *gin.Context, userID uuid.UUID) error {
	refreshToken, err := h.jwtManager.GenerateRefresh(userID)
	if err != nil {
		return err
	}
	c.SetCookie("refresh_token", refreshToken, int((7 * 24 * time.Hour).Seconds()), "/", "", true, true)
	return nil
}

func (h *AuthHandler) Refresh(c *gin.Context) {
	cookie, err := c.Cookie("refresh_token")
	if err != nil {
		response.Unauthorized(c, "missing refresh token")
		return
	}

	claims, err := h.jwtManager.Verify(cookie)
	if err != nil || claims.TokenType != pkgjwt.RefreshToken {
		response.Unauthorized(c, "invalid refresh token")
		return
	}

	accessToken, err := h.jwtManager.GenerateAccess(claims.UserID)
	if err != nil {
		response.Internal(c, err)
		return
	}

	response.OK(c, tokenResponse{
		AccessToken: accessToken,
		ExpiresAt:   time.Now().Add(15 * time.Minute),
	})
}

func (h *AuthHandler) Logout(c *gin.Context) {
	c.SetCookie("refresh_token", "", -1, "/", "", true, true)
	c.JSON(http.StatusOK, gin.H{"message": "logged out"})
}

func (h *AuthHandler) Me(c *gin.Context) {
	userID := middleware.GetUserID(c)
	var name, email string
	var avatar *string
	err := h.db.QueryRow(c, `SELECT name, email, avatar_url FROM users WHERE id = $1`, userID).Scan(&name, &email, &avatar)
	if err != nil {
		response.NotFound(c)
		return
	}
	response.OK(c, gin.H{"id": userID, "name": name, "email": email, "avatar_url": avatar})
}

// ---- Google login -----------------------------------------------------------

const nonceCookie = "g_nonce"

func setNonceCookie(c *gin.Context, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: nonceCookie, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
}

// GoogleLogin starts sign-in: nonce cookie + redirect to Google's consent screen.
func (h *AuthHandler) GoogleLogin(c *gin.Context) {
	if h.google == nil {
		response.Error(c, http.StatusServiceUnavailable, "google_not_configured")
		return
	}
	nonce, authURL, err := h.google.LoginStart()
	if err != nil {
		response.Internal(c, err)
		return
	}
	setNonceCookie(c, nonce, int(googlecal.StateTTL.Seconds()))
	c.Redirect(http.StatusFound, authURL)
}

// GoogleCallback finishes sign-in. It is a browser redirect, so every outcome is
// a 302 with a fixed error code; the frontend gets its access token via /auth/refresh.
func (h *AuthHandler) GoogleCallback(c *gin.Context) {
	if h.google == nil {
		response.Error(c, http.StatusServiceUnavailable, "google_not_configured")
		return
	}
	nonce, _ := c.Cookie(nonceCookie)
	setNonceCookie(c, "", -1) // single use, success or not
	fail := func(reason string) {
		c.Redirect(http.StatusFound, h.frontendURL+"/login?error="+url.QueryEscape(reason))
	}

	if c.Query("error") != "" {
		fail(googlecal.ReasonDenied)
		return
	}
	id, reason := h.google.LoginExchange(c, c.Query("code"), c.Query("state"), nonce)
	if reason != "" {
		fail(reason)
		return
	}
	if len(h.allowedEmails) > 0 && !slices.Contains(h.allowedEmails, strings.ToLower(id.Email)) {
		fail("not_allowed")
		return
	}

	userID, reason := h.resolveGoogleUser(c, id)
	if reason != "" {
		fail(reason)
		return
	}
	// Profile picture is cosmetic: keep only an https URL and never fail login over it.
	if strings.HasPrefix(id.Picture, "https://") && len(id.Picture) <= 500 {
		_, _ = h.db.Exec(c, `UPDATE users SET avatar_url = $1 WHERE id = $2`, id.Picture, userID)
	}
	switch err := h.google.SaveGrant(c, userID, id.Email, id.RefreshToken); {
	case errors.Is(err, googlecal.ErrNotConnected):
		fail(googlecal.ReasonExchange) // Google issued no refresh token and none is stored
		return
	case err != nil:
		fail(googlecal.ReasonInternal)
		return
	}
	if err := h.setRefreshCookie(c, userID); err != nil {
		fail(googlecal.ReasonInternal)
		return
	}
	h.google.SyncBackground(userID)
	c.Redirect(http.StatusFound, h.frontendURL+"/")
}

// resolveGoogleUser finds the user by google_sub, else links by email, else
// creates one. reason is "" on success.
func (h *AuthHandler) resolveGoogleUser(c *gin.Context, id googlecal.Identity) (uuid.UUID, string) {
	var userID uuid.UUID
	err := h.db.QueryRow(c, `SELECT id FROM users WHERE google_sub = $1`, id.Sub).Scan(&userID)
	if err == nil {
		return userID, ""
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, googlecal.ReasonInternal
	}

	var sub *string
	err = h.db.QueryRow(c, `SELECT id, google_sub FROM users WHERE lower(email) = lower($1)`, id.Email).Scan(&userID, &sub)
	switch {
	case err == nil:
		if sub != nil { // account already bound to a different Google identity
			return uuid.Nil, "not_allowed"
		}
		if _, err := h.db.Exec(c, `UPDATE users SET google_sub = $1 WHERE id = $2 AND google_sub IS NULL`, id.Sub, userID); err != nil {
			return uuid.Nil, googlecal.ReasonInternal
		}
		return userID, ""
	case !errors.Is(err, pgx.ErrNoRows):
		return uuid.Nil, googlecal.ReasonInternal
	}

	name := strings.TrimSpace(id.Name)
	if name == "" {
		name, _, _ = strings.Cut(id.Email, "@")
	}
	if err := h.db.QueryRow(c, `INSERT INTO users (name, email, google_sub) VALUES ($1, $2, $3) RETURNING id`,
		name, id.Email, id.Sub).Scan(&userID); err != nil {
		return uuid.Nil, googlecal.ReasonInternal
	}
	h.seedCategories(c, userID)
	return userID, ""
}
