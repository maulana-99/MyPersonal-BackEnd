package handler

import (
	"errors"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FocusHandler owns focus sessions — timed work/study blocks.
//
// The server is the clock of record: `started_at`, `paused_at`, and
// `paused_seconds` are written here, and `actual_minutes` is always derived
// from them. A client that crashes mid-session therefore loses nothing, and a
// session left running for hours cannot be inflated by a stale tab.
type FocusHandler struct {
	db *pgxpool.Pool
}

func NewFocusHandler(db *pgxpool.Pool) *FocusHandler {
	return &FocusHandler{db: db}
}

type FocusSession struct {
	ID             uuid.UUID  `json:"id"`
	CategoryID     *uuid.UUID `json:"category_id"`
	TaskID         *uuid.UUID `json:"task_id"`
	Title          string     `json:"title"`
	Goal           string     `json:"goal"`
	PlannedMinutes int        `json:"planned_minutes"`
	StartedAt      time.Time  `json:"started_at"`
	EndedAt        *time.Time `json:"ended_at"`
	PausedSeconds  int        `json:"paused_seconds"`
	ActualMinutes  int        `json:"actual_minutes"`
	Status         string     `json:"status"` // running|paused|completed|cancelled
	ElapsedMinutes int        `json:"elapsed_minutes"`
	CreatedAt      time.Time  `json:"created_at"`
}

type focusStartRequest struct {
	Title          string     `json:"title"           binding:"required,max=200"`
	Goal           string     `json:"goal"            binding:"max=2000"`
	CategoryID     *uuid.UUID `json:"category_id"`
	TaskID         *uuid.UUID `json:"task_id"`
	PlannedMinutes int        `json:"planned_minutes" binding:"required,min=1,max=600"`
}

// staleSessionHours is when an abandoned session stops holding the user's
// single active slot. Without it a crashed client blocks every new session.
const staleSessionHours = 12

// Start begins a focus session. One live session per user is enforced by
// `idx_focus_sessions_one_live`; a second start returns 409, not a silent
// second timer.
//
// POST /focus-sessions
func (h *FocusHandler) Start(c *gin.Context) {
	var req focusStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	userID := middleware.GetUserID(c)

	// Referenced rows must belong to the caller (the FK only proves existence).
	if !ownsOptional(c, h.db, "categories", req.CategoryID, userID) ||
		!ownsOptional(c, h.db, "tasks", req.TaskID, userID) {
		return
	}

	tx, err := h.db.Begin(c)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer tx.Rollback(c)

	// Reap an abandoned session so it cannot hold the slot forever.
	if _, err := tx.Exec(c, `
		UPDATE focus_sessions
		SET status = 'cancelled', ended_at = NOW(), updated_at = NOW()
		WHERE user_id = $1
		  AND status IN ('running', 'paused')
		  AND started_at < NOW() - ($2::int * interval '1 hour')
	`, userID, staleSessionHours); err != nil {
		respondDBError(c, err)
		return
	}

	var id uuid.UUID
	err = tx.QueryRow(c, `
		INSERT INTO focus_sessions (user_id, category_id, task_id, title, goal, planned_minutes)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING
		RETURNING id
	`, userID, req.CategoryID, req.TaskID, req.Title, req.Goal, req.PlannedMinutes).Scan(&id)
	if err != nil {
		// ErrNoRows here means the partial unique index rejected the insert:
		// the user already has a live session.
		if errors.Is(err, pgx.ErrNoRows) {
			response.Conflict(c, "a focus session is already running")
			return
		}
		respondDBError(c, err)
		return
	}

	if err := tx.Commit(c); err != nil {
		respondDBError(c, err)
		return
	}

	s, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	// Keep the caller's own elapsed arithmetic honest from the first render.
	s.ElapsedMinutes = elapsedMinutes(s, time.Now())

	response.Created(c, s)
}

// Active returns the live session, or null when none is running.
// GET /focus-sessions/active
func (h *FocusHandler) Active(c *gin.Context) {
	userID := middleware.GetUserID(c)

	s, err := h.scanOne(c, `
		SELECT `+focusColumns+`
		FROM focus_sessions
		WHERE user_id = $1 AND status IN ('running', 'paused')
		ORDER BY started_at DESC
		LIMIT 1
	`, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.OK(c, nil)
			return
		}
		respondDBError(c, err)
		return
	}
	s.ElapsedMinutes = elapsedMinutes(s, time.Now())
	response.OK(c, s)
}

