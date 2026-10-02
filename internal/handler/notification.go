package handler

import (
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NotificationHandler serves the Notification Center.
type NotificationHandler struct {
	db *pgxpool.Pool
}

func NewNotificationHandler(db *pgxpool.Pool) *NotificationHandler {
	return &NotificationHandler{db: db}
}

type Notification struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	IsRead    bool      `json:"is_read"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *NotificationHandler) List(c *gin.Context) {
	userID := middleware.GetUserID(c)
	p := pagination(c)

	rows, err := h.db.Query(c, `
		SELECT id, title, COALESCE(body, ''), is_read, created_at
		FROM notifications
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, userID, p.Limit, p.Offset)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	list := make([]Notification, 0, p.Limit)
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Title, &n.Body, &n.IsRead, &n.CreatedAt); err != nil {
			respondDBError(c, err)
			return
		}
		list = append(list, n)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, list)
}

// Read marks one notification read (dismiss shares the same state).
func (h *NotificationHandler) Read(c *gin.Context) {
	h.markRead(c, false)
}

// Dismiss is an alias for read kept as its own route: the UI distinguishes
// "dismiss" from "read" but the persisted state is identical.
func (h *NotificationHandler) Dismiss(c *gin.Context) {
	h.markRead(c, false)
}

func (h *NotificationHandler) markRead(c *gin.Context, _ bool) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	var n Notification
	err := h.db.QueryRow(c, `
		UPDATE notifications SET is_read = TRUE
		WHERE id = $1 AND user_id = $2
		RETURNING id, title, COALESCE(body, ''), is_read, created_at
	`, id, userID).Scan(&n.ID, &n.Title, &n.Body, &n.IsRead, &n.CreatedAt)
	if err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, n)
}

// ReadAll marks every unread notification for the caller as read.
func (h *NotificationHandler) ReadAll(c *gin.Context) {
	userID := middleware.GetUserID(c)

	if _, err := h.db.Exec(c, `
		UPDATE notifications SET is_read = TRUE WHERE user_id = $1 AND is_read = FALSE
	`, userID); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, gin.H{"status": "ok"})
}
