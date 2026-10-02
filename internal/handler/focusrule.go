package handler

import (
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FocusRuleHandler owns the web half of Productivity Mode: named time windows
// the user wants protected. V2 stores and displays them; blocking a site is a
// desktop/extension concern (Plan.md §3, V3), so `sites` is carried, not enforced.
type FocusRuleHandler struct {
	db *pgxpool.Pool
}

func NewFocusRuleHandler(db *pgxpool.Pool) *FocusRuleHandler {
	return &FocusRuleHandler{db: db}
}

type FocusRule struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	StartTime string    `json:"start_time"` // "HH:MM"
	EndTime   string    `json:"end_time"`   // "HH:MM"
	Days      []string  `json:"days"`
	Sites     []string  `json:"sites"`
	Enabled   bool      `json:"enabled"`
	ActiveNow bool      `json:"active_now"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type focusRuleRequest struct {
	Name      string   `json:"name"       binding:"required,max=100"`
	StartTime string   `json:"start_time" binding:"required"`
	EndTime   string   `json:"end_time"   binding:"required"`
	Days      []string `json:"days"       binding:"omitempty,dive,oneof=mon tue wed thu fri sat sun"`
	Sites     []string `json:"sites"      binding:"omitempty,max=50,dive,max=253"`
	Enabled   *bool    `json:"enabled"`
}

// maxFocusRules bounds a rule set the UI shows as one list.
const maxFocusRules = 50

const focusRuleColumns = `id, name, to_char(start_time, 'HH24:MI'), to_char(end_time, 'HH24:MI'),
	COALESCE(days, '{}'), COALESCE(sites, '{}'), enabled, created_at, updated_at`

func scanFocusRule(row rowScanner) (FocusRule, error) {
	var r FocusRule
	err := row.Scan(&r.ID, &r.Name, &r.StartTime, &r.EndTime, &r.Days, &r.Sites,
		&r.Enabled, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return FocusRule{}, err
	}
	return r, nil
}

// List: GET /focus-rules
func (h *FocusRuleHandler) List(c *gin.Context) {
	userID := middleware.GetUserID(c)
	p := pagination(c)

	rows, err := h.db.Query(c, `
		SELECT `+focusRuleColumns+`
		FROM focus_rules
		WHERE user_id = $1
		ORDER BY start_time ASC
		LIMIT $2 OFFSET $3
	`, userID, p.Limit, p.Offset)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	rules := make([]FocusRule, 0, p.Limit)
	for rows.Next() {
		r, err := scanFocusRule(rows)
		if err != nil {
			respondDBError(c, err)
			return
		}
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	markActiveRules(rules, time.Now())

	response.OK(c, rules)
}

// Active: GET /focus-rules/active — the enabled rules covering the current
// local time, so the UI can show "Focus Mode is active" without client math.
func (h *FocusRuleHandler) Active(c *gin.Context) {
	userID := middleware.GetUserID(c)

	rules, err := h.all(c, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	markActiveRules(rules, time.Now())

	active := make([]FocusRule, 0, len(rules))
	for _, r := range rules {
		if r.ActiveNow {
			active = append(active, r)
		}
	}

	response.OK(c, active)
}

// Get: GET /focus-rules/:id
func (h *FocusRuleHandler) Get(c *gin.Context) {
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
	response.OK(c, r)
}

func (h *FocusRuleHandler) Create(c *gin.Context) {
	var req focusRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if !validRuleWindow(c, &req) {
		return
	}

	userID := middleware.GetUserID(c)

	var count int
	if err := h.db.QueryRow(c, `SELECT COUNT(*)::int FROM focus_rules WHERE user_id = $1`, userID).Scan(&count); err != nil {
		respondDBError(c, err)
		return
	}
	if count >= maxFocusRules {
		response.BadRequest(c, "too many focus rules")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	days := req.Days
	if days == nil {
		days = []string{}
	}
	sites := req.Sites
	if sites == nil {
		sites = []string{}
	}

	var id uuid.UUID
	err := h.db.QueryRow(c, `
		INSERT INTO focus_rules (user_id, name, start_time, end_time, days, sites, enabled)
		VALUES ($1, $2, $3::time, $4::time, $5, $6, $7)
		RETURNING id
	`, userID, req.Name, req.StartTime, req.EndTime, days, sites, enabled).Scan(&id)
	if err != nil {
		respondDBError(c, err)
		return
	}

	r, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	response.Created(c, r)
}

func (h *FocusRuleHandler) Update(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}

	var req focusRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if !validRuleWindow(c, &req) {
		return
	}

	userID := middleware.GetUserID(c)

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	days := req.Days
	if days == nil {
		days = []string{}
	}
	sites := req.Sites
	if sites == nil {
		sites = []string{}
	}

	tag, err := h.db.Exec(c, `
		UPDATE focus_rules
		SET name = $1, start_time = $2::time, end_time = $3::time,
		    days = $4, sites = $5, enabled = $6, updated_at = NOW()
		WHERE id = $7 AND user_id = $8
	`, req.Name, req.StartTime, req.EndTime, days, sites, enabled, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.NotFound(c)
		return
	}

	r, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	response.OK(c, r)
}

func (h *FocusRuleHandler) Delete(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	tag, err := h.db.Exec(c, `DELETE FROM focus_rules WHERE id = $1 AND user_id = $2`, id, userID)
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

func (h *FocusRuleHandler) all(c *gin.Context, userID uuid.UUID) ([]FocusRule, error) {
	rows, err := h.db.Query(c, `
		SELECT `+focusRuleColumns+`
		FROM focus_rules WHERE user_id = $1 ORDER BY start_time ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	rules := make([]FocusRule, 0, 8)
	for rows.Next() {
		r, err := scanFocusRule(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, r)
	}
	return rules, rows.Err()
}

func (h *FocusRuleHandler) fetch(c *gin.Context, id, userID uuid.UUID) (FocusRule, error) {
	row := h.db.QueryRow(c, `
		SELECT `+focusRuleColumns+`
		FROM focus_rules WHERE id = $1 AND user_id = $2
	`, id, userID)
	r, err := scanFocusRule(row)
	if err != nil {
		return FocusRule{}, err
	}
	single := []FocusRule{r}
	markActiveRules(single, time.Now())
	return single[0], nil
}

// markActiveRules sets ActiveNow from the server clock and the rule's own
// wall-clock window. An empty `days` means every day.
func markActiveRules(rules []FocusRule, now time.Time) {
	weekday := weekdayKey(now)
	hhmm := now.Format("15:04")

	for i := range rules {
		r := &rules[i]
		if !r.Enabled {
			continue
		}
		if len(r.Days) > 0 {
			found := false
			for _, d := range r.Days {
				if d == weekday {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		if r.StartTime <= hhmm && hhmm < r.EndTime {
			r.ActiveNow = true
		}
	}
}

func validRuleWindow(c *gin.Context, req *focusRuleRequest) bool {
	start, err := time.Parse("15:04", req.StartTime)
	if err != nil {
		response.BadRequest(c, "start_time must be HH:MM")
		return false
	}
	end, err := time.Parse("15:04", req.EndTime)
	if err != nil {
		response.BadRequest(c, "end_time must be HH:MM")
		return false
	}
	if !end.After(start) {
		response.BadRequest(c, "end_time must be after start_time (overnight windows are not supported)")
		return false
	}
	return true
}
