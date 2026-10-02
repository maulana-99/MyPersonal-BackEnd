package handler

import (
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RoutineHandler owns routines and their ordered items.
type RoutineHandler struct {
	db *pgxpool.Pool
}

func NewRoutineHandler(db *pgxpool.Pool) *RoutineHandler {
	return &RoutineHandler{db: db}
}

type RoutineItem struct {
	ID             uuid.UUID `json:"id"`
	Title          string    `json:"title"`
	Duration       *int      `json:"duration"`
	SortOrder      int       `json:"sort_order"`
	CompletedToday bool      `json:"completed_today"`
}

type Routine struct {
	ID         uuid.UUID     `json:"id"`
	Name       string        `json:"name"`
	RepeatDays []string      `json:"repeat_days"`
	StartTime  *time.Time    `json:"start_time"`
	Items      []RoutineItem `json:"items"`
	CreatedAt  time.Time     `json:"created_at"`
}

type routineItemRequest struct {
	Title    string `json:"title"    binding:"required,max=200"`
	Duration *int   `json:"duration" binding:"omitempty,min=1,max=1440"`
}

type routineRequest struct {
	Name       string               `json:"name"        binding:"required,max=100"`
	RepeatDays []string             `json:"repeat_days" binding:"omitempty,dive,oneof=mon tue wed thu fri sat sun"`
	StartTime  *time.Time           `json:"start_time"`
	Items      []routineItemRequest `json:"items"       binding:"omitempty,dive"`
}

func (h *RoutineHandler) List(c *gin.Context) {
	userID := middleware.GetUserID(c)
	p := pagination(c)

	rows, err := h.db.Query(c, `
		SELECT id, name, COALESCE(repeat_days, '{}'), start_time, created_at
		FROM routines
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, userID, p.Limit, p.Offset)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	routines := make([]Routine, 0, p.Limit)
	ids := make([]uuid.UUID, 0, p.Limit)
	for rows.Next() {
		var r Routine
		if err := rows.Scan(&r.ID, &r.Name, &r.RepeatDays, &r.StartTime, &r.CreatedAt); err != nil {
			respondDBError(c, err)
			return
		}
		r.Items = []RoutineItem{}
		routines = append(routines, r)
		ids = append(ids, r.ID)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	items, err := h.itemsFor(c, ids)
	if err != nil {
		respondDBError(c, err)
		return
	}
	for i := range routines {
		if it, ok := items[routines[i].ID]; ok {
			routines[i].Items = it
		}
	}

	response.OK(c, routines)
}

func (h *RoutineHandler) Get(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	r, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	items, _ := h.itemsFor(c, []uuid.UUID{id})
	r.Items = items[id]

	response.OK(c, r)
}

func (h *RoutineHandler) Create(c *gin.Context) {
	var req routineRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	userID := middleware.GetUserID(c)

	tx, err := h.db.Begin(c)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer tx.Rollback(c)

	var id uuid.UUID
	err = tx.QueryRow(c, `
		INSERT INTO routines (user_id, name, repeat_days, start_time)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, userID, req.Name, req.RepeatDays, req.StartTime).Scan(&id)
	if err != nil {
		respondDBError(c, err)
		return
	}

	for i, item := range req.Items {
		if _, err := tx.Exec(c, `
			INSERT INTO routine_items (routine_id, title, duration, sort_order)
			VALUES ($1, $2, $3, $4)
		`, id, item.Title, item.Duration, i); err != nil {
			respondDBError(c, err)
			return
		}
	}

	if err := tx.Commit(c); err != nil {
		respondDBError(c, err)
		return
	}

	r, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	items, _ := h.itemsFor(c, []uuid.UUID{id})
	r.Items = items[id]

	response.Created(c, r)
}

// Update replaces the routine's name, days, and full item list. Items are
// re-created with sort_order from their array position, so the client's order
// is authoritative.
func (h *RoutineHandler) Update(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}

	var req routineRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	userID := middleware.GetUserID(c)

	tx, err := h.db.Begin(c)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer tx.Rollback(c)

	tag, err := tx.Exec(c, `
		UPDATE routines SET name = $1, repeat_days = $2, start_time = $3
		WHERE id = $4 AND user_id = $5
	`, req.Name, req.RepeatDays, req.StartTime, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.NotFound(c)
		return
	}

	if _, err := tx.Exec(c, `DELETE FROM routine_items WHERE routine_id = $1`, id); err != nil {
		respondDBError(c, err)
		return
	}
	for i, item := range req.Items {
		if _, err := tx.Exec(c, `
			INSERT INTO routine_items (routine_id, title, duration, sort_order)
			VALUES ($1, $2, $3, $4)
		`, id, item.Title, item.Duration, i); err != nil {
			respondDBError(c, err)
			return
		}
	}

	if err := tx.Commit(c); err != nil {
		respondDBError(c, err)
		return
	}

	r, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	items, _ := h.itemsFor(c, []uuid.UUID{id})
	r.Items = items[id]

	response.OK(c, r)
}

