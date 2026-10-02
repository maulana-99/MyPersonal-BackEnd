package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// Task types processed by the worker.
const (
	TypeSendReminder = "reminder:send"
)

// ReminderPayload is everything the worker needs without a DB round-trip to
// resolve what to say. Schedule fields are denormalised on purpose.
type ReminderPayload struct {
	ReminderID   uuid.UUID `json:"reminder_id"`
	ScheduleID   uuid.UUID `json:"schedule_id"`
	UserID       uuid.UUID `json:"user_id"`
	ScheduleName string    `json:"schedule_name"`
	StartTime    time.Time `json:"start_time"`
	OffsetMins   int       `json:"offset_mins"`
}

type Client struct {
	asynq *asynq.Client
}

func NewClient(addr, password string) *Client {
	return &Client{
		asynq: asynq.NewClient(asynq.RedisClientOpt{Addr: addr, Password: password}),
	}
}

func (c *Client) Close() error {
	return c.asynq.Close()
}

// EnqueueReminder schedules the reminder job to fire `delay` from now.
// A non-positive delay fires immediately (catch-up for a reminder created
// late but still in the future relative to its start time).
//
// The task carries TaskID(reminderID) so a duplicate enqueue for the same
// reminder is reported as ErrTaskIDConflict, which we treat as success —
// "already queued" is the desired outcome, not an error.
func (c *Client) EnqueueReminder(ctx context.Context, p ReminderPayload, delay time.Duration) error {
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal reminder payload: %w", err)
	}

	task := asynq.NewTask(TypeSendReminder, body)

	opts := []asynq.Option{
		asynq.MaxRetry(3),
		asynq.TaskID(p.ReminderID.String()), // dedupe: one queued task per reminder
	}
	if delay > 0 {
		opts = append(opts, asynq.ProcessIn(delay))
	}

	if _, err := c.asynq.EnqueueContext(ctx, task, opts...); err != nil {
		if errors.Is(err, asynq.ErrTaskIDConflict) {
			return nil // already enqueued for this reminder
		}
		return fmt.Errorf("enqueue reminder: %w", err)
	}
	return nil
}
