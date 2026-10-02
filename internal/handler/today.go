package handler

import (
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TodayHandler aggregates everything the Today view needs in one round-trip.
type TodayHandler struct {
	db *pgxpool.Pool
}

func NewTodayHandler(db *pgxpool.Pool) *TodayHandler {
	return &TodayHandler{db: db}
}

type TodayTask struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Priority string  `json:"priority"`
	Status   string  `json:"status"`
	DueDate  *string `json:"due_date"`
}

type TodayProgress struct {
	Total      int `json:"total"`
	Completed  int `json:"completed"`
	Percentage int `json:"percentage"`
}

type TodayResponse struct {
	Schedules struct {
		Upcoming  []Schedule `json:"upcoming"`
		Ongoing   []Schedule `json:"ongoing"`
		Completed []Schedule `json:"completed"`
		Missed    []Schedule `json:"missed"`
	} `json:"schedules"`
	Tasks        []TodayTask   `json:"tasks"`
	Progress     TodayProgress `json:"progress"`
	NextUpcoming *Schedule     `json:"next_upcoming"`
}

// Get: GET /today — schedules bucketed by computed status for the local day,
// plus today's tasks and a completion progress figure.
func (h *TodayHandler) Get(c *gin.Context) {
	userID := middleware.GetUserID(c)

	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	dayEnd := dayStart.Add(24 * time.Hour)

	resp := TodayResponse{}
	resp.Schedules.Upcoming = []Schedule{}
	resp.Schedules.Ongoing = []Schedule{}
	resp.Schedules.Completed = []Schedule{}
	resp.Schedules.Missed = []Schedule{}
	resp.Tasks = []TodayTask{}

	rows, err := h.db.Query(c, `
		SELECT s.id, NULL::uuid AS occurrence_id, s.category_id, s.title,
		       COALESCE(s.description, ''), s.start_time, s.end_time, s.all_day,
		       s.status, COALESCE(r.rule, ''), s.created_at, s.updated_at
		FROM schedules s
		LEFT JOIN schedule_recurrences r ON r.schedule_id = s.id
		WHERE s.user_id = $1
		  AND r.schedule_id IS NULL
		  AND s.start_time < $3
		  AND COALESCE(s.end_time, s.start_time) >= $2

		UNION ALL

		SELECT s.id, o.id AS occurrence_id, s.category_id, s.title,
		       COALESCE(s.description, ''), o.occurrence_start, o.occurrence_end, s.all_day,
		       o.status, COALESCE(r.rule, ''), s.created_at, s.updated_at
		FROM schedule_occurrences o
		JOIN schedules s ON s.id = o.schedule_id
		JOIN schedule_recurrences r ON r.schedule_id = s.id
		WHERE s.user_id = $1
		  AND o.occurrence_start < $3
		  AND COALESCE(o.occurrence_end, o.occurrence_start) >= $2

		ORDER BY 6 ASC
		LIMIT 200
	`, userID, dayStart, dayEnd)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		s, err := scanScheduleRow(rows, now)
		if err != nil {
			respondDBError(c, err)
			return
		}
		switch s.Status {
		case StatusOngoing:
			resp.Schedules.Ongoing = append(resp.Schedules.Ongoing, s)
		case StatusCompleted:
			resp.Schedules.Completed = append(resp.Schedules.Completed, s)
		case StatusMissed:
			resp.Schedules.Missed = append(resp.Schedules.Missed, s)
		default:
			resp.Schedules.Upcoming = append(resp.Schedules.Upcoming, s)
		}
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	// Progress counts today's non-skipped schedules.
	total := len(resp.Schedules.Upcoming) + len(resp.Schedules.Ongoing) +
		len(resp.Schedules.Completed) + len(resp.Schedules.Missed)
	resp.Progress = TodayProgress{
		Total:     total,
		Completed: len(resp.Schedules.Completed),
	}
	if total > 0 {
		resp.Progress.Percentage = (resp.Progress.Completed * 100) / total
	}

	// First upcoming schedule, if any.
	if len(resp.Schedules.Upcoming) > 0 {
		next := resp.Schedules.Upcoming[0]
		resp.NextUpcoming = &next
	}

	// Today's tasks (due today or overdue-but-pending, plus undated pending).
	taskRows, err := h.db.Query(c, `
		SELECT id, title, priority, status, due_date
		FROM tasks
		WHERE user_id = $1
		  AND (status = 'pending' OR (due_date IS NOT NULL AND due_date = $2::date))
		ORDER BY (due_date IS NULL), due_date ASC, priority DESC
		LIMIT 100
	`, userID, dayStart)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer taskRows.Close()

	for taskRows.Next() {
		var t TodayTask
		var due *time.Time
		if err := taskRows.Scan(&t.ID, &t.Title, &t.Priority, &t.Status, &due); err != nil {
			respondDBError(c, err)
			return
		}
		if due != nil {
			s := due.Format("2006-01-02")
			t.DueDate = &s
		}
		resp.Tasks = append(resp.Tasks, t)
	}
	if err := taskRows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, resp)
}
