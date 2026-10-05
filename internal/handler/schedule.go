package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/internal/queue"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Schedule status values (also used by schedule_occurrences).
const (
	StatusUpcoming  = "upcoming"
	StatusOngoing   = "ongoing"
	StatusCompleted = "completed"
	StatusMissed    = "missed"
	StatusSkipped   = "skipped"
)

// ScheduleHandler owns schedule CRUD, reminder fan-out, and status derivation.
type ScheduleHandler struct {
	db *pgxpool.Pool
	q  *queue.Client // nil in tests / API-only mode → reminders are not enqueued

	google *googlecal.Service // nil when Google sync is not configured
}

// SetGoogle enables Google Calendar push/delete.
func (h *ScheduleHandler) SetGoogle(g *googlecal.Service) { h.google = g }

func NewScheduleHandler(db *pgxpool.Pool, q *queue.Client) *ScheduleHandler {
	return &ScheduleHandler{db: db, q: q}
}

type Schedule struct {
	ID           uuid.UUID  `json:"id"`
	OccurrenceID *uuid.UUID `json:"occurrence_id"` // set only for recurring occurrences
	CategoryID   *uuid.UUID `json:"category_id"`
	Title        string     `json:"title"`
	Description  string     `json:"description"`
	StartTime    time.Time  `json:"start_time"`
	EndTime      *time.Time `json:"end_time"`
	AllDay       bool       `json:"all_day"`
	Status       string     `json:"status"`
	Recurrence   string     `json:"recurrence"`
	Reminders    []int      `json:"reminders"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`

	Source          string  `json:"source"`
	GoogleEventID   *string `json:"google_event_id"`
	GoogleSyncState *string `json:"google_sync_state"`
	GoogleSyncError *string `json:"google_sync_error"`

	Location  *string    `json:"location"`
	ColorID   *string    `json:"color_id"`
	MeetLink  *string    `json:"meet_link"`
	Timezone  string     `json:"timezone"`
	Attendees []Attendee `json:"attendees"`
}

type scheduleRequest struct {
	Title       string     `json:"title"       binding:"required,max=200"`
	Description string     `json:"description" binding:"max=2000"`
	CategoryID  *uuid.UUID `json:"category_id"`
	StartTime   time.Time  `json:"start_time"  binding:"required"`
	EndTime     *time.Time `json:"end_time"`
	AllDay      bool       `json:"all_day"`
	Recurrence  string     `json:"recurrence"  binding:"omitempty,max=500"`
	Reminders   []int      `json:"reminders"   binding:"omitempty,dive,min=0,max=40320"`

	// v2 (docs/google-sync-v2.md); push_to_google is gone (docs/google-first.md), unknown JSON keys are ignored. nil Reminders/Attendees/AddMeet/Timezone "" = keep on PUT.
	Location    string   `json:"location"    binding:"max=300"`
	ColorID     *string  `json:"color_id"`
	Timezone    string   `json:"timezone"`
	Attendees   []string `json:"attendees"`
	SendUpdates string   `json:"send_updates" binding:"omitempty,oneof=all none"`
	AddMeet     *bool    `json:"add_meet"`
}

// computedStatus derives the live status of a schedule from the clock.
// User-set terminal states (completed/skipped) always win.
func computedStatus(start time.Time, end *time.Time, stored string, now time.Time) string {
	if stored == StatusCompleted || stored == StatusSkipped {
		return stored
	}
	finish := start
	if end != nil && end.After(start) {
		finish = *end
	}
	switch {
	case now.Before(start):
		return StatusUpcoming
	case now.Before(finish):
		return StatusOngoing
	default:
		return StatusMissed
	}
}

const scheduleColumns = `s.id, s.category_id, s.title, COALESCE(s.description, ''),
	s.start_time, s.end_time, s.all_day, s.status,
	COALESCE(r.rule, ''), s.created_at, s.updated_at,
	s.source, s.google_event_id, s.google_sync_state, s.google_sync_error,
	s.location, s.color_id, s.meet_link, s.tz`

const scheduleFrom = `FROM schedules s
	LEFT JOIN schedule_recurrences r ON r.schedule_id = s.id`

