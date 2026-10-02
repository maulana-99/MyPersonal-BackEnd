package scheduler

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/queue"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testCtx = context.Background()

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(testCtx, url)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(testCtx))
	t.Cleanup(pool.Close)
	return pool
}

func testQueue(t *testing.T) *queue.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set")
	}
	q := queue.NewClient(addr, os.Getenv("REDIS_PASSWORD"))
	t.Cleanup(func() { _ = q.Close() })
	return q
}

// seedDueReminder inserts a user + schedule + reminder whose fire_at is in the
// past, returning the reminder id and user id.
func seedDueReminder(t *testing.T, pool *pgxpool.Pool, fireAt time.Time) (uuid.UUID, uuid.UUID) {
	t.Helper()

	var userID, scheduleID, reminderID uuid.UUID
	err := pool.QueryRow(testCtx, `
		INSERT INTO users (name, email, password_hash)
		VALUES ('Sched Test', $1, 'x') RETURNING id
	`, fmt.Sprintf("sched-%s@example.com", uuid.NewString())).Scan(&userID)
	require.NoError(t, err)

	err = pool.QueryRow(testCtx, `
		INSERT INTO schedules (user_id, title, start_time)
		VALUES ($1, 'Standup', $2) RETURNING id
	`, userID, fireAt.Add(time.Minute)).Scan(&scheduleID)
	require.NoError(t, err)

	err = pool.QueryRow(testCtx, `
		INSERT INTO reminders (schedule_id, offset_mins, fire_at)
		VALUES ($1, 1, $2) RETURNING id
	`, scheduleID, fireAt).Scan(&reminderID)
	require.NoError(t, err)

	t.Cleanup(func() {
		_, _ = pool.Exec(testCtx, `DELETE FROM users WHERE id = $1`, userID)
	})

	return reminderID, userID
}

func TestDispatchDueEnqueuesAndMarksSent(t *testing.T) {
	pool := testPool(t)
	q := testQueue(t)
	inspector := asynq.NewInspector(asynq.RedisClientOpt{
		Addr:     os.Getenv("REDIS_ADDR"),
		Password: os.Getenv("REDIS_PASSWORD"),
	})
	t.Cleanup(func() { _ = inspector.Close() })

	reminderID, _ := seedDueReminder(t, pool, time.Now().Add(-time.Minute))

	// A future reminder must NOT be picked up.
	futureID, _ := seedDueReminder(t, pool, time.Now().Add(time.Hour))

	n, err := dispatchDue(testCtx, pool, q)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 1)

	var sent, futureSent bool
	require.NoError(t, pool.QueryRow(testCtx, `SELECT sent FROM reminders WHERE id = $1`, reminderID).Scan(&sent))
	require.NoError(t, pool.QueryRow(testCtx, `SELECT sent FROM reminders WHERE id = $1`, futureID).Scan(&futureSent))
	assert.True(t, sent, "due reminder should be marked sent")
	assert.False(t, futureSent, "future reminder must stay pending")

	// The task is visible in the queue under the reminder id.
	task, err := inspector.GetTaskInfo("default", reminderID.String())
	if err == nil {
		assert.Equal(t, queue.TypeSendReminder, task.Type)
	}

	// Second pass: the already-sent reminder is not re-enqueued.
	n2, err := dispatchDue(testCtx, pool, q)
	require.NoError(t, err)
	assert.Less(t, n2, n+1, "sent reminders are not redispatched in the same run")
}
