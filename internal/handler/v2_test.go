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

// createHabit is a small helper so a test can obtain a habit id without
// restating the create payload.
func createHabit(t *testing.T, r http.Handler, token string, body gin.H) uuid.UUID {
	t.Helper()
	w := doJSON(t, r, http.MethodPost, "/habits", token, body)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	return decodeData[Habit](t, w).ID
}

func v2Router() *gin.Engine {
	note := NewNoteHandler(testDB)
	review := NewReviewHandler(testDB)
	rule := NewFocusRuleHandler(testDB)
	search := NewSearchHandler(testDB)

	// The sibling V1 handlers are mounted too: notes/reviews/search are only
	// meaningful against real schedules, tasks, habits, and routines.
	category := NewCategoryHandler(testDB)
	schedule := NewScheduleHandler(testDB, nil)
	task := NewTaskHandler(testDB)
	routine := NewRoutineHandler(testDB)
	habit := NewHabitHandler(testDB)

	return newProtectedRouter(func(g *gin.RouterGroup) {
		g.GET("/categories", category.List)
		g.POST("/categories", category.Create)

		g.GET("/schedules", schedule.List)
		g.POST("/schedules", schedule.Create)
		g.DELETE("/schedules/:id", schedule.Delete)

		g.GET("/tasks", task.List)
		g.POST("/tasks", task.Create)
		g.GET("/tasks/:id", task.Get)
		g.DELETE("/tasks/:id", task.Delete)

		g.GET("/routines", routine.List)
		g.POST("/routines", routine.Create)

		g.GET("/habits", habit.List)
		g.POST("/habits", habit.Create)
		g.GET("/habits/history", habit.History)
		g.GET("/habits/:id", habit.Get)
		g.POST("/habits/:id/complete", habit.Complete)
		g.GET("/habits/:id/logs", habit.Logs)

		g.GET("/notes", note.List)
		g.POST("/notes", note.Create)
		g.GET("/notes/:id", note.Get)
		g.PUT("/notes/:id", note.Update)
		g.DELETE("/notes/:id", note.Delete)

		g.GET("/daily-reviews", review.List)
		g.GET("/daily-reviews/:date", review.Get)
		g.PUT("/daily-reviews/:date", review.Upsert)

		g.GET("/focus-rules", rule.List)
		g.POST("/focus-rules", rule.Create)
		g.GET("/focus-rules/active", rule.Active)
		g.GET("/focus-rules/:id", rule.Get)
		g.PUT("/focus-rules/:id", rule.Update)
		g.DELETE("/focus-rules/:id", rule.Delete)

		g.GET("/search", search.Search)
	})
}

