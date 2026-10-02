package handler

import (
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// HabitHandler owns habits and their per-day completion log.
//
// Streaks and completion rates are computed from the log in ONE query per
// request (see logsFor) — never one query per day. All day arithmetic happens
// in Go local time: the DB session runs UTC while the user is UTC+7, so
// CURRENT_DATE would silently shift every user's day.
type HabitHandler struct {
	db *pgxpool.Pool
}

func NewHabitHandler(db *pgxpool.Pool) *HabitHandler {
	return &HabitHandler{db: db}
}

type Habit struct {
	ID               uuid.UUID  `json:"id"`
	CategoryID       *uuid.UUID `json:"category_id"`
	Name             string     `json:"name"`
	Description      string     `json:"description"`
	TargetType       string     `json:"target_type"` // daily|weekly
	TargetCount      int        `json:"target_count"`
	RepeatDays       []string   `json:"repeat_days"`
	TodayCount       int        `json:"today_count"`
	DoneToday        bool       `json:"done_today"`
	Streak           int        `json:"streak"`
	BestStreak       int        `json:"best_streak"`
	CompletionRate   int        `json:"completion_rate"` // percent
	TotalCompletions int        `json:"total_completions"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type habitRequest struct {
	Name        string     `json:"name"         binding:"required,max=100"`
	Description string     `json:"description"  binding:"max=2000"`
	CategoryID  *uuid.UUID `json:"category_id"`
	TargetType  string     `json:"target_type"  binding:"omitempty,oneof=daily weekly"`
	TargetCount int        `json:"target_count" binding:"omitempty,min=1,max=100"`
	RepeatDays  []string   `json:"repeat_days"  binding:"omitempty,dive,oneof=mon tue wed thu fri sat sun"`
}

type habitLogRequest struct {
	Date  string `json:"date"`  // "YYYY-MM-DD"; defaults to the caller's local day
	Delta *int   `json:"delta"` // default +1
	Count *int   `json:"count"` // absolute value; wins over delta
}

type HabitLog struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
	Note  string `json:"note"`
}

// HabitHistory is one habit's logs across a window, for the completion calendar.
type HabitHistory struct {
	HabitID uuid.UUID  `json:"habit_id"`
	Days    []HabitLog `json:"days"`
}

// habitWindowDays bounds how far back streak math looks. A year of daily
// logging is far beyond what the UI shows and keeps the single log query small.
const habitWindowDays = 400

func validRepeatDays(days []string) bool {
	for _, d := range days {
		switch d {
		case "mon", "tue", "wed", "thu", "fri", "sat", "sun":
		default:
			return false
		}
	}
	return true
}

func (h *HabitHandler) List(c *gin.Context) {
	userID := middleware.GetUserID(c)
	p := pagination(c)

	rows, err := h.db.Query(c, `
		SELECT id, category_id, name, COALESCE(description, ''), target_type,
		       target_count, COALESCE(repeat_days, '{}'), created_at, updated_at
		FROM habits
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, userID, p.Limit, p.Offset)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	habits := make([]Habit, 0, p.Limit)
	ids := make([]uuid.UUID, 0, p.Limit)
	for rows.Next() {
		var hb Habit
		if err := rows.Scan(&hb.ID, &hb.CategoryID, &hb.Name, &hb.Description,
			&hb.TargetType, &hb.TargetCount, &hb.RepeatDays, &hb.CreatedAt, &hb.UpdatedAt); err != nil {
			respondDBError(c, err)
			return
		}
		habits = append(habits, hb)
		ids = append(ids, hb.ID)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	if err := h.attachProgress(c, habits, ids, todayLocal()); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, habits)
}

func (h *HabitHandler) Get(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	hb, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}

	out := []Habit{hb}
	if err := h.attachProgress(c, out, []uuid.UUID{id}, todayLocal()); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, out[0])
}

func (h *HabitHandler) Create(c *gin.Context) {
	var req habitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if !validRepeatDays(req.RepeatDays) {
		response.BadRequest(c, "repeat_days must be mon..sun")
		return
	}
	applyHabitDefaults(&req)

	userID := middleware.GetUserID(c)

	var id uuid.UUID
	err := h.db.QueryRow(c, `
		INSERT INTO habits (user_id, category_id, name, description, target_type, target_count, repeat_days)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`, userID, req.CategoryID, req.Name, req.Description, req.TargetType, req.TargetCount, req.RepeatDays).Scan(&id)
	if err != nil {
		respondDBError(c, err)
		return
	}

	hb, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	response.Created(c, hb)
}