// scheduleStatusSQL mirrors computedStatus in SQL, so a query can filter on the
// status the API actually reports. Filtering the stored column instead silently
// matched nothing for the derived states — `?status=missed` returned an empty
// list while the same rows came back labelled "missed".
const scheduleStatusSQL = `CASE
		WHEN s.status IN ('completed', 'skipped') THEN s.status
		WHEN NOW() < s.start_time THEN 'upcoming'
		WHEN NOW() < CASE WHEN s.end_time IS NOT NULL AND s.end_time > s.start_time
		                  THEN s.end_time ELSE s.start_time END THEN 'ongoing'
		ELSE 'missed'
	END`

func (h *ScheduleHandler) List(c *gin.Context) {
	userID := middleware.GetUserID(c)
	p := pagination(c)

	var from, to *time.Time
	if raw := c.Query("from"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			response.BadRequest(c, "invalid from: must be RFC3339")
			return
		}
		from = &t
	}
	if raw := c.Query("to"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			response.BadRequest(c, "invalid to: must be RFC3339")
			return
		}
		to = &t
	}

	var status *string
	if raw := c.Query("status"); raw != "" {
		if !validStatus(raw) {
			response.BadRequest(c, "invalid status")
			return
		}
		status = &raw
	}

	rows, err := h.db.Query(c, `
		SELECT `+scheduleColumns+`
		`+scheduleFrom+`
		WHERE s.user_id = $1
		  AND ($2::timestamptz IS NULL OR s.start_time >= $2)
		  AND ($3::timestamptz IS NULL OR s.start_time <= $3)
		  AND ($4::text IS NULL OR `+scheduleStatusSQL+` = $4)
		ORDER BY s.start_time ASC
		LIMIT $5 OFFSET $6
	`, userID, from, to, status, p.Limit, p.Offset)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	now := time.Now()
	schedules := make([]Schedule, 0, p.Limit)
	ids := make([]uuid.UUID, 0, p.Limit)
	for rows.Next() {
		s, err := scanSchedule(rows, now)
		if err != nil {
			respondDBError(c, err)
			return
		}
		schedules = append(schedules, s)
		ids = append(ids, s.ID)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	reminders, err := h.remindersFor(c, ids)
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

	response.OK(c, schedules)
}

func (h *ScheduleHandler) Get(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	s, err := h.fetch(c, id, userID, time.Now())
	if err != nil {
		respondDBError(c, err)
		return
	}

	reminders, err := h.remindersFor(c, []uuid.UUID{id})
	if err != nil {
		respondDBError(c, err)
		return
	}
	s.Reminders = reminders[id]
	if !h.attachOne(c, &s) {
		return
	}

	response.OK(c, s)
}

// attachOne loads the guests of one schedule; it writes the error response on failure.
func (h *ScheduleHandler) attachOne(c *gin.Context, s *Schedule) bool {
	one := []Schedule{*s}
	if err := attachAttendees(c, h.db, one); err != nil {
		respondDBError(c, err)
		return false
	}
	s.Attendees = one[0].Attendees
	return true
}

func (h *ScheduleHandler) Create(c *gin.Context) {
	var req scheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if req.EndTime != nil && req.EndTime.Before(req.StartTime) {
		response.BadRequest(c, "end_time must not be before start_time")
		return
	}
	if code := req.validate(); code != "" {
		response.Error(c, http.StatusBadRequest, code)
		return
	}

	userID := middleware.GetUserID(c)
	id, ok := h.create(c, userID, req)
	if !ok {
		return
	}
	h.respondSchedule(c, id, userID, true)
}

