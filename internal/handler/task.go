package handler

import (
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TaskHandler owns tasks and their subtasks.
type TaskHandler struct {
	db     *pgxpool.Pool
	google *googlecal.TasksService // nil = tasks are always local
}

func NewTaskHandler(db *pgxpool.Pool) *TaskHandler {
	return &TaskHandler{db: db}
}

type Subtask struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Completed bool      `json:"completed"`
}

type Task struct {
	ID          uuid.UUID  `json:"id"`
	CategoryID  *uuid.UUID `json:"category_id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	DueDate     *string    `json:"due_date"`
	Priority    string     `json:"priority"`
	Status      string     `json:"status"`
	Subtasks    []Subtask  `json:"subtasks"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`

	// Google-shaped fields, defaults only in local mode.
	Notes       string     `json:"notes"`
	CompletedAt *time.Time `json:"completed_at"`
	Parent      *string    `json:"parent"`
	Position    string     `json:"position"`
	WebViewLink *string    `json:"web_view_link"`
}

type taskRequest struct {
	Title       string     `json:"title"       binding:"required,max=200"`
	Description string     `json:"description" binding:"max=2000"`
	CategoryID  *uuid.UUID `json:"category_id"`
	DueDate     *string    `json:"due_date"` // "2006-01-02"
	Priority    string     `json:"priority"  binding:"omitempty,oneof=low medium high"`
}

var validPriorities = map[string]bool{"low": true, "medium": true, "high": true}

const taskColumns = `id, category_id, title, COALESCE(description, ''), due_date,
	priority, status, created_at, updated_at`

func scanTaskBase(row rowScanner) (Task, error) {
	var t Task
	var due *time.Time
	err := row.Scan(&t.ID, &t.CategoryID, &t.Title, &t.Description, &due,
		&t.Priority, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Task{}, err
	}
	if due != nil {
		s := due.Format("2006-01-02")
		t.DueDate = &s
	}
	t.Subtasks = []Subtask{}
	t.Notes = t.Description
	return t, nil
}

func (h *TaskHandler) List(c *gin.Context) {
	if use, stop := h.useGoogle(c); stop {
		return
	} else if use {
		h.gList(c)
		return
	}
	userID := middleware.GetUserID(c)
	p := pagination(c)

	var status *string
	if raw := c.Query("status"); raw != "" {
		if raw != "pending" && raw != "completed" {
			response.BadRequest(c, "status must be pending or completed")
			return
		}
		status = &raw
	}

	var categoryID *uuid.UUID
	if raw := c.Query("category_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			response.BadRequest(c, "invalid category_id")
			return
		}
		categoryID = &id
	}

	var dueBefore *string
	if raw := c.Query("due_before"); raw != "" {
		if _, err := time.Parse("2006-01-02", raw); err != nil {
			response.BadRequest(c, "due_before must be YYYY-MM-DD")
			return
		}
		dueBefore = &raw
	}

	rows, err := h.db.Query(c, `
		SELECT `+taskColumns+`
		FROM tasks
		WHERE user_id = $1
		  AND ($2::text IS NULL OR status = $2)
		  AND ($3::uuid IS NULL OR category_id = $3)
		  AND ($4::date IS NULL OR due_date <= $4)
		ORDER BY (due_date IS NULL), due_date ASC, created_at DESC
		LIMIT $5 OFFSET $6
	`, userID, status, categoryID, dueBefore, p.Limit, p.Offset)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	tasks := make([]Task, 0, p.Limit)
	ids := make([]uuid.UUID, 0, p.Limit)
	for rows.Next() {
		t, err := scanTaskBase(rows)
		if err != nil {
			respondDBError(c, err)
			return
		}
		tasks = append(tasks, t)
		ids = append(ids, t.ID)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	subs, err := h.subtasksFor(c, ids)
	if err != nil {
		respondDBError(c, err)
		return
	}
	for i := range tasks {
		if s, ok := subs[tasks[i].ID]; ok {
			tasks[i].Subtasks = s
		}
	}

	response.OK(c, tasks)
}

func (h *TaskHandler) Get(c *gin.Context) {
	if use, stop := h.useGoogle(c); stop {
		return
	} else if use {
		h.gGet(c)
		return
	}
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	t, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}

	subs, err := h.subtasksFor(c, []uuid.UUID{id})
	if err != nil {
		respondDBError(c, err)
		return
	}
	t.Subtasks = subs[id]

	response.OK(c, t)
}

func (h *TaskHandler) Create(c *gin.Context) {
	if use, stop := h.useGoogle(c); stop {
		return
	} else if use {
		h.gCreate(c)
		return
	}
	var req taskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if req.Priority == "" {
		req.Priority = "medium"
	}
	if req.DueDate != nil {
		if _, err := time.Parse("2006-01-02", *req.DueDate); err != nil {
			response.BadRequest(c, "due_date must be YYYY-MM-DD")
			return
		}
	}

	userID := middleware.GetUserID(c)

	var id uuid.UUID
	err := h.db.QueryRow(c, `
		INSERT INTO tasks (user_id, category_id, title, description, due_date, priority)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, userID, req.CategoryID, req.Title, req.Description, req.DueDate, req.Priority).Scan(&id)
	if err != nil {
		respondDBError(c, err)
		return
	}

	t, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	response.Created(c, t)
}

