package handler

import (
	"context"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/teambition/rrule-go"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
)

const maxAttendees = 50

// Attendee is a guest of a schedule.
type Attendee struct {
	Email          string `json:"email"`
	DisplayName    string `json:"display_name"`
	ResponseStatus string `json:"response_status"`
	IsOrganizer    bool   `json:"is_organizer"`
}

var (
	colorRe = regexp.MustCompile(`^([1-9]|1[01])$`)
	untilRe = regexp.MustCompile(`^\d{8}(T\d{6}Z)?$`)
	days    = map[string]bool{"MO": true, "TU": true, "WE": true, "TH": true, "FR": true, "SA": true, "SU": true}
)

// validRecurrence accepts only the bare RRULE shape the frontend produces:
// FREQ, INTERVAL, BYDAY, BYMONTHDAY and one of COUNT / UNTIL.
func validRecurrence(rule string) bool {
	seen := map[string]bool{}
	for _, part := range strings.Split(rule, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || v == "" || seen[k] {
			return false
		}
		seen[k] = true
		switch k {
		case "FREQ":
			if v != "DAILY" && v != "WEEKLY" && v != "MONTHLY" && v != "YEARLY" {
				return false
			}
		case "INTERVAL", "COUNT":
			if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 9999 {
				return false
			}
		case "BYDAY":
			for _, d := range strings.Split(v, ",") {
				if !days[d] {
					return false
				}
			}
		case "BYMONTHDAY":
			for _, d := range strings.Split(v, ",") {
				if n, err := strconv.Atoi(d); err != nil || n == 0 || n < -31 || n > 31 {
					return false
				}
			}
		case "UNTIL":
			if !untilRe.MatchString(v) {
				return false
			}
		default:
			return false
		}
	}
	if !seen["FREQ"] || (seen["COUNT"] && seen["UNTIL"]) {
		return false
	}
	_, err := rrule.StrToRRule(rule)
	return err == nil
}

// validate checks the v2 fields and normalises attendees (lower-case, deduped).
// It returns an error code for the response, or "" when the request is fine.
func (r *scheduleRequest) validate() string {
	if r.Recurrence != "" && !validRecurrence(r.Recurrence) {
		return "invalid_recurrence"
	}
	if r.Timezone != "" {
		if _, err := time.LoadLocation(r.Timezone); err != nil || r.Timezone == "Local" {
			return "invalid_timezone"
		}
	}
	if r.ColorID != nil && !colorRe.MatchString(*r.ColorID) {
		return "invalid_color"
	}
	if r.Attendees != nil {
		if len(r.Attendees) > maxAttendees {
			return "invalid_attendee"
		}
		seen := map[string]bool{}
		out := make([]string, 0, len(r.Attendees))
		for _, raw := range r.Attendees {
			a, err := mail.ParseAddress(strings.TrimSpace(raw))
			if err != nil || a.Name != "" {
				return "invalid_attendee"
			}
			email := strings.ToLower(a.Address)
			if !seen[email] {
				seen[email] = true
				out = append(out, email)
			}
		}
		r.Attendees = out
	}
	return ""
}

func (r scheduleRequest) pushOpts() googlecal.PushOpts {
	return googlecal.PushOpts{AddMeet: r.AddMeet, SendUpdates: r.SendUpdates}
}

// applyTo overlays the request on an event loaded from the database, keeping
// existing guest responses for emails that stay.
func (r scheduleRequest) applyTo(e *googlecal.Event) {
	e.Title, e.Description = r.Title, r.Description
	e.Start, e.End, e.AllDay = r.StartTime, r.EndTime, r.AllDay
	e.Location = r.Location
	e.ColorID = ""
	if r.ColorID != nil {
		e.ColorID = *r.ColorID
	}
	if r.Timezone != "" {
		e.TimeZone = r.Timezone
	}
	if r.Reminders != nil {
		e.Reminders = r.Reminders
	}
	if r.Attendees != nil {
		old := map[string]googlecal.Attendee{}
		for _, a := range e.Attendees {
			old[a.Email] = a
		}
		e.Attendees = e.Attendees[:0:0]
		for _, email := range r.Attendees {
			a, ok := old[email]
			if !ok {
				a = googlecal.Attendee{Email: email, ResponseStatus: "needsAction"}
			}
			e.Attendees = append(e.Attendees, a)
		}
	}
}

// attachAttendees fills Attendees (never nil) for every schedule in the slice.
func attachAttendees(ctx context.Context, db *pgxpool.Pool, ss []Schedule) error {
	ids := uniqueScheduleIDs(ss)
	by := make(map[uuid.UUID][]Attendee, len(ids))
	if len(ids) > 0 {
		rows, err := db.Query(ctx, `
			SELECT schedule_id, email, COALESCE(display_name, ''), response_status, is_organizer
			FROM schedule_attendees WHERE schedule_id = ANY($1) ORDER BY email`, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var a Attendee
			if err := rows.Scan(&id, &a.Email, &a.DisplayName, &a.ResponseStatus, &a.IsOrganizer); err != nil {
				return err
			}
			by[id] = append(by[id], a)
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	for i := range ss {
		ss[i].Attendees = by[ss[i].ID]
		if ss[i].Attendees == nil {
			ss[i].Attendees = []Attendee{}
		}
	}
	return nil
}

// replaceAttendees makes the stored guest set equal to emails; existing rows
// (and their responses) are kept.
func replaceAttendees(ctx context.Context, db *pgxpool.Pool, id uuid.UUID, emails []string) error {
	if _, err := db.Exec(ctx, `DELETE FROM schedule_attendees
		WHERE schedule_id = $1 AND NOT (email = ANY($2))`, id, emails); err != nil {
		return err
	}
	for _, e := range emails {
		if _, err := db.Exec(ctx, `INSERT INTO schedule_attendees (schedule_id, email)
			VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, e); err != nil {
			return err
		}
	}
	return nil
}

// reminderOffsets lists a schedule's distinct live reminder offsets.
func reminderOffsets(ctx context.Context, db *pgxpool.Pool, id uuid.UUID) ([]int, error) {
	rows, err := db.Query(ctx, `SELECT DISTINCT offset_mins FROM reminders
		WHERE schedule_id = $1 AND dismissed = FALSE ORDER BY offset_mins`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}