func TestNoteCRUDAndAttach(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := v2Router()

	// Attach to a real schedule before creating the note.
	w := doJSON(t, r, http.MethodPost, "/schedules", token, gin.H{
		"title": "Lecture", "start_time": "2026-10-01T09:00:00Z",
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	scheduleID := decodeData[Schedule](t, w).ID

	w = doJSON(t, r, http.MethodPost, "/notes", token, gin.H{
		"title": "Lecture notes", "body": "Chapter 4", "schedule_id": scheduleID,
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	n := decodeData[Note](t, w)
	require.NotNil(t, n.ScheduleID)
	assert.Equal(t, scheduleID, *n.ScheduleID)

	// Filter by attachment.
	w = doJSON(t, r, http.MethodGet, "/notes?schedule_id="+scheduleID.String(), token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Len(t, decodeData[[]Note](t, w), 1)

	// Detach with null.
	w = doJSON(t, r, http.MethodPut, "/notes/"+n.ID.String(), token, gin.H{
		"title": "Lecture notes", "body": "Chapter 4",
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Nil(t, decodeData[Note](t, w).ScheduleID)

	w = doJSON(t, r, http.MethodDelete, "/notes/"+n.ID.String(), token, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
}

func TestNoteCannotAttachForeignSchedule(t *testing.T) {
	requireDB(t)
	_, tokenA := newUser(t)
	_, tokenB := newUser(t)
	r := v2Router()

	w := doJSON(t, r, http.MethodPost, "/schedules", tokenA, gin.H{
		"title": "A private", "start_time": "2026-10-01T09:00:00Z",
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	scheduleID := decodeData[Schedule](t, w).ID

	w = doJSON(t, r, http.MethodPost, "/notes", tokenB, gin.H{
		"title": "Sneaky", "schedule_id": scheduleID,
	})
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestNoteSurvivesParentDeletion(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := v2Router()

	w := doJSON(t, r, http.MethodPost, "/tasks", token, gin.H{"title": "Task with note"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	taskID := decodeData[Task](t, w).ID

	w = doJSON(t, r, http.MethodPost, "/notes", token, gin.H{"title": "Note", "task_id": taskID})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	noteID := decodeData[Note](t, w).ID

	// Deleting the task must detach, not delete, the note.
	w = doJSON(t, r, http.MethodDelete, "/tasks/"+taskID.String(), token, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/notes/"+noteID.String(), token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Nil(t, decodeData[Note](t, w).TaskID)
}

func TestDailyReviewUpsertAndSummary(t *testing.T) {
	requireDB(t)
	userID, token := newUser(t)
	r := v2Router()

	today := todayLocal().Format(dayFormat)

	// An unwritten day still returns a summary, with empty content.
	w := doJSON(t, r, http.MethodGet, "/daily-reviews/"+today, token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeData[DailyReview](t, w)
	assert.Equal(t, today, got.Date)
	assert.Empty(t, got.Content)

	// Seed a completed schedule and task for today so the summary is non-zero.
	_, err := testDB.Exec(testCtx, `
		INSERT INTO schedules (user_id, title, start_time, end_time, status)
		VALUES ($1, 'Done today', NOW() - interval '2 hours', NOW() - interval '1 hour', 'completed')
	`, userID)
	require.NoError(t, err)
	_, err = testDB.Exec(testCtx, `
		INSERT INTO tasks (user_id, title, status, updated_at)
		VALUES ($1, 'Task done today', 'completed', NOW())
	`, userID)
	require.NoError(t, err)

	// Upsert twice: the second write must update, not duplicate.
	for _, content := range []string{"First pass", "Second pass"} {
		w = doJSON(t, r, http.MethodPut, "/daily-reviews/"+today, token, gin.H{"content": content})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		got = decodeData[DailyReview](t, w)
		assert.Equal(t, content, got.Content)
		assert.Equal(t, 1, got.Summary.SchedulesCompleted)
		assert.Equal(t, 1, got.Summary.TasksCompleted)
	}

	// The content is the only persisted field: the summary is derived on read.
	var count int
	require.NoError(t, testDB.QueryRow(testCtx,
		`SELECT COUNT(*)::int FROM daily_reviews WHERE user_id = $1`, userID).Scan(&count))
	assert.Equal(t, 1, count, "upsert must not create a second row")

	w = doJSON(t, r, http.MethodGet, "/daily-reviews", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Len(t, decodeData[[]DailyReview](t, w), 1)

	w = doJSON(t, r, http.MethodGet, "/daily-reviews/not-a-date", token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestFocusRuleCRUDAndActiveWindow(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := v2Router()

	w := doJSON(t, r, http.MethodPost, "/focus-rules", token, gin.H{
		"name": "Deep Work", "start_time": "09:00", "end_time": "11:00",
		"days":  []string{"mon", "tue", "wed", "thu", "fri"},
		"sites": []string{"youtube.com", "reddit.com"},
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	rule := decodeData[FocusRule](t, w)
	assert.Equal(t, "09:00", rule.StartTime)
	assert.True(t, rule.Enabled)

	// Overnight windows are rejected (documented V2 limitation).
	w = doJSON(t, r, http.MethodPost, "/focus-rules", token, gin.H{
		"name": "Overnight", "start_time": "22:00", "end_time": "02:00",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodPost, "/focus-rules", token, gin.H{
		"name": "Bad time", "start_time": "9am", "end_time": "11:00",
	})
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	// A full-day enabled rule is always active right now.
	w = doJSON(t, r, http.MethodPost, "/focus-rules", token, gin.H{
		"name": "Always", "start_time": "00:00", "end_time": "23:59",
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	always := decodeData[FocusRule](t, w)

	w = doJSON(t, r, http.MethodGet, "/focus-rules/active", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	active := decodeData[[]FocusRule](t, w)
	require.Len(t, active, 1)
	assert.Equal(t, always.ID, active[0].ID)

	// Disabling it removes it from the active set.
	w = doJSON(t, r, http.MethodPut, "/focus-rules/"+always.ID.String(), token, gin.H{
		"name": "Always", "start_time": "00:00", "end_time": "23:59", "enabled": false,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/focus-rules/active", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Empty(t, decodeData[[]FocusRule](t, w))

	w = doJSON(t, r, http.MethodDelete, "/focus-rules/"+rule.ID.String(), token, nil)
	require.Equal(t, http.StatusNoContent, w.Code, w.Body.String())
}

func TestSearchAcrossDomains(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := v2Router()

	// One hit per domain, all matching "skripsi".
	w := doJSON(t, r, http.MethodPost, "/schedules", token, gin.H{
		"title": "Bimbingan skripsi", "start_time": "2026-10-01T09:00:00Z",
	})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	w = doJSON(t, r, http.MethodPost, "/tasks", token, gin.H{"title": "Revisi skripsi bab 3"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	w = doJSON(t, r, http.MethodPost, "/habits", token, gin.H{"name": "Baca skripsi referensi"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	w = doJSON(t, r, http.MethodPost, "/routines", token, gin.H{"name": "Rutin skripsi pagi"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	w = doJSON(t, r, http.MethodPost, "/notes", token, gin.H{"title": "Catatan skripsi", "body": "bab 4"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/search?q=skripsi", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	hits := decodeData[[]SearchResult](t, w)
	require.Len(t, hits, 5)

	types := map[string]bool{}
	for _, h := range hits {
		types[h.Type] = true
	}
	for _, want := range []string{"schedule", "task", "habit", "routine", "note"} {
		assert.True(t, types[want], "expected a %s hit", want)
	}

	// Type filter narrows to one source.
	w = doJSON(t, r, http.MethodGet, "/search?q=skripsi&type=task", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	filtered := decodeData[[]SearchResult](t, w)
	require.Len(t, filtered, 1)
	assert.Equal(t, "task", filtered[0].Type)

	// Body text is searchable for notes.
	w = doJSON(t, r, http.MethodGet, "/search?q=bab%204", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Len(t, decodeData[[]SearchResult](t, w), 1)

	// Short needles are rejected rather than scanning everything.
	w = doJSON(t, r, http.MethodGet, "/search?q=a", token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/search?q=skripsi&type=unknown", token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	// Wildcards in the needle are literal characters, not patterns.
	w = doJSON(t, r, http.MethodGet, "/search?q=%25%25", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Empty(t, decodeData[[]SearchResult](t, w))
}

func TestSearchStatusFilterUsesComputedScheduleStatus(t *testing.T) {
	requireDB(t)
	userID, token := newUser(t)
	r := v2Router()

	// A past schedule that was never completed is "missed" by the clock, but its
	// STORED status is the default "upcoming". Filtering must agree with what the
	// API reports, otherwise ?status=missed silently returns nothing.
	var pastID uuid.UUID
	require.NoError(t, testDB.QueryRow(testCtx, `
		INSERT INTO schedules (user_id, title, start_time, end_time)
		VALUES ($1, 'Konsultasi missed', NOW() - interval '3 hours', NOW() - interval '2 hours')
		RETURNING id
	`, userID).Scan(&pastID))

	upcoming := seedSchedule(t, userID, "Konsultasi upcoming",
		time.Now().Add(5*time.Hour), new(time.Now().Add(6*time.Hour)), "")

	w := doJSON(t, r, http.MethodGet, "/search?q=konsultasi", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	unfiltered := decodeData[[]SearchResult](t, w)
	require.Len(t, unfiltered, 2)

	byTitle := map[string]string{}
	for _, hit := range unfiltered {
		byTitle[hit.Title] = hit.Status
	}
	assert.Equal(t, "missed", byTitle["Konsultasi missed"], "status is computed, not the stored column")
	assert.Equal(t, "upcoming", byTitle["Konsultasi upcoming"])

	w = doJSON(t, r, http.MethodGet, "/search?q=konsultasi&status=missed", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	missed := decodeData[[]SearchResult](t, w)
	require.Len(t, missed, 1)
	assert.Equal(t, pastID, missed[0].ID)

	w = doJSON(t, r, http.MethodGet, "/search?q=konsultasi&status=upcoming", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	soon := decodeData[[]SearchResult](t, w)
	require.Len(t, soon, 1)
	assert.Equal(t, upcoming, soon[0].ID)

	// A status filter cannot match types that have no status.
	w = doJSON(t, r, http.MethodGet, "/search?q=konsultasi&status=bogus", token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestSearchStatusExcludesStatuslessTypes(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := v2Router()

	w := doJSON(t, r, http.MethodPost, "/tasks", token, gin.H{"title": "Target pending"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	w = doJSON(t, r, http.MethodPost, "/habits", token, gin.H{"name": "Target habit"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	w = doJSON(t, r, http.MethodPost, "/routines", token, gin.H{"name": "Target rutin"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	w = doJSON(t, r, http.MethodPost, "/notes", token, gin.H{"title": "Target note"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	// Unfiltered: all four types match the text.
	w = doJSON(t, r, http.MethodGet, "/search?q=target", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Len(t, decodeData[[]SearchResult](t, w), 4)

	// With a status: only the task can match, so the rest are excluded rather
	// than quietly ignoring the filter.
	w = doJSON(t, r, http.MethodGet, "/search?q=target&status=pending", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	filtered := decodeData[[]SearchResult](t, w)
	require.Len(t, filtered, 1)
	assert.Equal(t, "task", filtered[0].Type)
}

func TestHabitHistoryBatchesEveryHabit(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	r := v2Router()

	a := createHabit(t, r, token, gin.H{"name": "Hist A", "target_count": 1})
	b := createHabit(t, r, token, gin.H{"name": "Hist B", "target_count": 1})
	// A third habit with no logs must be absent from the history array.
	createHabit(t, r, token, gin.H{"name": "Hist C empty", "target_count": 1})

	for _, day := range []string{"2026-09-20", "2026-09-21"} {
		w := doJSON(t, r, http.MethodPost, "/habits/"+a.String()+"/complete", token,
			gin.H{"date": day, "delta": 1})
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
	w := doJSON(t, r, http.MethodPost, "/habits/"+b.String()+"/complete", token,
		gin.H{"date": "2026-09-22", "delta": 1})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// Window covering all of them, in one call for every habit.
	w = doJSON(t, r, http.MethodGet, "/habits/history?days=400", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	history := decodeData[[]HabitHistory](t, w)

	byHabit := map[uuid.UUID][]HabitLog{}
	for _, h := range history {
		byHabit[h.HabitID] = h.Days
	}
	require.Len(t, byHabit, 2, "the habit with no logs is absent, not empty")
	assert.Len(t, byHabit[a], 2)
	assert.Len(t, byHabit[b], 1)
	assert.Equal(t, "2026-09-20", byHabit[a][0].Date, "logs ascend by date")
	assert.Equal(t, 1, byHabit[a][0].Count)

	// A narrow window drops the older log rather than erroring.
	w = doJSON(t, r, http.MethodGet, "/habits/history?days=1", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/habits/history?days=0", token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	w = doJSON(t, r, http.MethodGet, "/habits/history?days=401", token, nil)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestHabitHistoryIsUserScoped(t *testing.T) {
	requireDB(t)
	_, tokenA := newUser(t)
	_, tokenB := newUser(t)
	r := v2Router()

	a := createHabit(t, r, tokenA, gin.H{"name": "Private habit", "target_count": 1})
	w := doJSON(t, r, http.MethodPost, "/habits/"+a.String()+"/complete", tokenA,
		gin.H{"date": "2026-09-20", "delta": 1})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/habits/history?days=400", tokenB, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Empty(t, decodeData[[]HabitHistory](t, w))
}

func TestDailyReviewIncludesTomorrow(t *testing.T) {
	requireDB(t)
	userID, token := newUser(t)
	r := v2Router()

	today := todayLocal()
	reviewDate := today.Format(dayFormat)

	// One schedule today (must not leak into tomorrow) and two tomorrow.
	seedSchedule(t, userID, "Hari ini", today.Add(10*time.Hour), nil, "")
	seedSchedule(t, userID, "Besok pagi", today.AddDate(0, 0, 1).Add(9*time.Hour), nil, "")
	seedSchedule(t, userID, "Besok malam", today.AddDate(0, 0, 1).Add(19*time.Hour), nil, "")
	// The day after tomorrow must not appear.
	seedSchedule(t, userID, "Lusa", today.AddDate(0, 0, 2).Add(9*time.Hour), nil, "")

	w := doJSON(t, r, http.MethodGet, "/daily-reviews/"+reviewDate, token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeData[DailyReview](t, w)

	require.Len(t, got.Summary.Tomorrow, 2, "only the next day's schedules")
	assert.Equal(t, "Besok pagi", got.Summary.Tomorrow[0].Title, "ordered by start time")
	assert.Equal(t, "Besok malam", got.Summary.Tomorrow[1].Title)

	// Tomorrow is relative to the REVIEWED date, not to now.
	otherDay := today.AddDate(0, 0, -3).Format(dayFormat)
	w = doJSON(t, r, http.MethodGet, "/daily-reviews/"+otherDay, token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	other := decodeData[DailyReview](t, w)
	assert.Empty(t, other.Summary.Tomorrow)
}

func TestScheduleListStatusFilterMatchesReportedStatus(t *testing.T) {
	requireDB(t)
	userID, token := newUser(t)
	h := NewScheduleHandler(testDB, nil)
	r := newProtectedRouter(func(g *gin.RouterGroup) {
		g.GET("/schedules", h.List)
	})

	// Stored status stays "upcoming"; only the clock makes it missed.
	require.NoError(t, testDB.QueryRow(testCtx, `
		INSERT INTO schedules (user_id, title, start_time, end_time)
		VALUES ($1, 'Terlewat', NOW() - interval '3 hours', NOW() - interval '2 hours')
		RETURNING id
	`, userID).Scan(new(uuid.UUID)))
	seedSchedule(t, userID, "Akan datang", time.Now().Add(3*time.Hour), nil, "")

	w := doJSON(t, r, http.MethodGet, "/schedules?status=missed", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	missed := decodeData[[]Schedule](t, w)
	require.Len(t, missed, 1)
	assert.Equal(t, "Terlewat", missed[0].Title)
	assert.Equal(t, "missed", missed[0].Status, "filtered bucket agrees with the reported status")

	// Every bucket is internally consistent: no row whose computed status
	// disagrees with the bucket that returned it.
	for _, want := range []string{"upcoming", "ongoing", "completed", "missed", "skipped"} {
		w = doJSON(t, r, http.MethodGet, "/schedules?status="+want, token, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		for _, s := range decodeData[[]Schedule](t, w) {
			assert.Equal(t, want, s.Status)
		}
	}
}

func TestSearchIsUserScoped(t *testing.T) {
	requireDB(t)
	_, tokenA := newUser(t)
	_, tokenB := newUser(t)
	r := v2Router()

	w := doJSON(t, r, http.MethodPost, "/tasks", tokenA, gin.H{"title": "Secret research"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	w = doJSON(t, r, http.MethodGet, "/search?q=research", tokenB, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Empty(t, decodeData[[]SearchResult](t, w))
}