// create is the write-through insert shared by Create and Duplicate: Google
// first, then the local cache. On any Google failure nothing is stored. It
// writes the error response and returns ok=false on failure. With Google not
// configured at all, schedules stay local-only (dev setups and tests).
func (h *ScheduleHandler) create(c *gin.Context, userID uuid.UUID, req scheduleRequest) (uuid.UUID, bool) {
	if !h.gate(c, userID) {
		return uuid.Nil, false
	}
	var ev googlecal.Event
	var res googlecal.Result
	if h.google != nil {
		req.applyTo(&ev)
		ev.Recurrence = req.Recurrence
		var err error
		if res, err = h.google.InsertRemote(c, userID, &ev, req.pushOpts()); err != nil {
			googleError(c, err)
			return uuid.Nil, false
		}
	}

	var id uuid.UUID
	err := h.db.QueryRow(c, `
		INSERT INTO schedules (user_id, category_id, title, description, start_time, end_time, all_day,
		                       location, color_id, tz, google_event_id, google_etag, meet_link)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, COALESCE(NULLIF($10, ''), 'UTC'),
		        NULLIF($11, ''), NULLIF($12, ''), NULLIF($13, ''))
		RETURNING id
	`, userID, req.CategoryID, req.Title, req.Description, req.StartTime, req.EndTime, req.AllDay,
		req.Location, req.ColorID, req.Timezone, res.ID, res.ETag, res.MeetLink).Scan(&id)
	if err != nil {
		if res.ID != "" { // do not leave an orphan event behind
			_ = h.google.DeleteRemote(c, userID, res.ID, "none")
		}
		respondDBError(c, err)
		return uuid.Nil, false
	}

	if req.Recurrence != "" {
		if _, err := h.db.Exec(c, `
			INSERT INTO schedule_recurrences (schedule_id, rule) VALUES ($1, $2)
		`, id, req.Recurrence); err != nil {
			respondDBError(c, err)
			return uuid.Nil, false
		}
	}

	if err := h.syncReminders(c, id, userID, req); err != nil {
		respondDBError(c, err)
		return uuid.Nil, false
	}
	if err := replaceAttendees(c, h.db, id, req.Attendees); err != nil {
		respondDBError(c, err)
		return uuid.Nil, false
	}
	h.syncSeries(userID, req.Recurrence)
	return id, true
}

// gate checks, before any write, that Google is usable (409 not_connected /
// needs_reauth otherwise). Always true when Google is not configured.
func (h *ScheduleHandler) gate(c *gin.Context, userID uuid.UUID) bool {
	if h.google == nil {
		return true
	}
	if err := h.google.Require(c, userID); err != nil {
		googleError(c, err)
		return false
	}
	return true
}

// syncSeries pulls Google's view after a recurring-series write so instances appear.
func (h *ScheduleHandler) syncSeries(userID uuid.UUID, recurrence string) {
	if h.google != nil && recurrence != "" {
		h.google.SyncBackground(userID)
	}
}

// remoteWrite loads schedule id as a Google event, lets mutate change it, and
// writes it to Google (insert when the row has no event yet, else patch)
// BEFORE the caller touches local data. Returns ok=false after writing the
// error response. With Google not configured it is a no-op.
func (h *ScheduleHandler) remoteWrite(c *gin.Context, userID, id uuid.UUID, opts googlecal.PushOpts,
	mutate func(*googlecal.Event)) (ev googlecal.Event, res googlecal.Result, ok bool) {
	if h.google == nil {
		return ev, res, true
	}
	ev, evID, err := h.google.LoadEvent(c, userID, id)
	if err != nil {
		respondDBError(c, err)
		return ev, res, false
	}
	mutate(&ev)
	if evID == "" {
		res, err = h.google.InsertRemote(c, userID, &ev, opts)
	} else {
		res, err = h.google.PatchRemote(c, userID, evID, &ev, opts)
		res.ID = evID
	}
	if err != nil {
		googleError(c, err)
		return ev, res, false
	}
	return ev, res, true
}

// respondSchedule writes the schedule with its reminders and guests.
func (h *ScheduleHandler) respondSchedule(c *gin.Context, id, userID uuid.UUID, created bool) {
	s, err := h.fetch(c, id, userID, time.Now())
	if err != nil {
		respondDBError(c, err)
		return
	}
	reminders, _ := h.remindersFor(c, []uuid.UUID{id})
	s.Reminders = reminders[id]
	if !h.attachOne(c, &s) {
		return
	}
	if created {
		response.Created(c, s)
		return
	}
	response.OK(c, s)
}

