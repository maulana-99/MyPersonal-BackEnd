package handler

import (
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func habitRouter() *gin.Engine {
	h := NewHabitHandler(testDB)
	return newProtectedRouter(func(g *gin.RouterGroup) {
		g.GET("/habits", h.List)
		g.POST("/habits", h.Create)
		g.GET("/habits/:id", h.Get)
		g.PUT("/habits/:id", h.Update)
		g.DELETE("/habits/:id", h.Delete)
		g.POST("/habits/:id/complete", h.Complete)
		g.GET("/habits/:id/logs", h.Logs)
	})
}

func TestHabitCRUDAndCompletion(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := habitRouter()

	w := doJSON(t, r, http.MethodPost, "/habits", token, gin.H{
		"name": "Drink water", "target_type": "daily", "target_count": 2,
		"repeat_days": []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	hb := decodeData[Habit](t, w)
	require.Equal(t, "Drink water", hb.Name)
	require.Equal(t, 2, hb.TargetCount)
	require.Equal(t, 0, hb.TodayCount)
	assert.False(t, hb.DoneToday)

	id := hb.ID.String()

	// First unit of two: logged, but the habit is not done yet.
	w = doJSON(t, r, http.MethodPost, "/habits/"+id+"/complete", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	hb = decodeData[Habit](t, w)
	assert.Equal(t, 1, hb.TodayCount)
	assert.False(t, hb.DoneToday, "target is 2, one unit is not done")
	assert.Equal(t, 0, hb.Streak, "an unmet target is not a streak day")

	// Second unit: done, streak 1.
	w = doJSON(t, r, http.MethodPost, "/habits/"+id+"/complete", token, gin.H{"delta": 1})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	hb = decodeData[Habit](t, w)
	assert.Equal(t, 2, hb.TodayCount)
	assert.True(t, hb.DoneToday)
	assert.Equal(t, 1, hb.Streak)

	// Absolute set back to zero deletes the row.
	w = doJSON(t, r, http.MethodPost, "/habits/"+id+"/complete", token, gin.H{"count": 0})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	hb = decodeData[Habit](t, w)
	assert.Equal(t, 0, hb.TodayCount)
	assert.False(t, hb.DoneToday)

	// A negative delta clamps to zero, never below.
	w = doJSON(t, r, http.MethodPost, "/habits/"+id+"/complete", token, gin.H{"delta": -5})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 0, decodeData[Habit](t, w).TodayCount)

	// Logs sub-resource returns just the days that were written.
	w = doJSON(t, r, http.MethodPost, "/habits/"+id+"/complete", token, gin.H{"delta": 1, "date": "2026-09-20"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/habits/"+id+"/logs", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	logs := decodeData[[]HabitLog](t, w)
	require.Len(t, logs, 1)
	assert.Equal(t, "2026-09-20", logs[0].Date)

	// Update and delete.
	w = doJSON(t, r, http.MethodPut, "/habits/"+id, token, gin.H{
		"name": "Drink more water", "target_count": 3,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "Drink more water", decodeData[Habit](t, w).Name)

	w = doJSON(t, r, http.MethodDelete, "/habits/"+id, token, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
}

func TestHabitStreakSkipsUnscheduledDays(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := habitRouter()

	// Scheduled only on Mondays and Wednesdays. Tue/Thu–Sun are not due, so
	// they must not break the run — that is the whole point of repeat_days.
	w := doJSON(t, r, http.MethodPost, "/habits", token, gin.H{
		"name": "Gym", "target_type": "daily", "repeat_days": []string{"mon", "wed"},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	id := decodeData[Habit](t, w).ID.String()

	// The three most recent due days (today included when today is due).
	due := []time.Time{}
	for d := todayLocal(); len(due) < 3; d = d.AddDate(0, 0, -1) {
		if wd := weekdayKey(d); wd == "mon" || wd == "wed" {
			due = append(due, d)
		}
	}

	for _, day := range due {
		w = doJSON(t, r, http.MethodPost, "/habits/"+id+"/complete", token, gin.H{
			"date": day.Format(dayFormat), "delta": 1,
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}

	w = doJSON(t, r, http.MethodGet, "/habits/"+id, token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	hb := decodeData[Habit](t, w)
	assert.Equal(t, 3, hb.Streak, "3 consecutive DUE days: the Tue gap is not a miss")
	assert.Equal(t, 3, hb.BestStreak)
	assert.Equal(t, 3, hb.TotalCompletions)

	// Rate is measured over the scheduled days since the earliest log.
	// Due days in that span: the 3 completed + the gap days that are not due
	// are excluded, so a 3-of-3 record is 100%.
	assert.Equal(t, 100, hb.CompletionRate)
}

func TestHabitDayStreakBreaksOnAMissedDueDay(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := habitRouter()

	w := doJSON(t, r, http.MethodPost, "/habits", token, gin.H{
		"name": "Daily read", "target_type": "daily", "target_count": 1,
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	id := decodeData[Habit](t, w).ID.String()

	// Complete today and the day before yesterday — yesterday (a due day) was
	// missed, so the live streak is just today.
	for _, offset := range []int{0, -2} {
		w = doJSON(t, r, http.MethodPost, "/habits/"+id+"/complete", token, gin.H{
			"date": todayLocal().AddDate(0, 0, offset).Format(dayFormat), "delta": 1,
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}

	w = doJSON(t, r, http.MethodGet, "/habits/"+id, token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	hb := decodeData[Habit](t, w)
	assert.Equal(t, 1, hb.Streak, "yesterday was a missed due day and broke the run")
	assert.Equal(t, 1, hb.BestStreak)
	assert.Equal(t, 2, hb.TotalCompletions)
}

func TestHabitOwnershipIsolation(t *testing.T) {
	requireDB(t)
	_, tokenA := newUser(t)
	_, tokenB := newUser(t)
	r := habitRouter()

	w := doJSON(t, r, http.MethodPost, "/habits", tokenA, gin.H{"name": "Private", "target_count": 1})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	id := decodeData[Habit](t, w).ID.String()

	w = doJSON(t, r, http.MethodPost, "/habits/"+id+"/complete", tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/habits/"+id, tokenB, nil)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodPut, "/habits/"+id, tokenB, gin.H{"name": "Stolen"})
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestHabitValidation(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := habitRouter()

	w := doJSON(t, r, http.MethodPost, "/habits", token, gin.H{"name": ""})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodPost, "/habits", token, gin.H{"name": "X", "repeat_days": []string{"funday"}})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodPost, "/habits", token, gin.H{"name": "X", "target_type": "hourly"})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestHabitWeeklyStreak(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := habitRouter()

	w := doJSON(t, r, http.MethodPost, "/habits", token, gin.H{
		"name": "Workout", "target_type": "weekly", "target_count": 3,
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	id := decodeData[Habit](t, w).ID.String()

	// Three sessions this week meets the weekly target.
	weekStartDay := weekStart(todayLocal())
	for i := 0; i < 3; i++ {
		day := weekStartDay.AddDate(0, 0, i)
		if day.After(todayLocal()) {
			break
		}
		w = doJSON(t, r, http.MethodPost, "/habits/"+id+"/complete", token, gin.H{
			"date": day.Format(dayFormat), "delta": 1,
		})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}

	w = doJSON(t, r, http.MethodGet, "/habits/"+id, token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	hb := decodeData[Habit](t, w)
	if todayLocal().Sub(weekStartDay) >= 48*time.Hour {
		assert.Equal(t, 1, hb.Streak, "this week met its quota")
	}
	assert.Equal(t, 3, hb.TotalCompletions)
}