func (h *HabitHandler) Update(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}

	var req habitRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if !validRepeatDays(req.RepeatDays) {
		response.BadRequest(c, "repeat_days must be mon..sun")
		return
	}
	applyHabitDefaults(&req)

	userID := middleware.GetUserID(c)

	tag, err := h.db.Exec(c, `
		UPDATE habits
		SET category_id = $1, name = $2, description = $3, target_type = $4,
		    target_count = $5, repeat_days = $6, updated_at = NOW()
		WHERE id = $7 AND user_id = $8
	`, req.CategoryID, req.Name, req.Description, req.TargetType,
		req.TargetCount, req.RepeatDays, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.NotFound(c)
		return
	}

	hb, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	response.OK(c, hb)
}

func (h *HabitHandler) Delete(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	tag, err := h.db.Exec(c, `DELETE FROM habits WHERE id = $1 AND user_id = $2`, id, userID)
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

// Complete logs one unit (or an absolute count) for a habit on a day.
//
// POST /habits/:id/complete {"date":"YYYY-MM-DD","delta":1}
//
// The write is a single upsert so two concurrent taps cannot create two rows
// for the same day; a result of zero deletes the row (nothing logged).
func (h *HabitHandler) Complete(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}

	// Empty body is valid: "log one today".
	var req habitLogRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		response.BadRequest(c, err.Error())
		return
	}

	day, ok := resolveDay(c, req.Date)
	if !ok {
		return
	}

	userID := middleware.GetUserID(c)
	if !ownsHabit(c, h.db, id, userID) {
		return
	}

	var delta int
	switch {
	case req.Count != nil:
		if *req.Count < 0 || *req.Count > 1000 {
			response.BadRequest(c, "count must be within 0..1000")
			return
		}
		// Absolute set: 0 deletes the row (nothing logged that day).
		if _, err := h.db.Exec(c, `
			INSERT INTO habit_logs (habit_id, user_id, log_date, count)
			VALUES ($1, $2, $3::date, $4)
			ON CONFLICT (habit_id, log_date)
			DO UPDATE SET count = EXCLUDED.count, updated_at = NOW()
		`, id, userID, day, *req.Count); err != nil {
			respondDBError(c, err)
			return
		}
	case req.Delta == nil:
		delta = 1
	default:
		delta = *req.Delta
		if delta < -100 || delta > 100 {
			response.BadRequest(c, "delta must be within -100..100")
			return
		}
	}

	if req.Count == nil {
		// Single atomic upsert: racing taps cannot create two rows for one day.
		if _, err := h.db.Exec(c, `
			INSERT INTO habit_logs (habit_id, user_id, log_date, count)
			VALUES ($1, $2, $3::date, GREATEST($4, 0))
			ON CONFLICT (habit_id, log_date)
			DO UPDATE SET count = GREATEST(habit_logs.count + $4, 0), updated_at = NOW()
		`, id, userID, day, delta); err != nil {
			respondDBError(c, err)
			return
		}
	}

	// A log that reached zero carries no information; dropping it keeps the
	// completion-rate denominator honest.
	if _, err := h.db.Exec(c, `
		DELETE FROM habit_logs WHERE habit_id = $1 AND log_date = $2::date AND count = 0
	`, id, day); err != nil {
		respondDBError(c, err)
		return
	}

	hb, err := h.fetch(c, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	out := []Habit{hb}
	if err := h.attachProgress(c, out, []uuid.UUID{id}, todayLocal()); err != nil {
		respondDBError(c, err)
		return
	}
	response.OK(c, out[0])
}