func (h *RoutineHandler) Delete(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	tag, err := h.db.Exec(c, `DELETE FROM routines WHERE id = $1 AND user_id = $2`, id, userID)
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

type reorderRequest struct {
	ItemIDs []uuid.UUID `json:"item_ids" binding:"required,min=1"`
}

// Reorder rewrites sort_order to match the given item id order.
func (h *RoutineHandler) Reorder(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	var req reorderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	userID := middleware.GetUserID(c)

	// Ownership gate.
	var exists bool
	if err := h.db.QueryRow(c, `SELECT TRUE FROM routines WHERE id = $1 AND user_id = $2`, id, userID).Scan(&exists); err != nil {
		response.NotFound(c)
		return
	}

	tx, err := h.db.Begin(c)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer tx.Rollback(c)

	for i, itemID := range req.ItemIDs {
		tag, err := tx.Exec(c, `
			UPDATE routine_items SET sort_order = $1
			WHERE id = $2 AND routine_id = $3
		`, i, itemID, id)
		if err != nil {
			respondDBError(c, err)
			return
		}
		if tag.RowsAffected() == 0 {
			response.BadRequest(c, "item does not belong to this routine")
			return
		}
	}

	if err := tx.Commit(c); err != nil {
		respondDBError(c, err)
		return
	}

	r, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	items, _ := h.itemsFor(c, []uuid.UUID{id})
	r.Items = items[id]

	response.OK(c, r)
}

// ---- internals -------------------------------------------------------------

func (h *RoutineHandler) fetch(c *gin.Context, id, userID uuid.UUID) (Routine, error) {
	var r Routine
	err := h.db.QueryRow(c, `
		SELECT id, name, COALESCE(repeat_days, '{}'), start_time, created_at
		FROM routines WHERE id = $1 AND user_id = $2
	`, id, userID).Scan(&r.ID, &r.Name, &r.RepeatDays, &r.StartTime, &r.CreatedAt)
	if err != nil {
		return Routine{}, err
	}
	r.Items = []RoutineItem{}
	return r, nil
}

func (h *RoutineHandler) itemsFor(c *gin.Context, ids []uuid.UUID) (map[uuid.UUID][]RoutineItem, error) {
	out := make(map[uuid.UUID][]RoutineItem, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	rows, err := h.db.Query(c, `
		SELECT i.routine_id, i.id, i.title, i.duration, i.sort_order,
		       (c.id IS NOT NULL) AS completed_today
		FROM routine_items i
		LEFT JOIN routine_completions c
		  ON c.item_id = i.id AND c.completed_on = CURRENT_DATE
		WHERE i.routine_id = ANY($1)
		ORDER BY i.routine_id, i.sort_order
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var rid uuid.UUID
		var it RoutineItem
		if err := rows.Scan(&rid, &it.ID, &it.Title, &it.Duration, &it.SortOrder, &it.CompletedToday); err != nil {
			return nil, err
		}
		out[rid] = append(out[rid], it)
	}
	return out, rows.Err()
}

// ToggleItem flips today's completion for one routine item. POST /routines/:id/items/:itemId/toggle.
func (h *RoutineHandler) ToggleItem(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	itemID, ok := paramUUID(c, "itemId")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	// Ownership gate: the item must belong to a routine owned by the caller.
	var belongs bool
	err := h.db.QueryRow(c, `
		SELECT EXISTS (
			SELECT 1 FROM routine_items i
			JOIN routines r ON r.id = i.routine_id
			WHERE i.id = $1 AND i.routine_id = $2 AND r.user_id = $3
		)
	`, itemID, id, userID).Scan(&belongs)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if !belongs {
		response.NotFound(c)
		return
	}

	// Toggle: delete today's completion if present, else insert it.
	tag, err := h.db.Exec(c, `
		DELETE FROM routine_completions
		WHERE routine_id = $1 AND item_id = $2 AND completed_on = CURRENT_DATE
	`, id, itemID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		if _, err := h.db.Exec(c, `
			INSERT INTO routine_completions (routine_id, item_id, completed_on)
			VALUES ($1, $2, CURRENT_DATE)
			ON CONFLICT (routine_id, item_id, completed_on) DO NOTHING
		`, id, itemID); err != nil {
			respondDBError(c, err)
			return
		}
	}

	// Return the full routine so the client can re-render in one round-trip.
	r, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	items, _ := h.itemsFor(c, []uuid.UUID{id})
	r.Items = items[id]

	response.OK(c, r)
}
