package googlecal

import (
	tasks "google.golang.org/api/tasks/v1"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	calendar "google.golang.org/api/calendar/v3"
)

func TestEncryptRoundTripAndTamper(t *testing.T) {
	key := make([]byte, 32)
	blob, err := Encrypt(key, []byte("refresh-token"))
	require.NoError(t, err)
	pt, err := Decrypt(key, blob)
	require.NoError(t, err)
	assert.Equal(t, "refresh-token", string(pt))

	blob[len(blob)-1] ^= 1
	_, err = Decrypt(key, blob)
	assert.Error(t, err)

	other := make([]byte, 32)
	other[0] = 1
	good, _ := Encrypt(key, []byte("x"))
	_, err = Decrypt(other, good)
	assert.Error(t, err)
	_, err = Decrypt(key, []byte("short"))
	assert.Error(t, err)
}

func TestParseKey(t *testing.T) {
	_, err := ParseKey("not base64!!")
	assert.Error(t, err)
	_, err = ParseKey("c2hvcnQ=")
	assert.Error(t, err)
	_, err = ParseKey("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	assert.NoError(t, err)
}

func TestState(t *testing.T) {
	id := uuid.New()
	tok, err := SignState("secret", id, time.Minute)
	require.NoError(t, err)
	got, err := VerifyState("secret", tok)
	require.NoError(t, err)
	assert.Equal(t, id, got)

	_, err = VerifyState("other", tok)
	assert.Error(t, err, "bad signature")

	expired, _ := SignState("secret", id, -time.Minute)
	_, err = VerifyState("secret", expired)
	assert.Error(t, err, "expired")

	_, err = VerifyState("secret", "garbage")
	assert.Error(t, err)
}

func TestToAPITimed(t *testing.T) {
	start := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	end := start.Add(30 * time.Minute)
	ev := toAPI(Event{Title: "A", Start: start, End: &end})
	assert.Equal(t, "2026-05-01T09:00:00Z", ev.Start.DateTime)
	assert.Equal(t, "2026-05-01T09:30:00Z", ev.End.DateTime)
	assert.Empty(t, ev.Start.Date)
	assert.Empty(t, ev.Recurrence)
}

func TestToAPIAllDayExclusiveEnd(t *testing.T) {
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	ev := toAPI(Event{Title: "A", Start: start, AllDay: true})
	assert.Equal(t, "2026-05-01", ev.Start.Date)
	assert.Equal(t, "2026-05-02", ev.End.Date)

	end := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	ev = toAPI(Event{Start: start, End: &end, AllDay: true})
	assert.Equal(t, "2026-05-04", ev.End.Date)
}

func TestToAPIRecurrence(t *testing.T) {
	start := time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)
	for _, rule := range []string{"FREQ=DAILY;COUNT=3", "RRULE:FREQ=DAILY;COUNT=3"} {
		ev := toAPI(Event{Start: start, Recurrence: rule})
		assert.Equal(t, []string{"RRULE:FREQ=DAILY;COUNT=3"}, ev.Recurrence)
		assert.Equal(t, "UTC", ev.Start.TimeZone)
	}
}

func TestFromAPI(t *testing.T) {
	e, ok := fromAPI(&calendar.Event{Id: "1", Etag: "e", Summary: "T",
		Start: &calendar.EventDateTime{DateTime: "2026-05-01T09:00:00+02:00"},
		End:   &calendar.EventDateTime{DateTime: "2026-05-01T10:00:00+02:00"}})
	require.True(t, ok)
	assert.False(t, e.AllDay)
	assert.True(t, e.Start.Equal(time.Date(2026, 5, 1, 7, 0, 0, 0, time.UTC)))
	assert.Equal(t, time.Hour, e.End.Sub(e.Start))

	e, ok = fromAPI(&calendar.Event{Id: "2",
		Start: &calendar.EventDateTime{Date: "2026-05-01"}, End: &calendar.EventDateTime{Date: "2026-05-02"}})
	require.True(t, ok)
	assert.True(t, e.AllDay)
	// round trip: exclusive end preserved
	assert.Equal(t, "2026-05-02", toAPI(e).End.Date)

	e, ok = fromAPI(&calendar.Event{Id: "3", Status: "cancelled"})
	assert.True(t, ok)
	assert.True(t, e.Cancelled)

	_, ok = fromAPI(&calendar.Event{Id: "4"})
	assert.False(t, ok)
}