// Logs lists a habit's log rows in a window, for the completion calendar.
//
// GET /habits/:id/logs?from=YYYY-MM-DD&to=YYYY-MM-DD
func (h *HabitHandler) Logs(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)
	if !ownsHabit(c, h.db, id, userID) {
		return
	}

	today := todayLocal()
	fromRaw := c.DefaultQuery("from", today.AddDate(0, 0, -90).Format(dayFormat))
	toRaw := c.DefaultQuery("to", today.Format(dayFormat))
	if _, ok := resolveDay(c, fromRaw); !ok {
		return
	}
	if _, ok := resolveDay(c, toRaw); !ok {
		return
	}

	rows, err := h.db.Query(c, `
		SELECT to_char(log_date, 'YYYY-MM-DD'), count, COALESCE(note, '')
		FROM habit_logs
		WHERE habit_id = $1 AND log_date BETWEEN $2::date AND $3::date
		ORDER BY log_date ASC
	`, id, fromRaw, toRaw)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	logs := make([]HabitLog, 0, 90)
	for rows.Next() {
		var l HabitLog
		if err := rows.Scan(&l.Date, &l.Count, &l.Note); err != nil {
			respondDBError(c, err)
			return
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, logs)
}

// History lists recent logs for every habit the caller owns, in ONE query.
//
// GET /habits/history?days=30
//
// The per-habit view lives here rather than on `GET /habits` so the list
// endpoint does not pay for history the user has not opened, and it is batched
// rather than per-habit so the completion calendar costs one round trip no
// matter how many habits exist.
func (h *HabitHandler) History(c *gin.Context) {
	userID := middleware.GetUserID(c)

	days := 30
	if raw := c.Query("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > habitWindowDays {
			response.BadRequest(c, "days must be within 1.."+strconv.Itoa(habitWindowDays))
			return
		}
		days = n
	}

	today := todayLocal()
	since := today.AddDate(0, 0, -(days - 1))

	rows, err := h.db.Query(c, `
		SELECT hl.habit_id, to_char(hl.log_date, 'YYYY-MM-DD'), hl.count
		FROM habit_logs hl
		JOIN habits hb ON hb.id = hl.habit_id
		WHERE hb.user_id = $1 AND hl.log_date BETWEEN $2::date AND $3::date
		ORDER BY hl.habit_id, hl.log_date
	`, userID, since.Format(dayFormat), today.Format(dayFormat))
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	out := make([]HabitHistory, 0, 8)
	index := make(map[uuid.UUID]int)
	for rows.Next() {
		var habitID uuid.UUID
		var log HabitLog
		if err := rows.Scan(&habitID, &log.Date, &log.Count); err != nil {
			respondDBError(c, err)
			return
		}
		i, ok := index[habitID]
		if !ok {
			i = len(out)
			index[habitID] = i
			out = append(out, HabitHistory{HabitID: habitID, Days: []HabitLog{}})
		}
		out[i].Days = append(out[i].Days, log)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, out)
}

// ---- internals -------------------------------------------------------------

func applyHabitDefaults(req *habitRequest) {
	if req.TargetType == "" {
		req.TargetType = "daily"
	}
	if req.TargetCount < 1 {
		req.TargetCount = 1
	}
	if req.RepeatDays == nil {
		req.RepeatDays = []string{}
	}
}

func (h *HabitHandler) fetch(c *gin.Context, id, userID uuid.UUID) (Habit, error) {
	var hb Habit
	err := h.db.QueryRow(c, `
		SELECT id, category_id, name, COALESCE(description, ''), target_type,
		       target_count, COALESCE(repeat_days, '{}'), created_at, updated_at
		FROM habits WHERE id = $1 AND user_id = $2
	`, id, userID).Scan(&hb.ID, &hb.CategoryID, &hb.Name, &hb.Description,
		&hb.TargetType, &hb.TargetCount, &hb.RepeatDays, &hb.CreatedAt, &hb.UpdatedAt)
	if err != nil {
		return Habit{}, err
	}
	return hb, nil
}

// attachProgress loads the logs for every habit in one query and fills the
// derived fields (today's count, streak, best streak, completion rate).
// The slice is mutated in place; callers pass a value slice they own.
func (h *HabitHandler) attachProgress(c *gin.Context, habits []Habit, ids []uuid.UUID, today time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	logs, err := h.logsFor(c, ids, today.AddDate(0, 0, -habitWindowDays))
	if err != nil {
		return err
	}
	for i := range habits {
		fillHabitProgress(&habits[i], logs[habits[i].ID], today)
	}
	return nil
}

