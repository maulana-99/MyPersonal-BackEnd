package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func todayRouter() *gin.Engine {
	h := NewTodayHandler(testDB)
	return newProtectedRouter(func(g *gin.RouterGroup) {
		g.GET("/today", h.Get)
	})
}

type todayEnvelope struct {
	Schedules struct {
		Upcoming  []Schedule `json:"upcoming"`
		Ongoing   []Schedule `json:"ongoing"`
		Completed []Schedule `json:"completed"`
		Missed    []Schedule `json:"missed"`
	} `json:"schedules"`
	Tasks []struct {
		ID       string  `json:"id"`
		Title    string  `json:"title"`
		Priority string  `json:"priority"`
		Status   string  `json:"status"`
		DueDate  *string `json:"due_date"`
	} `json:"tasks"`
	Progress struct {
		Total      int `json:"total"`
		Completed  int `json:"completed"`
		Percentage int `json:"percentage"`
	} `json:"progress"`
	NextUpcoming *Schedule `json:"next_upcoming"`
}

func TestTodayBucketsSchedules(t *testing.T) {
	requireDB(t)
	userID, token := newUser(t)
	r := todayRouter()

	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 1, 0, 0, now.Location())
	endOfDay := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 0, 0, now.Location())

	// Past schedule, not marked complete → missed. Clamp to start-of-day so a
	// run in the small hours does not push the seed into yesterday (which would
	// drop it from the today window). When the clock is genuinely too close to
	// midnight for a distinct past-today window, skip the past assertions.
	pastStart := now.Add(-3 * time.Hour)
	pastEnd := now.Add(-2 * time.Hour)
	pastInsideToday := !pastStart.Before(dayStart)
	if pastInsideToday {
		seedSchedule(t, userID, "Past", pastStart, &pastEnd, "")
	} else {
		// Anchor a past-today slot at 00:00 when the clock is just after midnight.
		pastStart = dayStart
		pastEnd = dayStart.Add(time.Minute)
		pastInsideToday = !pastEnd.After(now)
		if pastInsideToday {
			seedSchedule(t, userID, "Past", pastStart, &pastEnd, "")
		}
	}

	// Future schedule today → upcoming. Clamp to end-of-day so a run late at
	// night does not push the seed into tomorrow.
	futureStart := now.Add(3 * time.Hour)
	futureInsideToday := true
	if futureStart.After(endOfDay) {
		futureStart = endOfDay.Add(-time.Minute)
		futureInsideToday = futureStart.After(now)
	}
	if futureInsideToday {
		seedSchedule(t, userID, "Later", futureStart, nil, "")
	}

	// A completed schedule today stays completed regardless of clock.
	completedID := seedSchedule(t, userID, "Done", now.Add(-time.Minute), nil, "")
	_, err := testDB.Exec(testCtx, `UPDATE schedules SET status = 'completed' WHERE id = $1`, completedID)
	require.NoError(t, err)

	w := doJSON(t, r, http.MethodGet, "/today", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	env := decodeData[todayEnvelope](t, w)

	var upcoming, missed, completed []string
	for _, s := range env.Schedules.Upcoming {
		upcoming = append(upcoming, s.Title)
	}
	for _, s := range env.Schedules.Missed {
		missed = append(missed, s.Title)
	}
	for _, s := range env.Schedules.Completed {
		completed = append(completed, s.Title)
	}

	assert.Contains(t, completed, "Done")
	if pastInsideToday {
		assert.Contains(t, missed, "Past")
	}
	if futureInsideToday {
		assert.Contains(t, upcoming, "Later")
	}

	// total counts the seeds that landed inside today; completed counts "Done".
	wantTotal := 1 // Done
	if pastInsideToday {
		wantTotal++
	}
	if futureInsideToday {
		wantTotal++
	}
	assert.Equal(t, wantTotal, env.Progress.Total)
	assert.Equal(t, 1, env.Progress.Completed)
	assert.Equal(t, 100/wantTotal, env.Progress.Percentage)

	if futureInsideToday {
		require.NotNil(t, env.NextUpcoming)
		assert.Equal(t, "Later", env.NextUpcoming.Title)
	}
}

func TestTodayIncludesPendingTasks(t *testing.T) {
	requireDB(t)
	userID, token := newUser(t)
	r := todayRouter()

	_, err := testDB.Exec(testCtx, `
		INSERT INTO tasks (user_id, title, priority, status, due_date)
		VALUES ($1, 'Pay bills', 'high', 'pending', CURRENT_DATE)
	`, userID)
	require.NoError(t, err)
	// A completed task must not appear.
	_, err = testDB.Exec(testCtx, `
		INSERT INTO tasks (user_id, title, priority, status)
		VALUES ($1, 'Done task', 'low', 'completed')
	`, userID)
	require.NoError(t, err)

	w := doJSON(t, r, http.MethodGet, "/today", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	env := decodeData[todayEnvelope](t, w)

	require.Len(t, env.Tasks, 1)
	assert.Equal(t, "Pay bills", env.Tasks[0].Title)
	assert.Equal(t, "high", env.Tasks[0].Priority)
	require.NotNil(t, env.Tasks[0].DueDate)
}