func (h *ScheduleHandler) Update(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}

	var req scheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if req.EndTime != nil && req.EndTime.Before(req.StartTime) {
		response.BadRequest(c, "end_time must not be before start_time")
		return
	}
	if code := req.validate(); code != "" {
		response.Error(c, http.StatusBadRequest, code)
		return
	}

	userID := middleware.GetUserID(c)

	cur, ok := h.current(c, id, userID)
	if !ok || !h.gate(c, userID) {
		return
	}
	if cur.imported {
		req.Recurrence = "" // instances never carry a rule
	}

	// Google first, so a failure leaves the local row unchanged.
	ev, res, ok := h.remoteWrite(c, userID, id, req.pushOpts(), func(e *googlecal.Event) {
		req.applyTo(e)
		if !cur.imported {
			e.Recurrence = req.Recurrence
		}
	})
	if !ok {
		return
	}

	// user_id in WHERE is the ownership check — a foreign row updates 0 rows.
	tag, err := h.db.Exec(c, `
		UPDATE schedules
		SET category_id = $1, title = $2, description = $3,
		    start_time = $4, end_time = $5, all_day = $6,
		    location = NULLIF($9, ''), color_id = $10, tz = COALESCE(NULLIF($11, ''), tz),
		    updated_at = NOW()
		WHERE id = $7 AND user_id = $8
	`, req.CategoryID, req.Title, req.Description, req.StartTime, req.EndTime, req.AllDay, id, userID,
		req.Location, req.ColorID, req.Timezone)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.NotFound(c)
		return
	}

	if !cur.imported {
		// Replace the recurrence rule.
		if _, err := h.db.Exec(c, `DELETE FROM schedule_recurrences WHERE schedule_id = $1`, id); err != nil {
			respondDBError(c, err)
			return
		}
		if req.Recurrence != "" {
			if _, err := h.db.Exec(c, `
				INSERT INTO schedule_recurrences (schedule_id, rule) VALUES ($1, $2)
			`, id, req.Recurrence); err != nil {
				respondDBError(c, err)
				return
			}
		}
	}

	if req.Reminders == nil { // omitted = keep, but fire times follow the new start
		if req.Reminders, err = reminderOffsets(c, h.db, id); err != nil {
			respondDBError(c, err)
			return
		}
	}
	if err := h.syncReminders(c, id, userID, req); err != nil {
		respondDBError(c, err)
		return
	}
	if req.Attendees != nil {
		if err := replaceAttendees(c, h.db, id, req.Attendees); err != nil {
			respondDBError(c, err)
			return
		}
	}

	if h.google != nil {
		_ = h.google.Record(c, userID, id, res, ev)
		h.syncSeries(userID, ev.Recurrence)
	}
	h.respondSchedule(c, id, userID, false)
}

func (h *ScheduleHandler) Delete(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	var eventID *string
	if err := h.db.QueryRow(c, `SELECT google_event_id FROM schedules WHERE id = $1 AND user_id = $2`,
		id, userID).Scan(&eventID); err != nil {
		respondDBError(c, err)
		return
	}
	if !h.gate(c, userID) {
		return
	}

	// Guests are only notified of the cancellation when there are any.
	sendUpdates := "none"
	var guests int
	if err := h.db.QueryRow(c, `SELECT COUNT(*) FROM schedule_attendees WHERE schedule_id = $1`,
		id).Scan(&guests); err == nil && guests > 0 {
		sendUpdates = "all"
	}
	if q := c.Query("send_updates"); q == "all" || q == "none" {
		sendUpdates = q
	}

	// Google first, so a failure leaves the local row in place.
	if eventID != nil && h.google != nil {
		if err := h.google.DeleteRemote(c, userID, *eventID, sendUpdates); err != nil {
			googleError(c, err)
			return
		}
	}
	if _, err := h.db.Exec(c, `DELETE FROM schedules WHERE id = $1 AND user_id = $2`, id, userID); err != nil {
		respondDBError(c, err)
		return
	}

	response.NoContent(c)
}