// logsFor returns, per habit, every logged day at or after `since`.
// One query for the whole page: no per-habit and no per-day round trips.
func (h *HabitHandler) logsFor(c *gin.Context, ids []uuid.UUID, since time.Time) (map[uuid.UUID]map[string]int, error) {
	out := make(map[uuid.UUID]map[string]int, len(ids))

	rows, err := h.db.Query(c, `
		SELECT habit_id, to_char(log_date, 'YYYY-MM-DD'), count
		FROM habit_logs
		WHERE habit_id = ANY($1) AND log_date >= $2::date
		ORDER BY habit_id, log_date
	`, ids, since.Format(dayFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id uuid.UUID
		var day string
		var count int
		if err := rows.Scan(&id, &day, &count); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = make(map[string]int)
		}
		out[id][day] = count
	}
	return out, rows.Err()
}

// fillHabitProgress derives today's state and the streak numbers from the log
// map. Pure function over data — the same rules the UI displays.
func fillHabitProgress(hb *Habit, logs map[string]int, today time.Time) {
	if logs == nil {
		logs = map[string]int{}
	}
	todayKey := today.Format(dayFormat)
	hb.TodayCount = logs[todayKey]
	hb.DoneToday = hb.TodayCount >= hb.TargetCount && habitScheduledOn(hb, today)

	if hb.TargetType == "weekly" {
		hb.Streak, hb.BestStreak = weekStreak(hb, logs, today)
	} else {
		hb.Streak, hb.BestStreak = dayStreak(hb, logs, today)
	}

	hb.TotalCompletions = 0
	for _, n := range logs {
		hb.TotalCompletions += n
	}

	hb.CompletionRate = habitCompletionRate(hb, logs, today)
}

// habitScheduledOn reports whether a daily habit expects a log on `day`.
// An empty repeat_days means every day.
func habitScheduledOn(hb *Habit, day time.Time) bool {
	if hb.TargetType == "weekly" {
		return true
	}
	if len(hb.RepeatDays) == 0 {
		return true
	}
	wd := weekdayKey(day)
	for _, d := range hb.RepeatDays {
		if d == wd {
			return true
		}
	}
	return false
}

// dayStreak counts consecutive scheduled days meeting the target, walking back
// from today. An unfinished today does not break the streak — the day is not
// over yet — it is simply skipped.
func dayStreak(hb *Habit, logs map[string]int, today time.Time) (streak, best int) {
	limit := today.AddDate(0, 0, -habitWindowDays)

	// current run, walking forward through the window
	run := 0
	for d := limit; !d.After(today); d = d.AddDate(0, 0, 1) {
		if !habitScheduledOn(hb, d) {
			continue
		}
		met := logs[d.Format(dayFormat)] >= hb.TargetCount
		if met {
			run++
			if run > best {
				best = run
			}
			continue
		}
		// Today may still be completed; every other missed scheduled day ends the run.
		if d.Equal(today) {
			continue
		}
		run = 0
	}

	// The live streak is the run ending at today (or at the last scheduled day).
	streak = 0
	for d := today; !d.Before(limit); d = d.AddDate(0, 0, -1) {
		if !habitScheduledOn(hb, d) {
			continue
		}
		if logs[d.Format(dayFormat)] >= hb.TargetCount {
			streak++
			continue
		}
		if d.Equal(today) {
			continue // grace: today is unfinished, not missed
		}
		break
	}
	return streak, best
}

// weekStreak counts consecutive ISO weeks (Monday-start) where the weekly total
// met target_count. The current week is skipped while it can still be finished.
func weekStreak(hb *Habit, logs map[string]int, today time.Time) (streak, best int) {
	weekly := map[string]int{}
	for day, n := range logs {
		d, err := time.Parse(dayFormat, day)
		if err != nil {
			continue
		}
		weekly[weekKey(d)] += n
	}

	start := weekStart(today)
	start = start.AddDate(0, 0, -7*(habitWindowDays/7))

	run := 0
	for w := start; !w.After(weekStart(today)); w = w.AddDate(0, 0, 7) {
		if weekly[weekKey(w)] >= hb.TargetCount {
			run++
			if run > best {
				best = run
			}
			continue
		}
		if w.Equal(weekStart(today)) {
			continue // current week still in progress
		}
		run = 0
	}

	for w := weekStart(today); !w.Before(start); w = w.AddDate(0, 0, -7) {
		if weekly[weekKey(w)] >= hb.TargetCount {
			streak++
			continue
		}
		if w.Equal(weekStart(today)) {
			continue
		}
		break
	}
	return streak, best
}

