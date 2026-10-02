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

func notificationRouter() *gin.Engine {
	h := NewNotificationHandler(testDB)
	return newProtectedRouter(func(g *gin.RouterGroup) {
		g.GET("/notifications", h.List)
		g.POST("/notifications/:id/read", h.Read)
		g.POST("/notifications/:id/dismiss", h.Dismiss)
		g.POST("/notifications/read-all", h.ReadAll)
	})
}

func seedNotification(t *testing.T, userID uuid.UUID, title string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := testDB.QueryRow(testCtx, `
		INSERT INTO notifications (user_id, title, body) VALUES ($1, $2, 'body') RETURNING id
	`, userID, title).Scan(&id)
	require.NoError(t, err)
	return id
}

func TestNotificationLifecycle(t *testing.T) {
	requireDB(t)
	userID, token := newUser(t)
	r := notificationRouter()

	a := seedNotification(t, userID, "First")
	seedNotification(t, userID, "Second")

	w := doJSON(t, r, http.MethodGet, "/notifications", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	list := decodeData[[]Notification](t, w)
	require.Len(t, list, 2)

	w = doJSON(t, r, http.MethodPost, "/notifications/"+a.String()+"/read", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, decodeData[Notification](t, w).IsRead)

	// Dismiss behaves like read for persisted state.
	w = doJSON(t, r, http.MethodPost, "/notifications/"+a.String()+"/dismiss", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, decodeData[Notification](t, w).IsRead)

	w = doJSON(t, r, http.MethodPost, "/notifications/read-all", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/notifications", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	for _, n := range decodeData[[]Notification](t, w) {
		assert.True(t, n.IsRead, "read-all marks every notification read")
	}
}

func TestNotificationOwnership(t *testing.T) {
	requireDB(t)
	userIDA, _ := newUser(t)
	_, tokenB := newUser(t)
	r := notificationRouter()

	n := seedNotification(t, userIDA, "Private")

	w := doJSON(t, r, http.MethodPost, "/notifications/"+n.String()+"/read", tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestStatisticsAggregates(t *testing.T) {
	requireDB(t)
	userID, token := newUser(t)
	r := newProtectedRouter(func(g *gin.RouterGroup) {
		g.GET("/statistics", NewStatisticsHandler(testDB).Get)
	})

	// Recent schedules: one completed, one pending-past (missed), one upcoming.
	now := time.Now()
	completed := seedSchedule(t, userID, "Done", now.Add(-2*time.Hour), new(now.Add(-time.Hour)), "")
	_, err := testDB.Exec(testCtx, `UPDATE schedules SET status='completed' WHERE id=$1`, completed)
	require.NoError(t, err)
	seedSchedule(t, userID, "Missed", now.Add(-3*time.Hour), new(now.Add(-2*time.Hour)), "")
	seedSchedule(t, userID, "Upcoming", now.Add(time.Hour), nil, "")

	// One completed task in the window.
	_, err = testDB.Exec(testCtx, `
		INSERT INTO tasks (user_id, title, status, updated_at)
		VALUES ($1, 'Task done', 'completed', NOW())
	`, userID)
	require.NoError(t, err)

	w := doJSON(t, r, http.MethodGet, "/statistics?period=week", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	stats := decodeData[Statistics](t, w)

	assert.Equal(t, "week", stats.Period)
	assert.Equal(t, 3, stats.TotalSchedules)
	assert.Equal(t, 1, stats.CompletedSchedules)
	assert.Equal(t, 1, stats.MissedSchedules)
	assert.Equal(t, 33, stats.ScheduleCompletionRate)
	assert.Equal(t, 1, stats.CompletedTasks)
	assert.Equal(t, 0, stats.FocusTime, "no focus session was completed in this test")

	// The per-day series must be exactly the window: 7 days for a week, 1 for a
	// day, ending on the caller's local today. This guards the day-boundary
	// arithmetic (a generate_series over `NOW() - interval` produced 8 rows).
	require.Len(t, stats.Daily, 7)
	assert.Equal(t, todayLocal().Format(dayFormat), stats.Daily[len(stats.Daily)-1].Date)

	// Focus time becomes real once a session is completed.
	_, err = testDB.Exec(testCtx, `
		INSERT INTO focus_sessions (user_id, title, planned_minutes, started_at, ended_at,
		                            status, actual_minutes)
		VALUES ($1, 'Measured block', 50, NOW() - interval '1 hour', NOW(), 'completed', 47)
	`, userID)
	require.NoError(t, err)

	w = doJSON(t, r, http.MethodGet, "/statistics?period=week", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	stats = decodeData[Statistics](t, w)
	assert.Equal(t, 47, stats.FocusTime, "focus_time is measured, not hard-coded zero")
	assert.Equal(t, 1, stats.FocusSessions)

	w = doJSON(t, r, http.MethodGet, "/statistics?period=day", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, decodeData[Statistics](t, w).Daily, 1)

	w = doJSON(t, r, http.MethodGet, "/statistics?period=year", token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}