// Complete / Skip set a terminal status the clock cannot override.
func (h *ScheduleHandler) Complete(c *gin.Context) { h.setStatus(c, StatusCompleted) }
func (h *ScheduleHandler) Skip(c *gin.Context)     { h.setStatus(c, StatusSkipped) }

// Duplicate clones a schedule into a fresh, upcoming one. Category, title,
// description, times, all_day, recurrence rule, and reminder offsets are all
// copied; the new row gets its own id and re-derived reminder fire times.
func (h *ScheduleHandler) Duplicate(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	var src Schedule
	row := h.db.QueryRow(c, `
		SELECT `+scheduleColumns+`
		`+scheduleFrom+`
		WHERE s.id = $1 AND s.user_id = $2
	`, id, userID)
	src, err := scanSchedule(row, time.Now())
	if err != nil {
		respondDBError(c, err)
		return
	}

	srcReminders, err := h.remindersFor(c, []uuid.UUID{src.ID})
	if err != nil {
		respondDBError(c, err)
		return
	}

	req := scheduleRequest{
		Title:       src.Title,
		Description: src.Description,
		CategoryID:  src.CategoryID,
		StartTime:   src.StartTime,
		EndTime:     src.EndTime,
		AllDay:      src.AllDay,
		Recurrence:  src.Recurrence,
		Reminders:   srcReminders[src.ID],
		ColorID:     src.ColorID,
		Timezone:    src.Timezone,
	}
	if src.Location != nil {
		req.Location = *src.Location
	}

	newID, ok := h.create(c, userID, req)
	if !ok {
		return
	}

	h.respondSchedule(c, newID, userID, true)
}

type moveRequest struct {
	StartTime time.Time  `json:"start_time" binding:"required"`
	EndTime   *time.Time `json:"end_time"`
}

// Move reschedules a schedule in place. For recurring schedules the materialised
// occurrences and concrete reminders are cleared so the scheduler re-expands
// them from the new DTSTART seed.
func (h *ScheduleHandler) Move(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}

	var req moveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if req.EndTime != nil && req.EndTime.Before(req.StartTime) {
		response.BadRequest(c, "end_time must not be before start_time")
		return
	}

	userID := middleware.GetUserID(c)

	if _, ok := h.current(c, id, userID); !ok || !h.gate(c, userID) {
		return
	}

	// Google first, so a failure leaves the local row unchanged.
	ev, res, ok := h.remoteWrite(c, userID, id, googlecal.PushOpts{}, func(e *googlecal.Event) {
		e.Start, e.End = req.StartTime, req.EndTime
	})
	if !ok {
		return
	}

	tag, err := h.db.Exec(c, `
		UPDATE schedules SET start_time = $1, end_time = $2, updated_at = NOW()
		WHERE id = $3 AND user_id = $4
	`, req.StartTime, req.EndTime, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.NotFound(c)
		return
	}

	// Recurring: drop occurrences + concrete reminders; templates re-expand.
	if _, err := h.db.Exec(c, `DELETE FROM schedule_occurrences WHERE schedule_id = $1`, id); err != nil {
		respondDBError(c, err)
		return
	}
	if _, err := h.db.Exec(c, `DELETE FROM reminders WHERE schedule_id = $1 AND fire_at IS NOT NULL`, id); err != nil {
		respondDBError(c, err)
		return
	}

	// Re-sync reminder templates / concrete fire times for the new start.
	var recurrence string
	if err := h.db.QueryRow(c, `
		SELECT COALESCE(r.rule, '') FROM schedules s
		LEFT JOIN schedule_recurrences r ON r.schedule_id = s.id
		WHERE s.id = $1
	`, id).Scan(&recurrence); err != nil {
		respondDBError(c, err)
		return
	}
	offsets, err := h.remindersFor(c, []uuid.UUID{id})
	if err != nil {
		respondDBError(c, err)
		return
	}
	resync := scheduleRequest{
		StartTime:  req.StartTime,
		Recurrence: recurrence,
		Reminders:  offsets[id],
	}
	if err := h.syncReminders(c, id, userID, resync); err != nil {
		respondDBError(c, err)
		return
	}
	if h.google != nil {
		_ = h.google.Record(c, userID, id, res, ev)
		h.syncSeries(userID, ev.Recurrence)
	}

	h.respondSchedule(c, id, userID, false)
}

