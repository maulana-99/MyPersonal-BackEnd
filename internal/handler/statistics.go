package handler

import (
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// StatisticsHandler aggregates basic productivity metrics. Habit and focus
// metrics are V2 and intentionally reported as zero.
type StatisticsHandler struct {
	db *pgxpool.Pool
}

func NewStatisticsHandler(db *pgxpool.Pool) *StatisticsHandler {
	return &StatisticsHandler{db: db}
}

type Statistics struct {
	Period                 string      `json:"period"`
	TotalSchedules         int         `json:"total_schedules"`
	CompletedSchedules     int         `json:"completed_schedules"`
	MissedSchedules        int         `json:"missed_schedules"`
	ScheduleCompletionRate int         `json:"schedule_completion_rate"` // percent
	CompletedTasks         int         `json:"completed_tasks"`
	FocusTime              int         `json:"focus_time"`     // minutes of completed focus sessions
	FocusSessions          int         `json:"focus_sessions"` // completed session count
	HabitsDone             int         `json:"habits_done"`    // habit-days meeting target
	HabitsDue              int         `json:"habits_due"`     // habit-days scheduled
	HabitCompletionRate    int         `json:"habit_completion_rate"`
	DailyProductivity      int         `json:"daily_productivity"`
	WeeklyProductivity     int         `json:"weekly_productivity"`
	Daily                  []DailyStat `json:"daily"`
}

// DailyStat is one local day inside the statistics window, for the breakdown
// chart. Days with no activity are present with zeroes so the series is gapless.
type DailyStat struct {
	Date               string `json:"date"`
	SchedulesCompleted int    `json:"schedules_completed"`
	TasksCompleted     int    `json:"tasks_completed"`
	FocusMinutes       int    `json:"focus_minutes"`
	HabitsDone         int    `json:"habits_done"`
}

// Get: GET /statistics?period=day|week
//
// Completion rate counts schedules whose computed outcome is completed over
// every non-skipped schedule in the window. Missed schedules are those past
// their end without being marked complete.
func (h *StatisticsHandler) Get(c *gin.Context) {
	userID := middleware.GetUserID(c)

	period := c.DefaultQuery("period", "week")
	window := "7 days"
	if period == "day" {
		window = "1 day"
	} else if period != "week" {
		response.BadRequest(c, "period must be day or week")
		return
	}

	var stats Statistics
	stats.Period = period
	stats.Daily = []DailyStat{}

	err := h.db.QueryRow(c, `
		SELECT
			COUNT(*) FILTER (WHERE status != 'skipped'),
			COUNT(*) FILTER (WHERE status = 'completed'),
			COUNT(*) FILTER (
				WHERE status NOT IN ('completed', 'skipped')
				  AND COALESCE(end_time, start_time) < NOW()
			)
		FROM schedules
		WHERE user_id = $1
		  AND start_time >= NOW() - $2::interval
	`, userID, window).Scan(
		&stats.TotalSchedules, &stats.CompletedSchedules, &stats.MissedSchedules,
	)
	if err != nil {
		respondDBError(c, err)
		return
	}

	if stats.TotalSchedules > 0 {
		stats.ScheduleCompletionRate = (stats.CompletedSchedules * 100) / stats.TotalSchedules
	}

	// Tasks and focus time in one round trip: both are independent aggregates
	// over the same window, so there is no reason to pay for a second query.
	err = h.db.QueryRow(c, `
		SELECT
			(SELECT COUNT(*)::int FROM tasks
			  WHERE user_id = $1 AND status = 'completed'
			    AND updated_at >= NOW() - $2::interval),
			(SELECT COALESCE(SUM(actual_minutes), 0)::int FROM focus_sessions
			  WHERE user_id = $1 AND status = 'completed'
			    AND started_at >= NOW() - $2::interval),
			(SELECT COUNT(*)::int FROM focus_sessions
			  WHERE user_id = $1 AND status = 'completed'
			    AND started_at >= NOW() - $2::interval)
	`, userID, window).Scan(&stats.CompletedTasks, &stats.FocusTime, &stats.FocusSessions)
	if err != nil {
		respondDBError(c, err)
		return
	}

	// Habit completion over the window: due habit-days vs met habit-days.
	// One pass with generate_series so a day with no log is still counted due.
	//
	// The day bounds are Go-local values, never SQL `NOW()::date`: the DB session
	// runs UTC, so between 00:00 and 07:00 WIB `NOW()::date` is still yesterday
	// and every habit-day would be attributed to the wrong date.
	dayCount := 7
	if period == "day" {
		dayCount = 1
	}
	lastDay := todayLocal()
	firstDay := lastDay.AddDate(0, 0, -(dayCount - 1))

	err = h.db.QueryRow(c, `
		SELECT
			COALESCE(SUM(CASE WHEN scheduled THEN 1 ELSE 0 END), 0)::int,
			COALESCE(SUM(CASE WHEN scheduled AND COALESCE(l.count, 0) >= h.target_count THEN 1 ELSE 0 END), 0)::int
		FROM (
			SELECT h.*, d.day,
			       (h.target_type = 'weekly'
			        OR COALESCE(array_length(h.repeat_days, 1), 0) = 0
			        OR trim(to_char(d.day, 'dy')) = ANY(h.repeat_days)) AS scheduled
			FROM habits h
			CROSS JOIN generate_series($2::date, $3::date, interval '1 day') AS d(day)
			WHERE h.user_id = $1
		) h
		LEFT JOIN habit_logs l ON l.habit_id = h.id AND l.log_date = h.day
	`, userID, firstDay.Format(dayFormat), lastDay.Format(dayFormat)).Scan(&stats.HabitsDue, &stats.HabitsDone)
	if err != nil {
		respondDBError(c, err)
		return
	}

	if stats.HabitsDue > 0 {
		stats.HabitCompletionRate = clampPercent((stats.HabitsDone * 100) / stats.HabitsDue)
	}

	// Per-day breakdown for the window. generate_series makes the series
	// gapless; every metric is a scalar subquery per generated day.
	err = h.queryDaily(c, userID, firstDay, lastDay, &stats.Daily)
	if err != nil {
		respondDBError(c, err)
		return
	}

	// Productivity blends completion rate and task throughput into 0-100.
	stats.DailyProductivity = clampPercent(stats.ScheduleCompletionRate)
	stats.WeeklyProductivity = clampPercent(
		(stats.ScheduleCompletionRate + minInt(stats.CompletedTasks*10, 100)) / 2,
	)

	response.OK(c, stats)
}

// queryDaily fills the per-day series in one round trip. The day bounds are
// Go-local dates supplied by the caller, so the series cannot drift when the
// Postgres session runs in a different zone than the user.
func (h *StatisticsHandler) queryDaily(c *gin.Context, userID uuid.UUID, firstDay, lastDay time.Time, out *[]DailyStat) error {
	rows, err := h.db.Query(c, `
		SELECT to_char(d.day, 'YYYY-MM-DD'),
		       (SELECT COUNT(*)::int FROM schedules s
		         WHERE s.user_id = $1 AND s.status = 'completed'
		           AND s.start_time >= d.day AND s.start_time < d.day + interval '1 day'),
		       (SELECT COUNT(*)::int FROM tasks t
		         WHERE t.user_id = $1 AND t.status = 'completed'
		           AND t.updated_at >= d.day AND t.updated_at < d.day + interval '1 day'),
		       (SELECT COALESCE(SUM(f.actual_minutes), 0)::int FROM focus_sessions f
		         WHERE f.user_id = $1 AND f.status = 'completed'
		           AND f.started_at >= d.day AND f.started_at < d.day + interval '1 day'),
		       (SELECT COUNT(*)::int FROM habit_logs hl
		         JOIN habits hb ON hb.id = hl.habit_id
		         WHERE hl.user_id = $1 AND hl.log_date = d.day::date
		           AND hl.count >= hb.target_count)
		FROM generate_series($2::date, $3::date, interval '1 day') AS d(day)
		ORDER BY d.day ASC
	`, userID, firstDay.Format(dayFormat), lastDay.Format(dayFormat))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var d DailyStat
		if err := rows.Scan(&d.Date, &d.SchedulesCompleted, &d.TasksCompleted,
			&d.FocusMinutes, &d.HabitsDone); err != nil {
			return err
		}
		*out = append(*out, d)
	}
	return rows.Err()
}

func clampPercent(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
