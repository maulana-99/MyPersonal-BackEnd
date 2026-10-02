// Package scheduler scans for reminders that are due and hands them to the
// Asynq queue. It is the bridge between stored reminder rows and the worker.
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/queue"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultInterval is how often the scheduler looks for due reminders.
const DefaultInterval = 30 * time.Second

// Run blocks until ctx is cancelled, scanning for due reminders every interval.
// A nil client disables enqueueing (API-only mode) but the loop still no-ops.
func Run(ctx context.Context, db *pgxpool.Pool, q *queue.Client, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if q == nil {
				continue
			}
			if n, err := dispatchDue(ctx, db, q); err != nil {
				log.Printf("scheduler: %v", err)
			} else if n > 0 {
				log.Printf("scheduler: enqueued %d reminder(s)", n)
			}
			if n, err := expandRecurring(ctx, db, DefaultWindowDays); err != nil {
				log.Printf("scheduler: expand recurrences: %v", err)
			} else if n > 0 {
				log.Printf("scheduler: materialised %d occurrence(s)", n)
			}
			if n, err := materialiseOccurrenceReminders(ctx, db); err != nil {
				log.Printf("scheduler: materialise occurrence reminders: %v", err)
			} else if n > 0 {
				log.Printf("scheduler: materialised %d occurrence reminder(s)", n)
			}
		}
	}
}

type dueReminder struct {
	ID           uuid.UUID
	ScheduleID   uuid.UUID
	UserID       uuid.UUID
	ScheduleName string
	StartTime    time.Time
	OffsetMins   int
}

// dispatchDue finds reminders whose fire_at has passed and enqueues them.
// Exactly the rows it enqueues are marked sent, so a crash between enqueue and
// update re-enqueues under the same TaskID (idempotent at the Asynq level).
func dispatchDue(ctx context.Context, db *pgxpool.Pool, q *queue.Client) (int, error) {
	rows, err := db.Query(ctx, `
		SELECT r.id, r.schedule_id, s.user_id, s.title, s.start_time, r.offset_mins
		FROM reminders r
		JOIN schedules s ON s.id = r.schedule_id
		WHERE r.sent = FALSE
		  AND r.dismissed = FALSE
		  AND r.fire_at IS NOT NULL
		  AND r.fire_at <= NOW()
		ORDER BY r.fire_at
		LIMIT 100
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var due []dueReminder
	for rows.Next() {
		var d dueReminder
		if err := rows.Scan(&d.ID, &d.ScheduleID, &d.UserID, &d.ScheduleName, &d.StartTime, &d.OffsetMins); err != nil {
			return 0, err
		}
		due = append(due, d)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	sent := 0
	for _, d := range due {
		err := q.EnqueueReminder(ctx, queue.ReminderPayload{
			ReminderID:   d.ID,
			ScheduleID:   d.ScheduleID,
			UserID:       d.UserID,
			ScheduleName: d.ScheduleName,
			StartTime:    d.StartTime,
			OffsetMins:   d.OffsetMins,
		}, 0)
		if err != nil {
			log.Printf("scheduler: enqueue %s: %v", d.ID, err)
			continue
		}
		if _, err := db.Exec(ctx, `UPDATE reminders SET sent = TRUE WHERE id = $1`, d.ID); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}