// ---- google ----------------------------------------------------------------

// scheduleLink is the Google state of a schedule row.
type scheduleLink struct {
	imported bool // source='google'
}

// current loads the row's Google state. It writes 404 for a missing/foreign row.
func (h *ScheduleHandler) current(c *gin.Context, id, userID uuid.UUID) (scheduleLink, bool) {
	var source string
	err := h.db.QueryRow(c, `SELECT source FROM schedules WHERE id = $1 AND user_id = $2`,
		id, userID).Scan(&source)
	if err != nil {
		respondDBError(c, err)
		return scheduleLink{}, false
	}
	return scheduleLink{imported: source == "google"}, true
}

// GoogleRetry is gone: writes are write-through, so there is nothing to retry.
func (h *ScheduleHandler) GoogleRetry(c *gin.Context) {
	response.Error(c, http.StatusGone, "gone")
}

// ---- reminders (snooze / dismiss) -----------------------------------------

type reminderView struct {
	ID         uuid.UUID  `json:"id"`
	ScheduleID uuid.UUID  `json:"schedule_id"`
	OffsetMins int        `json:"offset_mins"`
	Sent       bool       `json:"sent"`
	Dismissed  bool       `json:"dismissed"`
	FireAt     *time.Time `json:"fire_at"`
}

// Reminders lists a schedule's reminder rows (including sent/dismissed), so the
// UI can offer snooze/dismiss on a concrete reminder.
func (h *ScheduleHandler) Reminders(c *gin.Context) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	// Ownership via the schedule's user_id.
	rows, err := h.db.Query(c, `
		SELECT r.id, r.schedule_id, r.offset_mins, r.sent, r.dismissed, r.fire_at
		FROM reminders r
		JOIN schedules s ON s.id = r.schedule_id
		WHERE r.schedule_id = $1 AND s.user_id = $2
		ORDER BY r.fire_at NULLS LAST, r.offset_mins
	`, id, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()

	out := make([]reminderView, 0, 4)
	for rows.Next() {
		var rv reminderView
		if err := rows.Scan(&rv.ID, &rv.ScheduleID, &rv.OffsetMins, &rv.Sent, &rv.Dismissed, &rv.FireAt); err != nil {
			respondDBError(c, err)
			return
		}
		out = append(out, rv)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}

	response.OK(c, out)
}

type snoozeRequest struct {
	Minutes int `json:"minutes" binding:"required,min=1,max=1440"`
}

