package handler

import (
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type CategoryHandler struct {
	db *pgxpool.Pool
}

func NewCategoryHandler(db *pgxpool.Pool) *CategoryHandler {
	return &CategoryHandler{db: db}
}

// Default categories seeded for every new user, per Plan.md §2.8.
var defaultCategories = []struct{ Name, Color string }{
	{"Work", "#3b82f6"},
	{"Study", "#8b5cf6"},
	{"Personal", "#10b981"},
	{"Health", "#ef4444"},
	{"Exercise", "#f59e0b"},
	{"Finance", "#14b8a6"},
	{"Social", "#ec4899"},
	{"Other", "#6b7280"},
}

type Category struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Color     string    `json:"color"`
	CreatedAt time.Time `json:"created_at"`
}

type categoryRequest struct {
	Name  string `json:"name"  binding:"required,max=50"`
	Color string `json:"color" binding:"omitempty,hexcolor"`
}

func (h *CategoryHandler) List(c *gin.Context) {
	userID := middleware.GetUserID(c)
	p := pagination(c)

	rows, err := h.db.Query(c, `
		SELECT id, name, COALESCE(color, ''), created_at
		FROM categories
		WHERE user_id = $1
		ORDER BY name
		LIMIT $2 OFFSET $3
	`, userID, p.Limit, p.Offset)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	categories := make([]Category, 0, p.Limit)
	for rows.Next() {
		var cat Category
		if err := rows.Scan(&cat.ID, &cat.Name, &cat.Color, &cat.CreatedAt); err != nil {
			respondDBError(c, err)
			return
		}
		categories = append(categories, cat)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, categories)
}

func (h *CategoryHandler) Create(c *gin.Context) {
	var req categoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	userID := middleware.GetUserID(c)

	var cat Category
	err := h.db.QueryRow(c, `
		INSERT INTO categories (user_id, name, color)
		VALUES ($1, $2, $3)
		RETURNING id, name, COALESCE(color, ''), created_at
	`, userID, req.Name, req.Color).Scan(&cat.ID, &cat.Name, &cat.Color, &cat.CreatedAt)
	if err != nil {
		respondDBError(c, err) // unique index on (user_id, lower(name)) → 409
		return
	}

	response.Created(c, cat)
}

func (h *CategoryHandler) Update(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}

	var req categoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	userID := middleware.GetUserID(c)

	var cat Category
	// user_id in WHERE is the ownership check — a foreign row updates 0 rows.
	err := h.db.QueryRow(c, `
		UPDATE categories
		SET name = $1, color = $2
		WHERE id = $3 AND user_id = $4
		RETURNING id, name, COALESCE(color, ''), created_at
	`, req.Name, req.Color, id, userID).Scan(&cat.ID, &cat.Name, &cat.Color, &cat.CreatedAt)
	if err != nil {
		respondDBError(c, err) // ErrNoRows → 404
		return
	}

	response.OK(c, cat)
}

func (h *CategoryHandler) Delete(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}

	userID := middleware.GetUserID(c)

	tag, err := h.db.Exec(c, `DELETE FROM categories WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.NotFound(c)
		return
	}

	response.NoContent(c)
}
