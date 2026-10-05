package googlecal

import (
	"strings"
	"time"

	"github.com/google/uuid"
	calendar "google.golang.org/api/calendar/v3"
)

const dateLayout = "2006-01-02"

// loadZone resolves an IANA name; empty or unknown falls back to UTC.
func loadZone(name string) *time.Location {
	if name == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.UTC
}

// confVersion is the conferenceDataVersion Google needs to honour Meet changes.
func confVersion(e Event) int64 {
	if e.AddMeet || e.RemoveMeet {
		return 1
	}
	return 0
}

// toAPI maps an app event to a Google event. All-day uses date fields (end
// exclusive); timed uses RFC3339 dateTime in the event's zone. Recurring events
// need an explicit timeZone, which is always set for timed events.
func toAPI(e Event) *calendar.Event {
	out := &calendar.Event{Summary: e.Title, Description: e.Description, Location: e.Location, ColorId: e.ColorID}
	// Patch drops empty strings; force them so a cleared field is cleared remotely.
	out.ForceSendFields = append(out.ForceSendFields, "Description", "Location")
	if e.ColorID == "" {
		out.NullFields = append(out.NullFields, "ColorId")
	}
	loc := loadZone(e.TimeZone)
	start := e.Start.In(loc)
	if e.AllDay {
		// The stored end is inclusive: the exclusive Google end is the day after.
		last := start
		if e.End != nil && e.End.After(e.Start) {
			last = e.End.In(loc)
		}
		out.Start = &calendar.EventDateTime{Date: start.Format(dateLayout)}
		out.End = &calendar.EventDateTime{Date: last.AddDate(0, 0, 1).Format(dateLayout)}
	} else {
		end := start.Add(time.Hour)
		if e.End != nil && e.End.After(e.Start) {
			end = e.End.In(loc)
		}
		out.Start = &calendar.EventDateTime{DateTime: start.Format(time.RFC3339), TimeZone: loc.String()}
		out.End = &calendar.EventDateTime{DateTime: end.Format(time.RFC3339), TimeZone: loc.String()}
	}
	if !e.Instance {
		if rule := strings.TrimPrefix(strings.TrimSpace(e.Recurrence), "RRULE:"); rule != "" {
			out.Recurrence = []string{"RRULE:" + rule}
		} else {
			// Patch ignores nil slices; force-send an empty recurrence to clear it.
			out.Recurrence = []string{}
			out.ForceSendFields = append(out.ForceSendFields, "Recurrence")
		}
	}

	rem := &calendar.EventReminders{UseDefault: len(e.Reminders) == 0, ForceSendFields: []string{"UseDefault"}}
	for _, m := range e.Reminders {
		rem.Overrides = append(rem.Overrides, &calendar.EventReminder{Method: "popup", Minutes: int64(m)})
	}
	out.Reminders = rem

	out.Attendees = []*calendar.EventAttendee{}
	for _, a := range e.Attendees {
		out.Attendees = append(out.Attendees, &calendar.EventAttendee{
			Email: a.Email, DisplayName: a.DisplayName, ResponseStatus: a.ResponseStatus})
	}
	out.ForceSendFields = append(out.ForceSendFields, "Attendees")

	switch {
	case e.AddMeet:
		out.ConferenceData = &calendar.ConferenceData{CreateRequest: &calendar.CreateConferenceRequest{
			RequestId:             uuid.NewString(),
			ConferenceSolutionKey: &calendar.ConferenceSolutionKey{Type: "hangoutsMeet"},
		}}
	case e.RemoveMeet:
		out.NullFields = append(out.NullFields, "ConferenceData")
	}
	return out
}

// fromAPI maps a Google event; ok=false when it has no usable start.
// All-day events store end = exclusive end minus 1s, so a one-day event ends
// at 23:59:59 of that day and round-trips through toAPI.
func fromAPI(ev *calendar.Event) (Event, bool) {
	e := Event{ID: ev.Id, ETag: ev.Etag, Cancelled: ev.Status == "cancelled",
		Title: ev.Summary, Description: ev.Description, RecurringID: ev.RecurringEventId,
		Location: ev.Location, ColorID: ev.ColorId, MeetLink: ev.HangoutLink}
	if e.Cancelled {
		return e, true
	}
	if ev.Start == nil {
		return e, false
	}
	switch {
	case ev.Start.Date != "":
		s, err := time.Parse(dateLayout, ev.Start.Date)
		if err != nil {
			return e, false
		}
		e.AllDay, e.Start = true, s
		if ev.End != nil && ev.End.Date != "" {
			if x, err := time.Parse(dateLayout, ev.End.Date); err == nil && x.After(s) {
				x = x.Add(-time.Second)
				e.End = &x
			}
		}
	case ev.Start.DateTime != "":
		s, err := time.Parse(time.RFC3339, ev.Start.DateTime)
		if err != nil {
			return e, false
		}
		e.Start = s
		if tz := ev.Start.TimeZone; tz != "" {
			if _, err := time.LoadLocation(tz); err == nil {
				e.TimeZone = tz
			}
		}
		if ev.End != nil && ev.End.DateTime != "" {
			if x, err := time.Parse(time.RFC3339, ev.End.DateTime); err == nil {
				e.End = &x
			}
		}
	default:
		return e, false
	}
	if r := ev.Reminders; r != nil && !r.UseDefault {
		seen := map[int]bool{}
		for _, o := range r.Overrides {
			m := int(o.Minutes)
			if (o.Method == "popup" || o.Method == "email") && !seen[m] {
				seen[m] = true
				e.Reminders = append(e.Reminders, m)
			}
		}
	}
	for _, a := range ev.Attendees {
		if a.Email == "" || a.Resource {
			continue
		}
		e.Attendees = append(e.Attendees, Attendee{Email: strings.ToLower(a.Email), DisplayName: a.DisplayName,
			ResponseStatus: a.ResponseStatus, Organizer: a.Organizer})
	}
	for _, r := range ev.Recurrence {
		if rule, ok := strings.CutPrefix(r, "RRULE:"); ok {
			e.Recurrence = rule
		}
	}
	return e, true
}