// Snooze pushes a reminder's fire time forward by `minutes` and re-queues it.
func (h *ScheduleHandler) Snooze(c *gin.Context) {
	scheduleID, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	reminderID, ok := paramUUID(c, "rid")
	if !ok {
		return
	}

	var req snoozeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	userID := middleware.GetUserID(c)

	fireAt := time.Now().Add(time.Duration(req.Minutes) * time.Minute)

	var scheduleName string
	var startTime time.Time
	err := h.db.QueryRow(c, `
		UPDATE reminders r
		SET fire_at = $1, sent = FALSE, dismissed = FALSE
		FROM schedules s
		WHERE r.id = $2 AND r.schedule_id = $3 AND s.id = r.schedule_id AND s.user_id = $4
		RETURNING s.title, s.start_time
	`, fireAt, reminderID, scheduleID, userID).Scan(&scheduleName, &startTime)
	if err != nil {
		respondDBError(c, err)
		return
	}

	if h.q != nil {
		_ = h.q.EnqueueReminder(c, queue.ReminderPayload{
			ReminderID:   reminderID,
			ScheduleID:   scheduleID,
			UserID:       userID,
			ScheduleName: scheduleName,
			StartTime:    startTime,
			OffsetMins:   0,
		}, time.Until(fireAt))
	}

	rv, err := h.reminderRow(c, reminderID, scheduleID, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	response.OK(c, rv)
}

// Dismiss marks a reminder dismissed so it is never re-fired.
func (h *ScheduleHandler) DismissReminder(c *gin.Context) {
	scheduleID, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	reminderID, ok := paramUUID(c, "rid")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	tag, err := h.db.Exec(c, `
		UPDATE reminders r
		SET dismissed = TRUE
		FROM schedules s
		WHERE r.id = $1 AND r.schedule_id = $2 AND s.id = r.schedule_id AND s.user_id = $3
	`, reminderID, scheduleID, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	if tag.RowsAffected() == 0 {
		response.NotFound(c)
		return
	}

	rv, err := h.reminderRow(c, reminderID, scheduleID, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	response.OK(c, rv)
}

func (h *ScheduleHandler) reminderRow(c *gin.Context, reminderID, scheduleID, userID uuid.UUID) (reminderView, error) {
	var rv reminderView
	err := h.db.QueryRow(c, `
		SELECT r.id, r.schedule_id, r.offset_mins, r.sent, r.dismissed, r.fire_at
		FROM reminders r
		JOIN schedules s ON s.id = r.schedule_id
		WHERE r.id = $1 AND r.schedule_id = $2 AND s.user_id = $3
	`, reminderID, scheduleID, userID).
		Scan(&rv.ID, &rv.ScheduleID, &rv.OffsetMins, &rv.Sent, &rv.Dismissed, &rv.FireAt)
	return rv, err
}

func (h *ScheduleHandler) setStatus(c *gin.Context, status string) {
	id, ok := paramUUID(c, "id")
	if !ok {
		return
	}
	userID := middleware.GetUserID(c)

	// Recurring schedules are surfaced as occurrences; the client passes the
	// occurrence id so the state lands on the instance, not the series.
	if raw := c.Query("occurrence_id"); raw != "" {
		occID, err := uuid.Parse(raw)
		if err != nil {
			response.BadRequest(c, "invalid occurrence_id")
			return
		}
		tag, err := h.db.Exec(c, `
			UPDATE schedule_occurrences o
			SET status = $1
			FROM schedules s
			WHERE o.id = $2 AND o.schedule_id = s.id AND s.id = $3 AND s.user_id = $4
		`, status, occID, id, userID)
		if err != nil {
			respondDBError(c, err)
			return
		}
		if tag.RowsAffected() == 0 {
			response.NotFound(c)
			return
		}
	} else {
		tag, err := h.db.Exec(c, `
			UPDATE schedules SET status = $1, updated_at = NOW()
			WHERE id = $2 AND user_id = $3
		`, status, id, userID)
		if err != nil {
			respondDBError(c, err)
			return
		}
		if tag.RowsAffected() == 0 {
			response.NotFound(c)
			return
		}
	}

	s, err := h.fetch(c, id, userID, time.Now())
	if err != nil {
		respondDBError(c, err)
		return
	}
	if !h.attachOne(c, &s) {
		return
	}
	response.OK(c, s)
}

// ---- internals -------------------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSchedule(row rowScanner, now time.Time) (Schedule, error) {
	var s Schedule
	var stored string
	err := row.Scan(&s.ID, &s.CategoryID, &s.Title, &s.Description,
		&s.StartTime, &s.EndTime, &s.AllDay, &stored,
		&s.Recurrence, &s.CreatedAt, &s.UpdatedAt,
		&s.Source, &s.GoogleEventID, &s.GoogleSyncState, &s.GoogleSyncError,
		&s.Location, &s.ColorID, &s.MeetLink, &s.Timezone)
	if err != nil {
		return Schedule{}, err
	}
	s.Status = computedStatus(s.StartTime, s.EndTime, stored, now)
	s.Reminders = []int{}
	s.Attendees = []Attendee{}
	return s, nil
}

// scanScheduleRow reads the occurrence-aware projection used by Calendar and
// Today, where occurrence_id may be NULL for one-time schedules.
func scanScheduleRow(row rowScanner, now time.Time) (Schedule, error) {
	var s Schedule
	var stored string
	err := row.Scan(&s.ID, &s.OccurrenceID, &s.CategoryID, &s.Title, &s.Description,
		&s.StartTime, &s.EndTime, &s.AllDay, &stored,
		&s.Recurrence, &s.CreatedAt, &s.UpdatedAt,
		&s.Source, &s.GoogleEventID, &s.GoogleSyncState, &s.GoogleSyncError,
		&s.Location, &s.ColorID, &s.MeetLink, &s.Timezone)
	if err != nil {
		return Schedule{}, err
	}
	s.Status = computedStatus(s.StartTime, s.EndTime, stored, now)
	s.Reminders = []int{}
	s.Attendees = []Attendee{}
	return s, nil
}

// uniqueScheduleIDs collects the distinct schedule ids from a result set.
func uniqueScheduleIDs(schedules []Schedule) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(schedules))
	ids := make([]uuid.UUID, 0, len(schedules))
	for _, s := range schedules {
		if _, ok := seen[s.ID]; ok {
			continue
		}
		seen[s.ID] = struct{}{}
		ids = append(ids, s.ID)
	}
	return ids
}

