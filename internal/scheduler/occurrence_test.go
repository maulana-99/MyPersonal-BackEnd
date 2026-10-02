package scheduler

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOccurrencesForDaily(t *testing.T) {
	seed := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	rs := recurringSchedule{ID: uuid.New(), Rule: "FREQ=DAILY;COUNT=10", Start: seed}

	got, err := occurrencesFor(rs, seed, seed.AddDate(0, 0, 3))
	require.NoError(t, err)
	// between [seed, seed+3d] inclusive → 4 daily hits.
	require.Len(t, got, 4)
	assert.Equal(t, seed, got[0])
	assert.Equal(t, seed.AddDate(0, 0, 3), got[3])
}

func TestOccurrencesForWeeklyByDay(t *testing.T) {
	// Monday 2026-03-02 08:00 UTC.
	seed := time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC)
	rs := recurringSchedule{ID: uuid.New(), Rule: "FREQ=WEEKLY;BYDAY=MO,WE,FR;COUNT=20", Start: seed}

	got, err := occurrencesFor(rs, seed, seed.AddDate(0, 0, 7))
	require.NoError(t, err)
	require.Len(t, got, 4, "Mon, Wed, Fri, Mon over one week")
	assert.Equal(t, time.Weekday(time.Monday), got[0].Weekday())
	assert.Equal(t, time.Weekday(time.Wednesday), got[1].Weekday())
	assert.Equal(t, time.Weekday(time.Friday), got[2].Weekday())
	assert.Equal(t, time.Weekday(time.Monday), got[3].Weekday())
}

func TestOccurrencesForInvalidRule(t *testing.T) {
	rs := recurringSchedule{Rule: "NOT A RULE", Start: time.Now()}
	_, err := occurrencesFor(rs, time.Now(), time.Now().Add(time.Hour))
	assert.Error(t, err)
}

func TestExpandRecurringIdempotent(t *testing.T) {
	pool := testPool(t)

	ctx := testCtx
	var userID, scheduleID uuid.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO users (name, email, password_hash)
		VALUES ('Rec', $1, 'x') RETURNING id
	`, "rec-"+uuid.NewString()+"@example.com").Scan(&userID)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID) })

	// Recurring schedule seeded "now" so the window [now, now+60d] is non-empty.
	start := time.Now().UTC().Truncate(time.Second)
	err = pool.QueryRow(ctx, `
		INSERT INTO schedules (user_id, title, start_time, end_time)
		VALUES ($1, 'Daily', $2, $3) RETURNING id
	`, userID, start, start.Add(30*time.Minute)).Scan(&scheduleID)
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `
		INSERT INTO schedule_recurrences (schedule_id, rule) VALUES ($1, 'FREQ=DAILY;COUNT=5')
	`, scheduleID)
	require.NoError(t, err)

	n, err := expandRecurring(ctx, pool, 60)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 4)

	// Second run inserts nothing new.
	n2, err := expandRecurring(ctx, pool, 60)
	require.NoError(t, err)
	assert.Zero(t, n2, "occurrence expansion is idempotent")

	var count int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id = $1
	`, scheduleID).Scan(&count))
	assert.GreaterOrEqual(t, count, 4)
}

func TestMaterialiseOccurrenceReminders(t *testing.T) {
	pool := testPool(t)
	ctx := testCtx

	var userID, scheduleID uuid.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO users (name, email, password_hash)
		VALUES ('Rem', $1, 'x') RETURNING id
	`, "rem-"+uuid.NewString()+"@example.com").Scan(&userID)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID) })

	start := time.Now().UTC().Truncate(time.Second)
	err = pool.QueryRow(ctx, `
		INSERT INTO schedules (user_id, title, start_time) VALUES ($1, 'Daily', $2) RETURNING id
	`, userID, start).Scan(&scheduleID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO schedule_recurrences (schedule_id, rule) VALUES ($1, 'FREQ=DAILY;COUNT=3')
	`, scheduleID)
	require.NoError(t, err)
	// Reminder template: 10 minutes before, no fire_at yet.
	_, err = pool.Exec(ctx, `
		INSERT INTO reminders (schedule_id, offset_mins, fire_at) VALUES ($1, 10, NULL)
	`, scheduleID)
	require.NoError(t, err)

	_, err = expandRecurring(ctx, pool, 60)
	require.NoError(t, err)

	created, err := materialiseOccurrenceReminders(ctx, pool)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, created, 2)

	// Concrete reminders exist with a fire_at and are unsent.
	var concrete int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM reminders WHERE schedule_id = $1 AND fire_at IS NOT NULL AND sent = FALSE
	`, scheduleID).Scan(&concrete))
	assert.GreaterOrEqual(t, concrete, 2)

	// Re-running adds nothing (unique partial index).
	again, err := materialiseOccurrenceReminders(ctx, pool)
	require.NoError(t, err)
	assert.Zero(t, again)
}
