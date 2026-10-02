package scheduler

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/teambition/rrule-go"
)

// DefaultWindowDays is how far ahead recurring schedules are materialised.
const DefaultWindowDays = 60

type recurringSchedule struct {
	ID       uuid.UUID
	Rule     string
	Start    time.Time
	Duration time.Duration
}

// expandRecurring materialises schedule_occurrences for every recurring
// schedule over [now, now+windowDays]. Inserts are idempotent via the
// (schedule_id, occurrence_start) unique index.
func expandRecurring(ctx context.Context, db *pgxpool.Pool, windowDays int) (int, error) {
	if windowDays <= 0 {
		windowDays = DefaultWindowDays
	}

	rows, err := db.Query(ctx, `
		SELECT s.id, r.rule, s.start_time,
		       COALESCE(EXTRACT(EPOCH FROM (s.end_time - s.start_time)), 0)
		FROM schedules s
		JOIN schedule_recurrences r ON r.schedule_id = s.id
		WHERE r.rule <> ''
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var schedules []recurringSchedule
	for rows.Next() {
		var rs recurringSchedule
		var durationSeconds float64
		if err := rows.Scan(&rs.ID, &rs.Rule, &rs.Start, &durationSeconds); err != nil {
			return 0, err
		}
		rs.Duration = time.Duration(durationSeconds) * time.Second
		schedules = append(schedules, rs)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	now := time.Now()
	windowEnd := now.AddDate(0, 0, windowDays)
	inserted := 0

	for _, rs := range schedules {
		starts, err := occurrencesFor(rs, now, windowEnd)
		if err != nil {
			continue // a malformed rule must not stall the whole scan
		}
		for _, start := range starts {
			var end *time.Time
			if rs.Duration > 0 {
				e := start.Add(rs.Duration)
				end = &e
			}
			tag, err := db.Exec(ctx, `
				INSERT INTO schedule_occurrences (schedule_id, occurrence_start, occurrence_end)
				VALUES ($1, $2, $3)
				ON CONFLICT (schedule_id, occurrence_start) DO NOTHING
			`, rs.ID, start, end)
			if err != nil {
				return inserted, err
			}
			inserted += int(tag.RowsAffected())
		}
	}

	return inserted, nil
}

// occurrencesFor expands one schedule's rule into start times within [from, to].
func occurrencesFor(rs recurringSchedule, from, to time.Time) ([]time.Time, error) {
	rule, err := rrule.StrToRRule(rs.Rule)
	if err != nil {
		return nil, err
	}
	// The schedule's start_time is the recurrence seed (DTSTART).
	rule.DTStart(rs.Start)
	return rule.Between(from, to, true), nil
}

// materialiseOccurrenceReminders turns each recurring schedule's reminder
// templates (fire_at IS NULL) into concrete reminder rows, one per upcoming
// occurrence. Idempotent via idx_reminders_unique_fire; sent stays FALSE so
// dispatchDue picks them up when their fire_at arrives.
func materialiseOccurrenceReminders(ctx context.Context, db *pgxpool.Pool) (int, error) {
	rows, err := db.Query(ctx, `
		INSERT INTO reminders (schedule_id, offset_mins, fire_at)
		SELECT tmpl.schedule_id, tmpl.offset_mins,
		       o.occurrence_start - (tmpl.offset_mins * interval '1 minute')
		FROM reminders tmpl
		JOIN schedule_occurrences o ON o.schedule_id = tmpl.schedule_id
		WHERE tmpl.fire_at IS NULL
		  AND o.occurrence_start > NOW()
		  AND o.occurrence_start < NOW() + interval '60 days'
		ON CONFLICT DO NOTHING
		RETURNING id
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	created := 0
	for rows.Next() {
		created++
	}
	return created, rows.Err()
}