// remindersForSchedules groups offset_mins by schedule id.
func remindersForSchedules(ctx context.Context, db *pgxpool.Pool, ids []uuid.UUID) (map[uuid.UUID][]int, error) {
	out := make(map[uuid.UUID][]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	rows, err := db.Query(ctx, `
		SELECT schedule_id, offset_mins
		FROM reminders
		WHERE schedule_id = ANY($1) AND dismissed = FALSE
		ORDER BY offset_mins
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var sid uuid.UUID
		var off int
		if err := rows.Scan(&sid, &off); err != nil {
			return nil, err
		}
		out[sid] = append(out[sid], off)
	}
	return out, rows.Err()
}

func (h *ScheduleHandler) fetch(c *gin.Context, id, userID uuid.UUID, now time.Time) (Schedule, error) {
	row := h.db.QueryRow(c, `
		SELECT `+scheduleColumns+`
		`+scheduleFrom+`
		WHERE s.id = $1 AND s.user_id = $2
	`, id, userID)
	return scanSchedule(row, now)
}

// remindersFor returns offset_mins grouped by schedule id for the given ids.
func (h *ScheduleHandler) remindersFor(c *gin.Context, ids []uuid.UUID) (map[uuid.UUID][]int, error) {
	return remindersForSchedules(c, h.db, ids)
}

// syncReminders replaces a schedule's reminders. One-time schedules get a
// concrete fire_at and are enqueued now; recurring schedules store templates
// (fire_at NULL) that the scheduler expands per occurrence.
func (h *ScheduleHandler) syncReminders(c *gin.Context, scheduleID, userID uuid.UUID, req scheduleRequest) error {
	if _, err := h.db.Exec(c, `DELETE FROM reminders WHERE schedule_id = $1`, scheduleID); err != nil {
		return err
	}

	recurring := req.Recurrence != ""

	for _, offset := range req.Reminders {
		if recurring {
			if _, err := h.db.Exec(c, `
				INSERT INTO reminders (schedule_id, offset_mins, fire_at)
				VALUES ($1, $2, NULL)
			`, scheduleID, offset); err != nil {
				return err
			}
			continue
		}

		fireAt := req.StartTime.Add(-time.Duration(offset) * time.Minute)

		var reminderID uuid.UUID
		err := h.db.QueryRow(c, `
			INSERT INTO reminders (schedule_id, offset_mins, fire_at)
			VALUES ($1, $2, $3)
			RETURNING id
		`, scheduleID, offset, fireAt).Scan(&reminderID)
		if err != nil {
			return err
		}

		if h.q == nil || !fireAt.After(time.Now().Add(-24*time.Hour)) {
			continue // past-due reminders are not re-fired on edit
		}

		if err := h.q.EnqueueReminder(c, queue.ReminderPayload{
			ReminderID:   reminderID,
			ScheduleID:   scheduleID,
			UserID:       userID,
			ScheduleName: req.Title,
			StartTime:    req.StartTime,
			OffsetMins:   offset,
		}, time.Until(fireAt)); err != nil {
			return err
		}
	}
	return nil
}

func validStatus(s string) bool {
	switch s {
	case StatusUpcoming, StatusOngoing, StatusCompleted, StatusMissed, StatusSkipped:
		return true
	}
	return false
}