// List returns focus history, newest first.
// GET /focus-sessions?from=&to=&status=
func (h *FocusHandler) List(c *gin.Context) {
	userID := middleware.GetUserID(c)
	p := pagination(c)

	from, ok := optionalTime(c, "from")
	if !ok {
		return
	}
	to, ok := optionalTime(c, "to")
	if !ok {
		return
	}

	status := c.Query("status")
	if status != "" && !validFocusStatus(status) {
		response.BadRequest(c, "invalid status")
		return
	}

	rows, err := h.db.Query(c, `
		SELECT `+focusColumns+`
		FROM focus_sessions
		WHERE user_id = $1
		  AND ($2::timestamptz IS NULL OR started_at >= $2)
		  AND ($3::timestamptz IS NULL OR started_at <= $3)
		  AND ($4::text IS NULL OR status = $4)
		ORDER BY started_at DESC
		LIMIT $5 OFFSET $6
	`, userID, from, to, nullable(status), p.Limit, p.Offset)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	sessions := make([]FocusSession, 0, p.Limit)
	now := time.Now()
	for rows.Next() {
		s, err := scanFocus(rows)
		if err != nil {
			respondDBError(c, err)
			return
		}
		s.ElapsedMinutes = elapsedMinutes(s, now)
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, sessions)
}

func (h *FocusHandler) Get(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	s, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	s.ElapsedMinutes = elapsedMinutes(s, time.Now())
	response.OK(c, s)
}

// Pause holds the timer. POST /focus-sessions/:id/pause
func (h *FocusHandler) Pause(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	tag, err := h.db.Exec(c, `
		UPDATE focus_sessions
		SET status = 'paused', paused_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND user_id = $2 AND status = 'running'
	`, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.BadRequest(c, "session is not running")
		return
	}

	h.respondFresh(c, id, userID)
}

// Resume settles the pause length into paused_seconds before running again.
// POST /focus-sessions/:id/resume
func (h *FocusHandler) Resume(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	tag, err := h.db.Exec(c, `
		UPDATE focus_sessions
		SET paused_seconds = paused_seconds
		      + GREATEST(0, EXTRACT(EPOCH FROM (NOW() - paused_at))::int),
		    paused_at = NULL,
		    status = 'running',
		    updated_at = NOW()
		WHERE id = $1 AND user_id = $2 AND status = 'paused'
	`, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.BadRequest(c, "session is not paused")
		return
	}

	h.respondFresh(c, id, userID)
}

// Complete ends a session and records the measured duration.
// POST /focus-sessions/:id/complete
func (h *FocusHandler) Complete(c *gin.Context) {
	h.finish(c, "completed")
}

// Cancel discards a session, keeping it out of the duration statistics.
// POST /focus-sessions/:id/cancel
func (h *FocusHandler) Cancel(c *gin.Context) {
	h.finish(c, "cancelled")
}

// finish closes a live session. Wall time minus accumulated pause time is the
// only source of actual_minutes — the client never supplies it.
func (h *FocusHandler) finish(c *gin.Context, status string) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	var endedAt time.Time
	err := h.db.QueryRow(c, `
		UPDATE focus_sessions
		SET status = $3,
		    ended_at = NOW(),
		    -- A pause still open at completion counts as paused time.
		    paused_seconds = paused_seconds
		      + CASE WHEN status = 'paused'
		             THEN GREATEST(0, EXTRACT(EPOCH FROM (NOW() - paused_at))::int)
		             ELSE 0 END,
		    paused_at = NULL,
		    actual_minutes = GREATEST(0,
		      FLOOR(EXTRACT(EPOCH FROM (
		        NOW() - started_at
		        - (paused_seconds + CASE WHEN status = 'paused'
		                                 THEN EXTRACT(EPOCH FROM (NOW() - paused_at))
		                                 ELSE 0 END) * interval '1 second'
		      )) / 60)::int),
		    updated_at = NOW()
		WHERE id = $1 AND user_id = $2 AND status IN ('running', 'paused')
		RETURNING ended_at
	`, id, userID, status).Scan(&endedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.BadRequest(c, "session is not active")
			return
		}
		respondDBError(c, err)
		return
	}

	h.respondFresh(c, id, userID)
}

