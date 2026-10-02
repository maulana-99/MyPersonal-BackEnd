package queue

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
)

func testClient(t *testing.T) *Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set")
	}
	c := NewClient(addr, os.Getenv("REDIS_PASSWORD"))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestEnqueueReminderIdempotent(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	p := ReminderPayload{
		ReminderID:   uuid.New(),
		ScheduleID:   uuid.New(),
		UserID:       uuid.New(),
		ScheduleName: "Test",
		StartTime:    time.Now().Add(time.Hour),
		OffsetMins:   10,
	}

	require.NoError(t, c.EnqueueReminder(ctx, p, 5*time.Minute))
	// Same TaskID (reminder id) → the second enqueue is a no-op, not a failure.
	require.NoError(t, c.EnqueueReminder(ctx, p, 1*time.Minute))

	inspector := asynq.NewInspector(asynq.RedisClientOpt{
		Addr:     os.Getenv("REDIS_ADDR"),
		Password: os.Getenv("REDIS_PASSWORD"),
	})
	defer inspector.Close()

	info, err := inspector.GetTaskInfo("default", p.ReminderID.String())
	require.NoError(t, err)
	require.Equal(t, TypeSendReminder, info.Type)

	// Clean up so the queue does not grow across runs.
	_ = inspector.DeleteTask("default", p.ReminderID.String())
}
