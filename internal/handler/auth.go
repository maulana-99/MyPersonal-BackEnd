package handler

import (
	"net/http"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	pkgjwt "github.com/chronaxis/daily-planner-backend/pkg/jwt"
	"github.com/chronaxis/daily-planner-backend/pkg/password"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type AuthHandler struct {
	db         *pgxpool.Pool
	jwtManager *pkgjwt.Manager
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

	// Seed default categories (Plan.md §2.8). A seed failure must not block
	// registration — the user can create categories manually.
	for _, dc := range defaultCategories {
		if _, err := h.db.Exec(c, `
			INSERT INTO categories (user_id, name, color)
			VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING
		`, userID, dc.Name, dc.Color); err != nil {
			break
		}
	}

	c.JSON(http.StatusCreated, gin.H{"message": "registered successfully"})
}

func (h *AuthHandler) Login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	var userID uuid.UUID
	var hash string
	err := h.db.QueryRow(c, `
		SELECT id, password_hash FROM users WHERE email = $1
	`, req.Email).Scan(&userID, &hash)
	if err != nil {
		response.Unauthorized(c, "invalid credentials")
		return
	}

	if !password.Verify(req.Password, hash) {
		response.Unauthorized(c, "invalid credentials")
		return
	}

	accessToken, err := h.jwtManager.GenerateAccess(userID)
	if err != nil {
		response.Internal(c, err)
		return
	}

	refreshToken, err := h.jwtManager.GenerateRefresh(userID)
	if err != nil {
		response.Internal(c, err)
		return
	}

	// Refresh token in HttpOnly Secure cookie (per architecture spec)
	c.SetCookie("refresh_token", refreshToken, int((7 * 24 * time.Hour).Seconds()), "/", "", true, true)

	response.OK(c, tokenResponse{
		AccessToken: accessToken,
		ExpiresAt:   time.Now().Add(15 * time.Minute),
	})
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
	err := h.db.QueryRow(c, `SELECT name, email FROM users WHERE id = $1`, userID).Scan(&name, &email)
	if err != nil {
		response.NotFound(c)
		return
	}
	response.OK(c, gin.H{"id": userID, "name": name, "email": email})
}
