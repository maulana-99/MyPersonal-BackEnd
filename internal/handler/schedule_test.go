package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func scheduleRouter() *gin.Engine {
	h := NewScheduleHandler(testDB, nil) // nil queue: no Redis in tests
	return newProtectedRouter(func(g *gin.RouterGroup) {
		g.GET("/schedules", h.List)
		g.POST("/schedules", h.Create)
		g.GET("/schedules/:id", h.Get)
		g.PUT("/schedules/:id", h.Update)
		g.DELETE("/schedules/:id", h.Delete)
		g.POST("/schedules/:id/complete", h.Complete)
		g.POST("/schedules/:id/skip", h.Skip)
		g.POST("/schedules/:id/duplicate", h.Duplicate)
		g.POST("/schedules/:id/move", h.Move)
		g.GET("/schedules/:id/reminders", h.Reminders)
		g.POST("/schedules/:id/reminders/:rid/snooze", h.Snooze)
		g.POST("/schedules/:id/reminders/:rid/dismiss", h.DismissReminder)
	})
}

func TestScheduleCRUD(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := scheduleRouter()

	start := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	end := start.Add(time.Hour)

	w := doJSON(t, r, http.MethodPost, "/schedules", token, gin.H{
		"title":       "Dentist",
		"description": "bring card",
		"start_time":  start,
		"end_time":    end,
		"reminders":   []int{30, 0},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	created := decodeData[Schedule](t, w)
	require.NotEqual(t, uuid.Nil, created.ID)
	assert.Equal(t, "Dentist", created.Title)
	assert.Equal(t, StatusUpcoming, created.Status)
	assert.ElementsMatch(t, []int{30, 0}, created.Reminders)

	// List
	w = doJSON(t, r, http.MethodGet, "/schedules", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	all := decodeData[[]Schedule](t, w)
	require.Len(t, all, 1)

	// Get
	w = doJSON(t, r, http.MethodGet, "/schedules/"+created.ID.String(), token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, created.ID, decodeData[Schedule](t, w).ID)

	// Update (drop reminders to one)
	w = doJSON(t, r, http.MethodPut, "/schedules/"+created.ID.String(), token, gin.H{
		"title": "Dentist (moved)", "start_time": start, "end_time": end, "reminders": []int{15},
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	updated := decodeData[Schedule](t, w)
	assert.Equal(t, "Dentist (moved)", updated.Title)
	assert.Equal(t, []int{15}, updated.Reminders)

	// Complete then delete
	w = doJSON(t, r, http.MethodPost, "/schedules/"+created.ID.String()+"/complete", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, StatusCompleted, decodeData[Schedule](t, w).Status)

	w = doJSON(t, r, http.MethodDelete, "/schedules/"+created.ID.String(), token, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/schedules/"+created.ID.String(), token, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestScheduleOwnershipNotFound(t *testing.T) {
	requireDB(t)
	_, tokenA := newUser(t)
	_, tokenB := newUser(t)
	r := scheduleRouter()

	start := time.Now().Add(time.Hour).UTC()
	w := doJSON(t, r, http.MethodPost, "/schedules", tokenA, gin.H{"title": "Mine", "start_time": start})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	s := decodeData[Schedule](t, w)

	w = doJSON(t, r, http.MethodGet, "/schedules/"+s.ID.String(), tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodPut, "/schedules/"+s.ID.String(), tokenB, gin.H{"title": "Stolen", "start_time": start})
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodPost, "/schedules/"+s.ID.String()+"/complete", tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodDelete, "/schedules/"+s.ID.String(), tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/schedules", tokenB, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, decodeData[[]Schedule](t, w))
}

func TestScheduleListFilters(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := scheduleRouter()

	now := time.Now().UTC().Truncate(time.Second)
	soon := now.Add(1 * time.Hour)
	later := now.Add(72 * time.Hour)

	for _, spec := range []struct {
		title string
		start time.Time
	}{{"Soon", soon}, {"Later", later}} {
		w := doJSON(t, r, http.MethodPost, "/schedules", token, gin.H{"title": spec.title, "start_time": spec.start})
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	}

	// from/to window keeps only "Soon".
	from := now.Add(30 * time.Minute).Format(time.RFC3339)
	to := now.Add(24 * time.Hour).Format(time.RFC3339)
	w := doJSON(t, r, http.MethodGet, "/schedules?from="+from+"&to="+to, token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	list := decodeData[[]Schedule](t, w)
	require.Len(t, list, 1)
	assert.Equal(t, "Soon", list[0].Title)

	// Bad RFC3339 → 400.
	w = doJSON(t, r, http.MethodGet, "/schedules?from=not-a-date", token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestScheduleValidation(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := scheduleRouter()

	// Missing title.
	w := doJSON(t, r, http.MethodPost, "/schedules", token, gin.H{"start_time": time.Now().Add(time.Hour)})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	// end_time before start_time.
	start := time.Now().Add(2 * time.Hour)
	w = doJSON(t, r, http.MethodPost, "/schedules", token, gin.H{
		"title": "Bad", "start_time": start, "end_time": start.Add(-time.Hour),
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	// Reminder offset out of range.
	w = doJSON(t, r, http.MethodPost, "/schedules", token, gin.H{
		"title": "Bad", "start_time": start, "reminders": []int{9999},
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestScheduleDuplicateAndMove(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := scheduleRouter()

	start := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	end := start.Add(time.Hour)

	// Create a recurring schedule with reminders.
	w := doJSON(t, r, http.MethodPost, "/schedules", token, gin.H{
		"title": "Standup", "start_time": start, "end_time": end,
		"recurrence": "FREQ=DAILY", "reminders": []int{10},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	src := decodeData[Schedule](t, w)

	// Duplicate → new id, same fields, fresh upcoming status.
	w = doJSON(t, r, http.MethodPost, "/schedules/"+src.ID.String()+"/duplicate", token, nil)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	dup := decodeData[Schedule](t, w)
	require.NotEqual(t, src.ID, dup.ID)
	assert.Equal(t, "Standup", dup.Title)
	assert.Equal(t, "FREQ=DAILY", dup.Recurrence)
	assert.ElementsMatch(t, []int{10}, dup.Reminders)
	assert.Equal(t, StatusUpcoming, dup.Status)

	// Duplicate an unknown/foreign id → 404.
	w = doJSON(t, r, http.MethodPost, "/schedules/"+uuid.NewString()+"/duplicate", token, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	// Move → start/end change, reminder re-derived.
	newStart := start.Add(24 * time.Hour)
	w = doJSON(t, r, http.MethodPost, "/schedules/"+dup.ID.String()+"/move", token, gin.H{
		"start_time": newStart, "end_time": newStart.Add(30 * time.Minute),
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	moved := decodeData[Schedule](t, w)
	assert.Equal(t, newStart.UTC().Truncate(time.Second), moved.StartTime.UTC().Truncate(time.Second))

	// Move validation: end before start → 400.
	w = doJSON(t, r, http.MethodPost, "/schedules/"+dup.ID.String()+"/move", token, gin.H{
		"start_time": newStart, "end_time": newStart.Add(-time.Hour),
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestScheduleReminderSnoozeDismiss(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := scheduleRouter()

	start := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	w := doJSON(t, r, http.MethodPost, "/schedules", token, gin.H{
		"title": "Meeting", "start_time": start, "reminders": []int{15},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	s := decodeData[Schedule](t, w)

	// List reminders → one row, not dismissed.
	w = doJSON(t, r, http.MethodGet, "/schedules/"+s.ID.String()+"/reminders", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	list := decodeData[[]reminderView](t, w)
	require.Len(t, list, 1)
	require.False(t, list[0].Dismissed)
	rid := list[0].ID.String()

	// Snooze +10 minutes → fire_at pushed, dismissed stays false.
	w = doJSON(t, r, http.MethodPost, "/schedules/"+s.ID.String()+"/reminders/"+rid+"/snooze", token, gin.H{"minutes": 10})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	snoozed := decodeData[reminderView](t, w)
	require.NotNil(t, snoozed.FireAt)
	assert.False(t, snoozed.Dismissed)

	// Dismiss → dismissed true.
	w = doJSON(t, r, http.MethodPost, "/schedules/"+s.ID.String()+"/reminders/"+rid+"/dismiss", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	dismissed := decodeData[reminderView](t, w)
	assert.True(t, dismissed.Dismissed)

	// Dismiss a foreign reminder → 404.
	_, tokenB := newUser(t)
	w = doJSON(t, r, http.MethodPost, "/schedules/"+s.ID.String()+"/reminders/"+rid+"/dismiss", tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestScheduleCompleteOccurrence(t *testing.T) {
	requireDB(t)
	userID, token := newUser(t)
	r := scheduleRouter()

	// A daily schedule starting 30 minutes ago → an occurrence exists/creates.
	start := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)
	w := doJSON(t, r, http.MethodPost, "/schedules", token, gin.H{
		"title": "Standup", "start_time": start, "recurrence": "FREQ=DAILY",
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	s := decodeData[Schedule](t, w)

	// Seed an occurrence directly (the scheduler owns materialisation).
	var occID uuid.UUID
	err := testDB.QueryRow(testCtx, `
		INSERT INTO schedule_occurrences (schedule_id, occurrence_start, status)
		VALUES ($1, $2, 'upcoming') RETURNING id
	`, s.ID, start).Scan(&occID)
	require.NoError(t, err)

	// Completing with occurrence_id must update the occurrence, not the series.
	w = doJSON(t, r, http.MethodPost,
		"/schedules/"+s.ID.String()+"/complete?occurrence_id="+occID.String(), token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var occStatus, baseStatus string
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT status FROM schedule_occurrences WHERE id=$1`, occID).Scan(&occStatus))
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT status FROM schedules WHERE id=$1`, s.ID).Scan(&baseStatus))
	assert.Equal(t, StatusCompleted, occStatus)
	assert.Equal(t, StatusUpcoming, baseStatus, "base schedule must be untouched")

	// A foreign occurrence id → 404, nothing written.
	_, tokenB := newUser(t)
	w = doJSON(t, r, http.MethodPost,
		"/schedules/"+s.ID.String()+"/complete?occurrence_id="+occID.String(), tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	_ = userID
}

func TestComputedStatus(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	start := now.Add(time.Hour)
	end := start.Add(time.Hour)

	assert.Equal(t, StatusUpcoming, computedStatus(start, &end, StatusUpcoming, now))
	assert.Equal(t, StatusOngoing, computedStatus(now.Add(-time.Minute), &end, StatusUpcoming, now))
	pastEnd := now.Add(-2 * time.Hour)
	assert.Equal(t, StatusMissed, computedStatus(now.Add(-3*time.Hour), &pastEnd, StatusUpcoming, now))
	// Terminal states are sticky regardless of clock.
	assert.Equal(t, StatusCompleted, computedStatus(now.Add(-3*time.Hour), &pastEnd, StatusCompleted, now))
	assert.Equal(t, StatusSkipped, computedStatus(now.Add(-3*time.Hour), &pastEnd, StatusSkipped, now))
	// No end_time: the schedule collapses to its start instant, so it is
	// upcoming before start and missed from start onward — never ongoing.
	assert.Equal(t, StatusUpcoming, computedStatus(now.Add(time.Minute), nil, StatusUpcoming, now))
	assert.Equal(t, StatusMissed, computedStatus(now, nil, StatusUpcoming, now))
}
