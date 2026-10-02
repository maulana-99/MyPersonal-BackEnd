package handler

import (
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReviewHandler owns the end-of-day review: one free-text entry per local day,
// plus a derived summary of what actually happened that day.
//
// The summary is computed on read and never persisted — `content` holds the
// user's own prose only, so a summary bug can never overwrite their writing.
type ReviewHandler struct {
	db *pgxpool.Pool
}

func NewReviewHandler(db *pgxpool.Pool) *ReviewHandler {
	return &ReviewHandler{db: db}
}

type ReviewSummary struct {
	Date               string `json:"date"`
	SchedulesTotal     int    `json:"schedules_total"`
	SchedulesCompleted int    `json:"schedules_completed"`
	SchedulesMissed    int    `json:"schedules_missed"`
	TasksCompleted     int    `json:"tasks_completed"`
	FocusMinutes       int    `json:"focus_minutes"`
	HabitsDue          int    `json:"habits_due"`
	HabitsDone         int    `json:"habits_done"`
	// Tomorrow is the day after the reviewed date (Plan.md §2.12). A review is
	// written at the end of a day, so its most useful forward-looking part is
	// what is already booked next.
	Tomorrow []ReviewSchedule `json:"tomorrow"`
}

// ReviewSchedule is the minimal shape the review needs to preview a day.
type ReviewSchedule struct {
	ID        uuid.UUID  `json:"id"`
	Title     string     `json:"title"`
	StartTime time.Time  `json:"start_time"`
	EndTime   *time.Time `json:"end_time"`
	AllDay    bool       `json:"all_day"`
}

type DailyReview struct {
	Date      string        `json:"date"`
	Content   string        `json:"content"`
	Summary   ReviewSummary `json:"summary"`
	CreatedAt *time.Time    `json:"created_at"`
	UpdatedAt *time.Time    `json:"updated_at"`
}

type reviewRequest struct {
	Content string `json:"content" binding:"max=20000"`
}

// Get: GET /daily-reviews/:date — the stored review (possibly empty) plus the
// derived summary for that day.
func (h *ReviewHandler) Get(c *gin.Context) {
	date, ok := paramDate(c, "date")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	out := DailyReview{Date: date}

	var content string
	var created, updated time.Time
	err := h.db.QueryRow(c, `
		SELECT content, created_at, updated_at
		FROM daily_reviews WHERE user_id = $1 AND review_date = $2::date
	`, userID, date).Scan(&content, &created, &updated)
	if err == nil {
		out.Content = content
		out.CreatedAt = &created
		out.UpdatedAt = &updated
	}

	summary, err := h.summarise(c, userID, date)
	if err != nil {
		respondDBError(c, err)
		return
	}
	out.Summary = summary

	response.OK(c, out)
}

// Upsert: PUT /daily-reviews/:date
//
// A single INSERT ... ON CONFLICT: two tabs saving the same day cannot race
// into a duplicate-key error the way a read-then-write would.
func (h *ReviewHandler) Upsert(c *gin.Context) {
	date, ok := paramDate(c, "date")
	if !ok {
		return
	}

	var req reviewRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	userID := middleware.GetUserID(c)

	var created, updated time.Time
	err := h.db.QueryRow(c, `
		INSERT INTO daily_reviews (user_id, review_date, content)
		VALUES ($1, $2::date, $3)
		ON CONFLICT (user_id, review_date)
		DO UPDATE SET content = EXCLUDED.content, updated_at = NOW()
		RETURNING created_at, updated_at
	`, userID, date, req.Content).Scan(&created, &updated)
	if err != nil {
		respondDBError(c, err)
		return
	}

	summary, err := h.summarise(c, userID, date)
	if err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, DailyReview{
		Date: date, Content: req.Content, Summary: summary,
		CreatedAt: &created, UpdatedAt: &updated,
	})
}

// List: GET /daily-reviews?from=&to= — written reviews only, newest first.
func (h *ReviewHandler) List(c *gin.Context) {
	userID := middleware.GetUserID(c)
	p := pagination(c)

	today := todayLocal()
	from := c.DefaultQuery("from", today.AddDate(0, 0, -30).Format(dayFormat))
	to := c.DefaultQuery("to", today.Format(dayFormat))
	if _, ok := resolveDay(c, from); !ok {
		return
	}
	if _, ok := resolveDay(c, to); !ok {
		return
	}

	rows, err := h.db.Query(c, `
		SELECT to_char(review_date, 'YYYY-MM-DD'), content, created_at, updated_at
		FROM daily_reviews
		WHERE user_id = $1 AND review_date BETWEEN $2::date AND $3::date
		ORDER BY review_date DESC
		LIMIT $4 OFFSET $5
	`, userID, from, to, p.Limit, p.Offset)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	out := make([]DailyReview, 0, p.Limit)
	for rows.Next() {
		var r DailyReview
		var created, updated time.Time
		if err := rows.Scan(&r.Date, &r.Content, &created, &updated); err != nil {
			respondDBError(c, err)
			return
		}
		r.CreatedAt, r.UpdatedAt = &created, &updated
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, out)
}