func (h *FocusHandler) Delete(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	tag, err := h.db.Exec(c, `
		DELETE FROM focus_sessions
		WHERE id = $1 AND user_id = $2 AND status NOT IN ('running', 'paused')
	`, id, userID)
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

// Stats summarises focus time for the caller.
// GET /focus-sessions/stats?period=day|week
func (h *FocusHandler) Stats(c *gin.Context) {
	userID := middleware.GetUserID(c)

	period := c.DefaultQuery("period", "week")
	window := "7 days"
	switch period {
	case "day":
		window = "1 day"
	case "week":
	default:
		response.BadRequest(c, "period must be day or week")
		return
	}

	var totalMinutes, sessions, longest int
	err := h.db.QueryRow(c, `
		SELECT COALESCE(SUM(actual_minutes), 0)::int,
		       COUNT(*)::int,
		       COALESCE(MAX(actual_minutes), 0)::int
		FROM focus_sessions
		WHERE user_id = $1
		  AND status = 'completed'
		  AND started_at >= NOW() - $2::interval
	`, userID, window).Scan(&totalMinutes, &sessions, &longest)
	if err != nil {
		respondDBError(c, err)
		return
	}

	avg := 0
	if sessions > 0 {
		avg = totalMinutes / sessions
	}

	response.OK(c, gin.H{
		"period":          period,
		"total_minutes":   totalMinutes,
		"sessions":        sessions,
		"average_minutes": avg,
		"longest_minutes": longest,
	})
}

// ---- internals -------------------------------------------------------------

const focusColumns = `id, category_id, task_id, title, COALESCE(goal, ''),
	planned_minutes, started_at, ended_at, paused_seconds, actual_minutes,
	status, created_at`

func scanFocus(row rowScanner) (FocusSession, error) {
	var s FocusSession
	err := row.Scan(&s.ID, &s.CategoryID, &s.TaskID, &s.Title, &s.Goal,
		&s.PlannedMinutes, &s.StartedAt, &s.EndedAt, &s.PausedSeconds,
		&s.ActualMinutes, &s.Status, &s.CreatedAt)
	if err != nil {
		return FocusSession{}, err
	}
	return s, nil
}

func (h *FocusHandler) fetch(c *gin.Context, id, userID uuid.UUID) (FocusSession, error) {
	return h.scanOne(c, `
		SELECT `+focusColumns+`
		FROM focus_sessions WHERE id = $1 AND user_id = $2
	`, id, userID)
}

func (h *FocusHandler) scanOne(c *gin.Context, sql string, args ...any) (FocusSession, error) {
	return scanFocus(h.db.QueryRow(c, sql, args...))
}

// respondFresh writes the session back after a state change, so the client
// never has to guess what the server just recorded.
func (h *FocusHandler) respondFresh(c *gin.Context, id, userID uuid.UUID) {
	s, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	s.ElapsedMinutes = elapsedMinutes(s, time.Now())
	response.OK(c, s)
}

// elapsedMinutes is the wall-clock run time minus settled AND in-flight pause
// time. Pure arithmetic over the stored row: the UI can show a ticking timer
// without the server sending a number every second.
func elapsedMinutes(s FocusSession, now time.Time) int {
	if s.Status == "completed" || s.Status == "cancelled" {
		return s.ActualMinutes
	}

	run := now.Sub(s.StartedAt).Seconds() - float64(s.PausedSeconds)
	// A session paused with no settled seconds still has an open pause offset
	// only if the DB knows paused_at; the API contract returns the settled
	// number, so the client adds its own local delta while paused.
	if run < 0 {
		return 0
	}
	return int(run / 60)
}

func validFocusStatus(s string) bool {
	switch s {
	case "running", "paused", "completed", "cancelled":
		return true
	}
	return false
}

// ownsOptional verifies an optional FK target belongs to the caller. A nil id
// is always allowed. `table` is only ever an internal literal.
func ownsOptional(c *gin.Context, db *pgxpool.Pool, table string, id *uuid.UUID, userID uuid.UUID) bool {
	if id == nil {
		return true
	}
	var exists bool
	err := db.QueryRow(c, `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id = $1 AND user_id = $2)`, *id, userID).Scan(&exists)
	if err != nil {
		respondDBError(c, err)
		return false
	}
	if !exists {
		response.NotFound(c)
		return false
	}
	return true
}

// optionalTime parses an optional RFC3339 query parameter.
func optionalTime(c *gin.Context, name string) (*time.Time, bool) {
	raw := c.Query(name)
	if raw == "" {
		return nil, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		response.BadRequest(c, name+" must be RFC3339")
		return nil, false
	}
	return &t, true
}

// nullable turns "" into a SQL NULL so one query can express "filter or not".
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