// habitCompletionRate is met periods over elapsed scheduled periods, starting
// at the later of the creation day and the habit's earliest log (so a habit
// backfilled with history is measured over that history, not over an empty
// window). Capped at 100.
func habitCompletionRate(hb *Habit, logs map[string]int, today time.Time) int {
	created := hb.CreatedAt.In(time.Local)
	from := time.Date(created.Year(), created.Month(), created.Day(), 0, 0, 0, 0, time.Local)

	// An earlier log extends the window backwards — the habit was in use then.
	for day := range logs {
		if d, err := time.Parse(dayFormat, day); err == nil && d.Before(from) {
			from = d
		}
	}

	if from.After(today) {
		from = today
	}
	if floor := today.AddDate(0, 0, -habitWindowDays); from.Before(floor) {
		from = floor
	}

	if hb.TargetType == "weekly" {
		total, met := 0, 0
		weekly := map[string]int{}
		for day, n := range logs {
			if d, err := time.Parse(dayFormat, day); err == nil {
				weekly[weekKey(d)] += n
			}
		}
		for w := weekStart(from); !w.After(weekStart(today)); w = w.AddDate(0, 0, 7) {
			total++
			if weekly[weekKey(w)] >= hb.TargetCount {
				met++
			}
		}
		if total == 0 {
			return 0
		}
		return clampPercent((met * 100) / total)
	}

	total, met := 0, 0
	for d := from; !d.After(today); d = d.AddDate(0, 0, 1) {
		if !habitScheduledOn(hb, d) {
			continue
		}
		if d.Equal(today) && logs[d.Format(dayFormat)] < hb.TargetCount {
			continue // today is not a miss yet
		}
		total++
		if logs[d.Format(dayFormat)] >= hb.TargetCount {
			met++
		}
	}
	if total == 0 {
		return 0
	}
	return clampPercent((met * 100) / total)
}

const dayFormat = "2006-01-02"

// todayLocal is "today" in the server's local zone (WIB on the dev box).
func todayLocal() time.Time {
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
}

// resolveDay parses a YYYY-MM-DD client date, defaulting to today. On failure
// it writes 400 and returns ok=false.
func resolveDay(c *gin.Context, raw string) (string, bool) {
	if raw == "" {
		return todayLocal().Format(dayFormat), true
	}
	if _, err := time.Parse(dayFormat, raw); err != nil {
		response.BadRequest(c, "date must be YYYY-MM-DD")
		return "", false
	}
	return raw, true
}

var weekdayKeys = map[time.Weekday]string{
	time.Monday: "mon", time.Tuesday: "tue", time.Wednesday: "wed",
	time.Thursday: "thu", time.Friday: "fri", time.Saturday: "sat", time.Sunday: "sun",
}

func weekdayKey(d time.Time) string { return weekdayKeys[d.Weekday()] }

// weekStart returns the Monday 00:00 of the week containing d.
func weekStart(d time.Time) time.Time {
	offset := (int(d.Weekday()) + 6) % 7 // Monday == 0
	day := d.AddDate(0, 0, -offset)
	return time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, d.Location())
}

func weekKey(d time.Time) string { return weekStart(d).Format(dayFormat) }

// ownsHabit reports whether the habit belongs to the caller, writing 404 when
// it does not (same shape as the ownership gates in routine.go).
func ownsHabit(c *gin.Context, db *pgxpool.Pool, id, userID uuid.UUID) bool {
	var exists bool
	err := db.QueryRow(c, `SELECT TRUE FROM habits WHERE id = $1 AND user_id = $2`, id, userID).Scan(&exists)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			response.NotFound(c)
			return false
		}
		respondDBError(c, err)
		return false
	}
	return true
}
