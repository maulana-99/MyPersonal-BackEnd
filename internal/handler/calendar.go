package handler

import (
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CalendarHandler returns schedules that overlap a requested window.
type CalendarHandler struct {
	db *pgxpool.Pool
}

func NewCalendarHandler(db *pgxpool.Pool) *CalendarHandler {
	return &CalendarHandler{db: db}
}

var validViews = map[string]bool{"day": true, "week": true, "month": true}

// List: GET /calendar?start=&end=&view=day|week|month
//
// Overlap semantics: a schedule is included when it starts before the window
// ends AND ends after the window starts. Schedules with no end_time are
// treated as instantaneous at start_time.
func (h *CalendarHandler) List(c *gin.Context) {
	userID := middleware.GetUserID(c)

	view := c.DefaultQuery("view", "week")
	if !validViews[view] {
		response.BadRequest(c, "view must be one of: day, week, month")
		return
	}

	start, err := time.Parse(time.RFC3339, c.Query("start"))
	if err != nil {
		response.BadRequest(c, "invalid start: must be RFC3339")
		return
	}
	end, err := time.Parse(time.RFC3339, c.Query("end"))
	if err != nil {
		response.BadRequest(c, "invalid end: must be RFC3339")
		return
	}
	if !end.After(start) {
		response.BadRequest(c, "end must be after start")
		return
	}

	// Union of one-time schedules and materialised occurrences. A recurring
	// schedule's own row is excluded when it has occurrences so the base
	// schedule is not double-counted.
	rows, err := h.db.Query(c, `
		SELECT s.id, NULL::uuid AS occurrence_id, s.category_id, s.title,
		       COALESCE(s.description, ''), s.start_time, s.end_time, s.all_day,
		       s.status, COALESCE(r.rule, ''), s.created_at, s.updated_at,
		       s.source, s.google_event_id, s.google_sync_state, s.google_sync_error,
			       s.location, s.color_id, s.meet_link, s.tz
		FROM schedules s
		LEFT JOIN schedule_recurrences r ON r.schedule_id = s.id
		WHERE s.user_id = $1
		  AND r.schedule_id IS NULL
		  AND s.start_time < $3
		  AND COALESCE(s.end_time, s.start_time) > $2

		UNION ALL

		SELECT s.id, o.id AS occurrence_id, s.category_id, s.title,
		       COALESCE(s.description, ''), o.occurrence_start, o.occurrence_end, s.all_day,
		       o.status, COALESCE(r.rule, ''), s.created_at, s.updated_at,
		       s.source, s.google_event_id, s.google_sync_state, s.google_sync_error,
			       s.location, s.color_id, s.meet_link, s.tz
		FROM schedule_occurrences o
		JOIN schedules s ON s.id = o.schedule_id
		JOIN schedule_recurrences r ON r.schedule_id = s.id
		WHERE s.user_id = $1
		  AND o.occurrence_start < $3
		  AND COALESCE(o.occurrence_end, o.occurrence_start) > $2

		ORDER BY 6 ASC
		LIMIT 500
	`, userID, start, end)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	now := time.Now()
	schedules := make([]Schedule, 0, 32)
	for rows.Next() {
		s, err := scanScheduleRow(rows, now)
		if err != nil {
			respondDBError(c, err)
			return
		}
		schedules = append(schedules, s)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	reminderIDs := uniqueScheduleIDs(schedules)
	reminders, err := remindersForSchedules(c, h.db, reminderIDs)
	if err != nil {
		respondDBError(c, err)
		return
	}
	for i := range schedules {
		schedules[i].Reminders = reminders[schedules[i].ID]
	}

	if err := attachAttendees(c, h.db, schedules); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, gin.H{
		"view":      view,
		"start":     start,
		"end":       end,
		"schedules": schedules,
	})
}