func (h *TaskHandler) Update(c *gin.Context) {
	if use, stop := h.useGoogle(c); stop {
		return
	} else if use {
		h.gUpdate(c)
		return
	}
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}

	var req taskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if req.Priority != "" && !validPriorities[req.Priority] {
		response.BadRequest(c, "priority must be low, medium, or high")
		return
	}
	if req.DueDate != nil {
		if _, err := time.Parse("2006-01-02", *req.DueDate); err != nil {
			response.BadRequest(c, "due_date must be YYYY-MM-DD")
			return
		}
	}

	userID := middleware.GetUserID(c)

	// COALESCE keeps the existing priority when the request omits it.
	tag, err := h.db.Exec(c, `
		UPDATE tasks
		SET category_id = $1, title = $2, description = $3, due_date = $4,
		    priority = COALESCE(NULLIF($5, ''), priority), updated_at = NOW()
		WHERE id = $6 AND user_id = $7
	`, req.CategoryID, req.Title, req.Description, req.DueDate, req.Priority, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.NotFound(c)
		return
	}

	t, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	subs, _ := h.subtasksFor(c, []uuid.UUID{id})
	t.Subtasks = subs[id]

	response.OK(c, t)
}

func (h *TaskHandler) Delete(c *gin.Context) {
	if use, stop := h.useGoogle(c); stop {
		return
	} else if use {
		h.gDelete(c)
		return
	}
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	tag, err := h.db.Exec(c, `DELETE FROM tasks WHERE id = $1 AND user_id = $2`, id, userID)
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

// Complete toggles pending <-> completed.
func (h *TaskHandler) Complete(c *gin.Context) {
	if use, stop := h.useGoogle(c); stop {
		return
	} else if use {
		h.gComplete(c)
		return
	}
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	tag, err := h.db.Exec(c, `
		UPDATE tasks
		SET status = CASE WHEN status = 'completed' THEN 'pending' ELSE 'completed' END,
		    updated_at = NOW()
		WHERE id = $1 AND user_id = $2
	`, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.NotFound(c)
		return
	}

	t, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	subs, _ := h.subtasksFor(c, []uuid.UUID{id})
	t.Subtasks = subs[id]

	response.OK(c, t)
}

// ---- subtasks --------------------------------------------------------------

type subtaskRequest struct {
	Title     string `json:"title"`
	Completed *bool  `json:"completed"`
}

func (h *TaskHandler) AddSubtask(c *gin.Context) {
	taskID, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	var req subtaskRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Title == "" {
		response.BadRequest(c, "title is required")
		return
	}
	userID := middleware.GetUserID(c)

	// Ownership gate: the parent task must belong to the caller.
	var exists bool
	if err := h.db.QueryRow(c, `SELECT TRUE FROM tasks WHERE id = $1 AND user_id = $2`, taskID, userID).Scan(&exists); err != nil {
		response.NotFound(c)
		return
	}

	var s Subtask
	err := h.db.QueryRow(c, `
		INSERT INTO task_subtasks (task_id, title) VALUES ($1, $2)
		RETURNING id, title, completed
	`, taskID, req.Title).Scan(&s.ID, &s.Title, &s.Completed)
	if err != nil {
		respondDBError(c, err)
		return
	}
	response.Created(c, s)
}

func (h *TaskHandler) UpdateSubtask(c *gin.Context) {
	taskID, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	subID, ok := paramUUID(c, "sid")
	if !ok {
		return
	}
	var req subtaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	userID := middleware.GetUserID(c)

	var s Subtask
	err := h.db.QueryRow(c, `
		UPDATE task_subtasks st
		SET title = COALESCE(NULLIF($1, ''), st.title),
		    completed = COALESCE($2, st.completed)
		FROM tasks t
		WHERE st.id = $3 AND st.task_id = $4 AND t.id = st.task_id AND t.user_id = $5
		RETURNING st.id, st.title, st.completed
	`, req.Title, req.Completed, subID, taskID, userID).Scan(&s.ID, &s.Title, &s.Completed)
	if err != nil {
		respondDBError(c, err)
		return
	}
	response.OK(c, s)
}

func (h *TaskHandler) DeleteSubtask(c *gin.Context) {
	taskID, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	subID, ok := paramUUID(c, "sid")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	tag, err := h.db.Exec(c, `
		DELETE FROM task_subtasks st
		USING tasks t
		WHERE st.id = $1 AND st.task_id = $2 AND t.id = st.task_id AND t.user_id = $3
	`, subID, taskID, userID)
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

// ---- internals -------------------------------------------------------------

func (h *TaskHandler) fetch(c *gin.Context, id, userID uuid.UUID) (Task, error) {
	row := h.db.QueryRow(c, `SELECT `+taskColumns+` FROM tasks WHERE id = $1 AND user_id = $2`, id, userID)
	return scanTaskBase(row)
}

func (h *TaskHandler) subtasksFor(c *gin.Context, ids []uuid.UUID) (map[uuid.UUID][]Subtask, error) {
	out := make(map[uuid.UUID][]Subtask, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	rows, err := h.db.Query(c, `
		SELECT task_id, id, title, completed
		FROM task_subtasks
		WHERE task_id = ANY($1)
		ORDER BY id
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var taskID uuid.UUID
		var s Subtask
		if err := rows.Scan(&taskID, &s.ID, &s.Title, &s.Completed); err != nil {
			return nil, err
		}
		out[taskID] = append(out[taskID], s)
	}
	return out, rows.Err()
}