func TestToAPIV2Fields(t *testing.T) {
	start := time.Date(2026, 5, 1, 23, 30, 0, 0, time.UTC) // 06:30 on May 2 in Jakarta
	ev := toAPI(Event{
		Title: "A", Start: start, TimeZone: "Asia/Jakarta", Location: "HQ", ColorID: "3",
		Reminders: []int{10, 60}, SendUpdates: "all", AddMeet: true,
		Attendees: []Attendee{{Email: "a@b.com", ResponseStatus: "accepted"}},
	})
	assert.Equal(t, "2026-05-02T06:30:00+07:00", ev.Start.DateTime)
	assert.Equal(t, "Asia/Jakarta", ev.Start.TimeZone)
	assert.Equal(t, "HQ", ev.Location)
	assert.Equal(t, "3", ev.ColorId)
	require.False(t, ev.Reminders.UseDefault)
	require.Len(t, ev.Reminders.Overrides, 2)
	assert.Equal(t, "popup", ev.Reminders.Overrides[0].Method)
	assert.Equal(t, int64(60), ev.Reminders.Overrides[1].Minutes)
	require.Len(t, ev.Attendees, 1)
	assert.Equal(t, "a@b.com", ev.Attendees[0].Email)
	require.NotNil(t, ev.ConferenceData)
	assert.Equal(t, "hangoutsMeet", ev.ConferenceData.CreateRequest.ConferenceSolutionKey.Type)
	assert.NotEmpty(t, ev.ConferenceData.CreateRequest.RequestId)
	assert.Equal(t, int64(1), confVersion(Event{AddMeet: true}))
	assert.Equal(t, int64(1), confVersion(Event{RemoveMeet: true}))
	assert.Equal(t, int64(0), confVersion(Event{}))

	// all-day uses the date in the event zone
	d := toAPI(Event{Start: start, AllDay: true, TimeZone: "Asia/Jakarta"})
	assert.Equal(t, "2026-05-02", d.Start.Date)

	// defaults: empty reminders -> useDefault, guests/colour cleared on patch, Meet removal
	ev = toAPI(Event{Start: start, RemoveMeet: true, Instance: true})
	assert.True(t, ev.Reminders.UseDefault)
	assert.Contains(t, ev.NullFields, "ColorId")
	assert.Contains(t, ev.NullFields, "ConferenceData")
	assert.Contains(t, ev.ForceSendFields, "Attendees")
	assert.Nil(t, ev.Recurrence, "instances never send a rule")
	assert.Equal(t, "UTC", ev.Start.TimeZone)
}

func TestFromAPIV2Fields(t *testing.T) {
	e, ok := fromAPI(&calendar.Event{Id: "1", Location: "HQ", ColorId: "9", HangoutLink: "https://meet/x",
		Start:     &calendar.EventDateTime{DateTime: "2026-05-01T09:00:00+02:00", TimeZone: "Europe/Paris"},
		Reminders: &calendar.EventReminders{Overrides: []*calendar.EventReminder{{Method: "popup", Minutes: 10}, {Method: "email", Minutes: 30}, {Method: "popup", Minutes: 10}}},
		Attendees: []*calendar.EventAttendee{{Email: "A@B.com", ResponseStatus: "tentative"}, {Email: "room@x", Resource: true}},
	})
	require.True(t, ok)
	assert.Equal(t, "HQ", e.Location)
	assert.Equal(t, "9", e.ColorID)
	assert.Equal(t, "https://meet/x", e.MeetLink)
	assert.Equal(t, "Europe/Paris", e.TimeZone)
	assert.Equal(t, []int{10, 30}, e.Reminders)
	assert.Equal(t, []Attendee{{Email: "a@b.com", ResponseStatus: "tentative"}}, e.Attendees)

	e, _ = fromAPI(&calendar.Event{Id: "2", Start: &calendar.EventDateTime{Date: "2026-05-01"},
		Reminders: &calendar.EventReminders{UseDefault: true, Overrides: []*calendar.EventReminder{{Method: "popup", Minutes: 5}}}})
	assert.Empty(t, e.Reminders)
}

func TestLoginStateIsSeparate(t *testing.T) {
	tok, err := signState("secret", loginStatePurpose, nonceHash("n1"), time.Minute)
	require.NoError(t, err)
	got, err := verifyState("secret", loginStatePurpose, tok)
	require.NoError(t, err)
	assert.Equal(t, nonceHash("n1"), got)

	_, err = VerifyState("secret", tok)
	assert.Error(t, err, "login state is not a connect state")
	conn, _ := SignState("secret", uuid.New(), time.Minute)
	_, err = verifyState("secret", loginStatePurpose, conn)
	assert.Error(t, err, "connect state is not a login state")
}

func TestScopesUseFullDrive(t *testing.T) {
	c := NewClient("id", "secret", "http://x/cb", "http://x/login")
	for _, u := range []string{c.AuthURL("s"), c.LoginURL("s")} {
		pu, err := url.Parse(u)
		require.NoError(t, err)
		assert.Contains(t, strings.Fields(pu.Query().Get("scope")), "https://www.googleapis.com/auth/drive")
		assert.NotContains(t, u, "drive.file")
	}
	assert.Equal(t, "https://www.googleapis.com/auth/drive", driveScope)
}

func TestQueryEscape(t *testing.T) {
	assert.Equal(t, `'a\'b\\c é'`, Query(`a'b\c é`))
}

func TestFromTaskDueNoTzShift(t *testing.T) {
	it := fromTask(&tasks.Task{Id: "a", Due: "2026-10-12T00:00:00.000Z", Status: "needsAction"})
	if it.DueDate == nil || *it.DueDate != "2026-10-12" || it.Status != "pending" {
		t.Fatalf("got %+v", it)
	}
	if dueStamp("2026-10-12") != "2026-10-12T00:00:00.000Z" {
		t.Fatal("dueStamp")
	}
}
