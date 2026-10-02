package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/chronaxis/daily-planner-backend/internal/queue"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleSendReminderInsertsNotification(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	require.NoError(t, err)
	defer pool.Close()

	var userID uuid.UUID
	err = pool.QueryRow(ctx, `
		INSERT INTO users (name, email, password_hash)
		VALUES ('W', $1, 'x') RETURNING id
	`, fmt.Sprintf("w-%s@example.com", uuid.NewString())).Scan(&userID)
	require.NoError(t, err)
	defer pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)

	s := New(asynq.RedisClientOpt{}, pool) // redis opt unused by the handler

	payload, err := json.Marshal(queue.ReminderPayload{
		ReminderID:   uuid.New(),
		UserID:       userID,
		ScheduleName: "Standup",
		OffsetMins:   15,
	})
	require.NoError(t, err)

	task := asynq.NewTask(queue.TypeSendReminder, payload)
	require.NoError(t, s.handleSendReminder(ctx, task))

	var title, body string
	err = pool.QueryRow(ctx, `
		SELECT title, body FROM notifications WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1
	`, userID).Scan(&title, &body)
	require.NoError(t, err)
	assert.Equal(t, "Standup", title)
	assert.Equal(t, "Starts in 15 minutes", body)

	// Zero offset → "Starting now".
	payload, err = json.Marshal(queue.ReminderPayload{UserID: userID, ScheduleName: "Now", OffsetMins: 0})
	require.NoError(t, err)
	require.NoError(t, s.handleSendReminder(ctx, asynq.NewTask(queue.TypeSendReminder, payload)))
	err = pool.QueryRow(ctx, `
		SELECT body FROM notifications WHERE user_id = $1 ORDER BY created_at DESC LIMIT 1
	`, userID).Scan(&body)
	require.NoError(t, err)
	assert.Equal(t, "Starting now", body)
}
