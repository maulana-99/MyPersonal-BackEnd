package handler

import (
	"strings"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SearchHandler serves one global search across the app's searchable domains.
type SearchHandler struct {
	db *pgxpool.Pool
}

func NewSearchHandler(db *pgxpool.Pool) *SearchHandler {
	return &SearchHandler{db: db}
}

// SearchResult is the uniform row every source projects onto, so the client
// renders one list regardless of where a hit came from.
type SearchResult struct {
	Type       string     `json:"type"` // schedule|task|habit|routine|note
	ID         uuid.UUID  `json:"id"`
	Title      string     `json:"title"`
	Snippet    string     `json:"snippet"`
	Date       *time.Time `json:"date"`
	CategoryID *uuid.UUID `json:"category_id"`
	Status     string     `json:"status"` // stored status; schedules are not computed here
}

var searchableTypes = map[string]bool{
	"schedule": true, "task": true, "habit": true, "routine": true, "note": true,
}

// searchStatuses is every value the status filter can meaningfully match:
// schedule outcomes plus the task states. Habits, routines, and notes carry no
// status, so a status filter excludes them rather than silently ignoring it.
var searchStatuses = map[string]bool{
	"upcoming": true, "ongoing": true, "completed": true,
	"missed": true, "skipped": true, "pending": true,
}

// Search: GET /search?q=&type=&status=&from=&to=&category_id=
//
// One UNION ALL, one round trip, an outer ORDER BY + LIMIT. Every branch filters
// its own user_id — ownership is never inferred through a join. Matching is a
// literal, case-insensitive substring test (`position`), so `%` and `_` in the
// user's text are ordinary characters, not wildcards.
func (h *SearchHandler) Search(c *gin.Context) {
	userID := middleware.GetUserID(c)
	p := pagination(c)

	q := strings.TrimSpace(c.Query("q"))
	if len([]rune(q)) < 2 {
		response.BadRequest(c, "q must be at least 2 characters")
		return
	}

	var kind *string
	if raw := c.Query("type"); raw != "" {
		if !searchableTypes[raw] {
			response.BadRequest(c, "type must be one of: schedule, task, habit, routine, note")
			return
		}
		kind = &raw
	}

	var status *string
	if raw := c.Query("status"); raw != "" {
		if !searchStatuses[raw] {
			response.BadRequest(c, "status must be one of: upcoming, ongoing, completed, missed, skipped, pending")
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

	from, ok := optionalTime(c, "from")
	if !ok {
		return
	}
	to, ok := optionalTime(c, "to")
	if !ok {
		return
	}

	// Every branch projects the same column list and types (NULL::text etc.),
	// otherwise Postgres rejects the UNION outright.
	rows, err := h.db.Query(c, `
		WITH hits AS (
			SELECT 'schedule'::text AS type, s.id, s.title,
			       COALESCE(s.description, '') AS snippet,
			       s.start_time AS happened_at, s.category_id,
			       `+scheduleStatusSQL+` AS status
			FROM schedules s
			WHERE s.user_id = $1
			  AND ($2::text IS NULL OR $2 = 'schedule')
			  AND ($4::timestamptz IS NULL OR s.start_time >= $4)
			  AND ($5::timestamptz IS NULL OR s.start_time <= $5)
			  AND ($6::uuid IS NULL OR s.category_id = $6)
			  AND ($7::text IS NULL OR `+scheduleStatusSQL+` = $7)
			  AND (position(lower($3) IN lower(s.title)) > 0
			       OR position(lower($3) IN lower(COALESCE(s.description, ''))) > 0)

			UNION ALL

			SELECT 'task', t.id, t.title, COALESCE(t.description, ''),
			       t.updated_at, t.category_id, t.status
			FROM tasks t
			WHERE t.user_id = $1
			  AND ($2::text IS NULL OR $2 = 'task')
			  AND ($6::uuid IS NULL OR t.category_id = $6)
			  AND ($7::text IS NULL OR t.status = $7)
			  AND (position(lower($3) IN lower(t.title)) > 0
			       OR position(lower($3) IN lower(COALESCE(t.description, ''))) > 0)

			UNION ALL

			-- Habits, routines, and notes have no status: a status filter
			-- excludes them instead of quietly ignoring the filter.
			SELECT 'habit', hb.id, hb.name, COALESCE(hb.description, ''),
			       hb.updated_at, hb.category_id, ''
			FROM habits hb
			WHERE hb.user_id = $1
			  AND ($2::text IS NULL OR $2 = 'habit')
			  AND ($6::uuid IS NULL OR hb.category_id = $6)
			  AND $7::text IS NULL
			  AND (position(lower($3) IN lower(hb.name)) > 0
			       OR position(lower($3) IN lower(COALESCE(hb.description, ''))) > 0)

			UNION ALL

			SELECT 'routine', r.id, r.name, '',
			       r.created_at, NULL::uuid, ''
			FROM routines r
			WHERE r.user_id = $1
			  AND ($2::text IS NULL OR $2 = 'routine')
			  AND $7::text IS NULL
			  AND position(lower($3) IN lower(r.name)) > 0

			UNION ALL

			SELECT 'note', n.id, n.title, COALESCE(n.body, ''),
			       n.updated_at, NULL::uuid, ''
			FROM notes n
			WHERE n.user_id = $1
			  AND ($2::text IS NULL OR $2 = 'note')
			  AND $7::text IS NULL
			  AND (position(lower($3) IN lower(n.title)) > 0
			       OR position(lower($3) IN lower(COALESCE(n.body, ''))) > 0)
		)
		SELECT type, id, title, snippet, happened_at, category_id, status
		FROM hits
		ORDER BY happened_at DESC NULLS LAST
		LIMIT $8 OFFSET $9
	`, userID, kind, q, from, to, categoryID, status, p.Limit, p.Offset)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	results := make([]SearchResult, 0, p.Limit)
	for rows.Next() {
		var r SearchResult
		var happened *time.Time
		if err := rows.Scan(&r.Type, &r.ID, &r.Title, &r.Snippet, &happened, &r.CategoryID, &r.Status); err != nil {
			respondDBError(c, err)
			return
		}
		r.Date = happened
		r.Snippet = truncateSnippet(r.Snippet, q)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, results)
}

// ---- internals -------------------------------------------------------------

// truncateSnippet keeps a result row scannable: a window around the first match,
// clamped to a phrase-sized excerpt.
func truncateSnippet(text, needle string) string {
	const window = 140
	if len(text) <= window {
		return text
	}
	idx := strings.Index(strings.ToLower(text), strings.ToLower(needle))
	if idx < 0 {
		return text[:window] + "…"
	}
	start := idx - 40
	if start < 0 {
		start = 0
	}
	end := start + window
	if end > len(text) {
		end = len(text)
	}
	out := text[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(text) {
		out += "…"
	}
	return out
}
