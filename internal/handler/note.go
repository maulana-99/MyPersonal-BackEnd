package handler

import (
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NoteHandler owns standalone notes and notes attached to a schedule or task.
type NoteHandler struct {
	db *pgxpool.Pool
}

func NewNoteHandler(db *pgxpool.Pool) *NoteHandler {
	return &NoteHandler{db: db}
}

type Note struct {
	ID         uuid.UUID  `json:"id"`
	Title      string     `json:"title"`
	Body       string     `json:"body"`
	ScheduleID *uuid.UUID `json:"schedule_id"`
	TaskID     *uuid.UUID `json:"task_id"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type noteRequest struct {
	Title      string     `json:"title"       binding:"required,max=200"`
	Body       string     `json:"body"        binding:"max=20000"`
	ScheduleID *uuid.UUID `json:"schedule_id"`
	TaskID     *uuid.UUID `json:"task_id"`
}

const noteColumns = `id, title, COALESCE(body, ''), schedule_id, task_id, created_at, updated_at`

func scanNote(row rowScanner) (Note, error) {
	var n Note
	err := row.Scan(&n.ID, &n.Title, &n.Body, &n.ScheduleID, &n.TaskID, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return Note{}, err
	}
	return n, nil
}

// List: GET /notes?schedule_id=&task_id=
func (h *NoteHandler) List(c *gin.Context) {
	userID := middleware.GetUserID(c)
	p := pagination(c)

	var scheduleID, taskID *uuid.UUID
	if raw := c.Query("schedule_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			response.BadRequest(c, "invalid schedule_id")
			return
		}
		scheduleID = &id
	}
	if raw := c.Query("task_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			response.BadRequest(c, "invalid task_id")
			return
		}
		taskID = &id
	}

	rows, err := h.db.Query(c, `
		SELECT `+noteColumns+`
		FROM notes
		WHERE user_id = $1
		  AND ($2::uuid IS NULL OR schedule_id = $2)
		  AND ($3::uuid IS NULL OR task_id = $3)
		ORDER BY updated_at DESC
		LIMIT $4 OFFSET $5
	`, userID, scheduleID, taskID, p.Limit, p.Offset)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	notes := make([]Note, 0, p.Limit)
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			respondDBError(c, err)
			return
		}
		notes = append(notes, n)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, notes)
}

func (h *NoteHandler) Get(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	n, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	response.OK(c, n)
}

func (h *NoteHandler) Create(c *gin.Context) {
	var req noteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	userID := middleware.GetUserID(c)

	// The FK proves the row exists, not that it is the caller's.
	if !ownsOptional(c, h.db, "schedules", req.ScheduleID, userID) ||
		!ownsOptional(c, h.db, "tasks", req.TaskID, userID) {
		return
	}

	var id uuid.UUID
	err := h.db.QueryRow(c, `
		INSERT INTO notes (user_id, title, body, schedule_id, task_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, userID, req.Title, req.Body, req.ScheduleID, req.TaskID).Scan(&id)
	if err != nil {
		respondDBError(c, err)
		return
	}

	n, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	response.Created(c, n)
}

// Update rewrites the note and its attachments. Sending null detaches.
func (h *NoteHandler) Update(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}

	var req noteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	userID := middleware.GetUserID(c)

	if !ownsOptional(c, h.db, "schedules", req.ScheduleID, userID) ||
		!ownsOptional(c, h.db, "tasks", req.TaskID, userID) {
		return
	}

	tag, err := h.db.Exec(c, `
		UPDATE notes
		SET title = $1, body = $2, schedule_id = $3, task_id = $4, updated_at = NOW()
		WHERE id = $5 AND user_id = $6
	`, req.Title, req.Body, req.ScheduleID, req.TaskID, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.NotFound(c)
		return
	}

	n, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	response.OK(c, n)
}

func (h *NoteHandler) Delete(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	tag, err := h.db.Exec(c, `DELETE FROM notes WHERE id = $1 AND user_id = $2`, id, userID)
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

func (h *NoteHandler) fetch(c *gin.Context, id, userID uuid.UUID) (Note, error) {
	row := h.db.QueryRow(c, `
		SELECT `+noteColumns+`
		FROM notes WHERE id = $1 AND user_id = $2
	`, id, userID)
	return scanNote(row)
}
