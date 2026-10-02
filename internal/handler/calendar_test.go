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

func calendarRouter() *gin.Engine {
	h := NewCalendarHandler(testDB)
	return newProtectedRouter(func(g *gin.RouterGroup) {
		g.GET("/calendar", h.List)
	})
}

type calendarEnvelope struct {
	View      string     `json:"view"`
	Start     time.Time  `json:"start"`
	End       time.Time  `json:"end"`
	Schedules []Schedule `json:"schedules"`
}

// seedSchedule inserts a schedule directly so tests can place it anywhere in
// time without fighting the create endpoint's validation.
func seedSchedule(t *testing.T, userID uuid.UUID, title string, start time.Time, end *time.Time, recurrence string) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	err := testDB.QueryRow(testCtx, `
		INSERT INTO schedules (user_id, title, start_time, end_time)
		VALUES ($1, $2, $3, $4) RETURNING id
	`, userID, title, start, end).Scan(&id)
	require.NoError(t, err)

	if recurrence != "" {
		_, err := testDB.Exec(testCtx, `
			INSERT INTO schedule_recurrences (schedule_id, rule) VALUES ($1, $2)
		`, id, recurrence)
		require.NoError(t, err)
	}
	return id
}

func TestCalendarOverlapWindow(t *testing.T) {
	requireDB(t)
	userID, token := newUser(t)
	r := calendarRouter()

	base := time.Now().UTC().Truncate(time.Second)
	windowStart := base.Add(24 * time.Hour)
	windowEnd := windowStart.Add(24 * time.Hour)

	mk := func(offset time.Duration) *time.Time {
		e := base.Add(offset)
		return &e
	}
	_ = mk

	// Fully inside.
	inside := windowStart.Add(time.Hour)
	insideEnd := inside.Add(time.Hour)
	seedSchedule(t, userID, "Inside", inside, &insideEnd, "")

	// Straddles the window start.
	crossStart := windowStart.Add(-30 * time.Minute)
	crossEnd := windowStart.Add(30 * time.Minute)
	seedSchedule(t, userID, "CrossStart", crossStart, &crossEnd, "")

	// Ends exactly at window start → excluded (half-open interval).
	endsAtStart := windowStart.Add(-time.Hour)
	seedSchedule(t, userID, "EndsAtStart", endsAtStart, &windowStart, "")

	// Starts exactly at window end → excluded.
	seedSchedule(t, userID, "StartsAtEnd", windowEnd, nil, "")

	// Entirely before.
	beforeEnd := windowStart.Add(-2 * time.Hour)
	seedSchedule(t, userID, "Before", windowStart.Add(-3*time.Hour), &beforeEnd, "")

	url := "/calendar?start=" + windowStart.Format(time.RFC3339) + "&end=" + windowEnd.Format(time.RFC3339) + "&view=day"
	w := doJSON(t, r, http.MethodGet, url, token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	env := decodeData[calendarEnvelope](t, w)
	titles := make([]string, 0, len(env.Schedules))
	for _, s := range env.Schedules {
		titles = append(titles, s.Title)
	}
	assert.ElementsMatch(t, []string{"Inside", "CrossStart"}, titles)
	assert.Equal(t, "day", env.View)
}

func TestCalendarRecurringOccurrences(t *testing.T) {
	requireDB(t)
	userID, token := newUser(t)
	r := calendarRouter()

	// Daily rule seeded from 10:00 today UTC; occurrences materialised by hand.
	seed := time.Now().UTC().Truncate(time.Second).Add(2 * time.Hour)
	scheduleID := seedSchedule(t, userID, "Daily standup", seed, nil, "FREQ=DAILY;COUNT=10")

	for i := 0; i < 3; i++ {
		_, err := testDB.Exec(testCtx, `
			INSERT INTO schedule_occurrences (schedule_id, occurrence_start)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING
		`, scheduleID, seed.AddDate(0, 0, i))
		require.NoError(t, err)
	}

	from := seed.Add(-time.Hour)
	to := seed.AddDate(0, 0, 5)
	url := "/calendar?start=" + from.Format(time.RFC3339) + "&end=" + to.Format(time.RFC3339) + "&view=month"
	w := doJSON(t, r, http.MethodGet, url, token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	env := decodeData[calendarEnvelope](t, w)
	require.Len(t, env.Schedules, 3, "one row per materialised occurrence")
	for _, s := range env.Schedules {
		assert.Equal(t, scheduleID, s.ID)
		require.NotNil(t, s.OccurrenceID, "occurrence rows carry an occurrence_id")
	}
}

func TestCalendarValidation(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := calendarRouter()

	now := time.Now().UTC()
	valid := "start=" + now.Format(time.RFC3339) + "&end=" + now.Add(time.Hour).Format(time.RFC3339)

	w := doJSON(t, r, http.MethodGet, "/calendar?"+valid+"&view=year", token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/calendar?start=nope&end="+now.Format(time.RFC3339), token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	// end before start.
	w = doJSON(t, r, http.MethodGet, "/calendar?start="+now.Format(time.RFC3339)+"&end="+now.Add(-time.Hour).Format(time.RFC3339), token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}