// ---- internals -------------------------------------------------------------

// summarise aggregates one local day from four tables. Each metric is one
// indexed query over a bounded window — never one query per day or per habit.
func (h *ReviewHandler) summarise(c *gin.Context, userID uuid.UUID, date string) (ReviewSummary, error) {
	day, err := time.Parse(dayFormat, date)
	if err != nil {
		return ReviewSummary{}, err
	}
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.Local)
	end := start.AddDate(0, 0, 1)

	sum := ReviewSummary{Date: date}

	err = h.db.QueryRow(c, `
		SELECT
			COUNT(*) FILTER (WHERE status != 'skipped'),
			COUNT(*) FILTER (WHERE status = 'completed'),
			COUNT(*) FILTER (
				WHERE status NOT IN ('completed', 'skipped')
				  AND COALESCE(end_time, start_time) < $3
			)
		FROM schedules
		WHERE user_id = $1
		  AND start_time >= $2 AND start_time < $3
	`, userID, start, end).Scan(&sum.SchedulesTotal, &sum.SchedulesCompleted, &sum.SchedulesMissed)
	if err != nil {
		return ReviewSummary{}, err
	}

	err = h.db.QueryRow(c, `
		SELECT COUNT(*)
		FROM tasks
		WHERE user_id = $1 AND status = 'completed'
		  AND updated_at >= $2 AND updated_at < $3
	`, userID, start, end).Scan(&sum.TasksCompleted)
	if err != nil {
		return ReviewSummary{}, err
	}

	err = h.db.QueryRow(c, `
		SELECT COALESCE(SUM(actual_minutes), 0)::int
		FROM focus_sessions
		WHERE user_id = $1 AND status = 'completed'
		  AND started_at >= $2 AND started_at < $3
	`, userID, start, end).Scan(&sum.FocusMinutes)
	if err != nil {
		return ReviewSummary{}, err
	}

	// Habits scheduled that weekday, and how many met their target that day.
	// One pass over the user's habits with the day's log joined in.
	err = h.db.QueryRow(c, `
		SELECT
			COALESCE(SUM(CASE WHEN scheduled THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN scheduled AND COALESCE(l.count, 0) >= h.target_count THEN 1 ELSE 0 END), 0)
		FROM (
			SELECT h.*, (
				h.target_type = 'weekly'
				OR COALESCE(array_length(h.repeat_days, 1), 0) = 0
				OR trim(to_char($2::date, 'dy')) = ANY(h.repeat_days)
			) AS scheduled
			FROM habits h
			WHERE h.user_id = $1
		) h
		LEFT JOIN habit_logs l ON l.habit_id = h.id AND l.log_date = $2::date
	`, userID, date).Scan(&sum.HabitsDue, &sum.HabitsDone)
	if err != nil {
		return ReviewSummary{}, err
	}

	// Tomorrow's commitments, so an end-of-day review can look forward. One
	// query over the next local day; recurring occurrences are unioned in the
	// same way Today does it so a recurring schedule is not missed.
	tomorrowStart := end
	tomorrowEnd := end.AddDate(0, 0, 1)

	rows, err := h.db.Query(c, `
		SELECT s.id, s.title, s.start_time, s.end_time, s.all_day
		FROM schedules s
		LEFT JOIN schedule_recurrences r ON r.schedule_id = s.id
		WHERE s.user_id = $1
		  AND r.schedule_id IS NULL
		  AND s.start_time < $3
		  AND COALESCE(s.end_time, s.start_time) >= $2

		UNION ALL

		SELECT s.id, s.title, o.occurrence_start, o.occurrence_end, s.all_day
		FROM schedule_occurrences o
		JOIN schedules s ON s.id = o.schedule_id
		JOIN schedule_recurrences r ON r.schedule_id = s.id
		WHERE s.user_id = $1
		  AND o.occurrence_start < $3
		  AND COALESCE(o.occurrence_end, o.occurrence_start) >= $2

		ORDER BY 3 ASC
		LIMIT 50
	`, userID, tomorrowStart, tomorrowEnd)
	if err != nil {
		return ReviewSummary{}, err
	}
	defer rows.Close()

	sum.Tomorrow = make([]ReviewSchedule, 0, 8)
	for rows.Next() {
		var rs ReviewSchedule
		if err := rows.Scan(&rs.ID, &rs.Title, &rs.StartTime, &rs.EndTime, &rs.AllDay); err != nil {
			return ReviewSummary{}, err
		}
		sum.Tomorrow = append(sum.Tomorrow, rs)
	}
	if err := rows.Err(); err != nil {
		return ReviewSummary{}, err
	}

	return sum, nil
}

// paramDate reads a YYYY-MM-DD path parameter, writing 400 on failure.
func paramDate(c *gin.Context, name string) (string, bool) {
	raw := c.Param(name)
	if _, err := time.Parse(dayFormat, raw); err != nil {
		response.BadRequest(c, name+" must be YYYY-MM-DD")
		return "", false
	}
	return raw, true
}
