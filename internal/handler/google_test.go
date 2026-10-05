package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGoogle is an in-memory googlecal.Client; no network.
type fakeGoogle struct {
	events    []googlecal.Event
	listErrs  []error // consumed one per ListEvents call
	listCalls []string
	pushErr   error
	inserted  []googlecal.Event
	patched   []string
	patchedEv []googlecal.Event
	deleted   []string
	deleteSU  []string
	revoked   int
	exchRT    string
	exchEmail string
	exchErr   error
	deleteErr error
	loginID   googlecal.Identity
	loginErr  error
}

func (f *fakeGoogle) LoginURL(state string) string {
	return "https://accounts.example/login?state=" + state
}
func (f *fakeGoogle) ExchangeLogin(context.Context, string) (googlecal.Identity, error) {
	return f.loginID, f.loginErr
}

func (f *fakeGoogle) AuthURL(state string) string {
	return "https://accounts.example/auth?state=" + state
}
func (f *fakeGoogle) Exchange(context.Context, string) (string, string, error) {
	return f.exchRT, f.exchEmail, f.exchErr
}
func (f *fakeGoogle) ListEvents(_ context.Context, _, tok string) (googlecal.ListResult, error) {
	f.listCalls = append(f.listCalls, tok)
	if len(f.listErrs) > 0 {
		err := f.listErrs[0]
		f.listErrs = f.listErrs[1:]
		if err != nil {
			return googlecal.ListResult{}, err
		}
	}
	return googlecal.ListResult{Events: f.events, NextSyncToken: "tok-next"}, nil
}
func (f *fakeGoogle) InsertEvent(_ context.Context, _ string, e googlecal.Event) (googlecal.Result, error) {
	if f.pushErr != nil {
		return googlecal.Result{}, f.pushErr
	}
	f.inserted = append(f.inserted, e)
	r := googlecal.Result{ID: "gid-" + e.Title, ETag: "etag-1"}
	if e.AddMeet {
		r.MeetLink = "https://meet.example/abc"
	}
	return r, nil
}
func (f *fakeGoogle) PatchEvent(_ context.Context, _, id string, e googlecal.Event) (googlecal.Result, error) {
	if f.pushErr != nil {
		return googlecal.Result{}, f.pushErr
	}
	f.patched = append(f.patched, id)
	f.patchedEv = append(f.patchedEv, e)
	r := googlecal.Result{ID: id, ETag: "etag-2"}
	if e.AddMeet {
		r.MeetLink = "https://meet.example/new"
	}
	return r, nil
}
func (f *fakeGoogle) DeleteEvent(_ context.Context, _, id, su string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, id)
	f.deleteSU = append(f.deleteSU, su)
	return nil
}
func (f *fakeGoogle) Revoke(context.Context, string) error { f.revoked++; return nil }

var testKey = make([]byte, 32)

const stateSecret = "state-secret"

type googleEnv struct {
	f   *fakeGoogle
	svc *googlecal.Service
	r   *gin.Engine
}

func newGoogleEnv(t *testing.T, configured bool) googleEnv {
	t.Helper()
	f := &fakeGoogle{exchRT: "rt-secret", exchEmail: "me@example.com"}
	var svc *googlecal.Service
	if configured {
		svc = googlecal.NewService(testDB, f, testKey, stateSecret)
		svc.Go = func(fn func()) { fn() }
	}
	gh := NewGoogleHandler(svc, "http://front")
	sh := NewScheduleHandler(testDB, nil)
	sh.SetGoogle(svc)
	r := newProtectedRouter(func(g *gin.RouterGroup) {
		g.POST("/integrations/google/connect", gh.Connect)
		g.GET("/integrations/google/status", gh.Status)
		g.POST("/integrations/google/sync", gh.Sync)
		g.DELETE("/integrations/google", gh.Disconnect)
		g.POST("/schedules", sh.Create)
		g.GET("/schedules/:id", sh.Get)
		g.PUT("/schedules/:id", sh.Update)
		g.POST("/schedules/:id/move", sh.Move)
		g.DELETE("/schedules/:id", sh.Delete)
		g.POST("/integrations/google/upload-local", gh.UploadLocal)
		g.POST("/schedules/:id/duplicate", sh.Duplicate)
		g.POST("/schedules/:id/google-retry", sh.GoogleRetry)
	})
	r.GET("/callback", gh.Callback)
	return googleEnv{f, svc, r}
}

func connectUser(t *testing.T, e googleEnv, userID uuid.UUID) {
	t.Helper()
	state, err := googlecal.SignState(stateSecret, userID, time.Minute)
	require.NoError(t, err)
	w := doJSON(t, e.r, http.MethodGet, "/callback?code=c&state="+state, "", nil)
	require.Equal(t, http.StatusFound, w.Code)
	require.Equal(t, "http://front/calendar?google=connected", w.Header().Get("Location"))
}

func newPushed(t *testing.T, e googleEnv, token string, body gin.H) Schedule {
	t.Helper()
	w := doJSON(t, e.r, http.MethodPost, "/schedules", token, body)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	return decodeData[Schedule](t, w)
}

func TestGoogleNotConfigured(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	e := newGoogleEnv(t, false)

	w := doJSON(t, e.r, http.MethodGet, "/integrations/google/status", token, nil)
	require.Equal(t, http.StatusOK, w.Code)
	st := decodeData[map[string]any](t, w)
	assert.Equal(t, false, st["configured"])

	for _, p := range []struct{ m, path string }{
		{http.MethodPost, "/integrations/google/connect"},
		{http.MethodPost, "/integrations/google/sync"},
		{http.MethodDelete, "/integrations/google"},
	} {
		w = doJSON(t, e.r, p.m, p.path, token, nil)
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Contains(t, w.Body.String(), "google_not_configured")
	}
}

func TestGoogleConnectCallbackStatus(t *testing.T) {
	requireDB(t)
	uid, token := newUser(t)
	e := newGoogleEnv(t, true)

	w := doJSON(t, e.r, http.MethodPost, "/integrations/google/connect", token, nil)
	require.Equal(t, http.StatusOK, w.Code)
	u := decodeData[map[string]string](t, w)["url"]
	state := u[strings.Index(u, "state=")+6:]
	got, err := googlecal.VerifyState(stateSecret, state)
	require.NoError(t, err)
	assert.Equal(t, uid, got)

	// status before: not connected
	w = doJSON(t, e.r, http.MethodGet, "/integrations/google/status", token, nil)
	st := decodeData[map[string]any](t, w)
	assert.Equal(t, true, st["configured"])
	assert.Equal(t, false, st["connected"])

	// bad state / denied / exchange failure: fixed reasons
	w = doJSON(t, e.r, http.MethodGet, "/callback?code=c&state=bogus", "", nil)
	assert.Equal(t, "http://front/calendar?google=error&reason=state", w.Header().Get("Location"))
	w = doJSON(t, e.r, http.MethodGet, "/callback?error=access_denied", "", nil)
	assert.Equal(t, "http://front/calendar?google=error&reason=denied", w.Header().Get("Location"))
	e.f.exchErr = errors.New("boom secret")
	w = doJSON(t, e.r, http.MethodGet, "/callback?code=c&state="+state, "", nil)
	assert.Equal(t, "http://front/calendar?google=error&reason=exchange", w.Header().Get("Location"))
	e.f.exchErr = nil

	connectUser(t, e, uid)
	w = doJSON(t, e.r, http.MethodGet, "/integrations/google/status", token, nil)
	st = decodeData[map[string]any](t, w)
	assert.Equal(t, true, st["connected"])
	assert.Equal(t, "me@example.com", st["email"])
	assert.NotNil(t, st["last_synced_at"], "first sync ran")
	assert.NotContains(t, w.Body.String(), "rt-secret")

	// token is stored encrypted
	var enc []byte
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT refresh_token_enc FROM integrations_google WHERE user_id=$1`, uid).Scan(&enc))
	assert.NotContains(t, string(enc), "rt-secret")
}

func TestGoogleSyncImport(t *testing.T) {
	requireDB(t)
	uid, token := newUser(t)
	e := newGoogleEnv(t, true)

	w := doJSON(t, e.r, http.MethodPost, "/integrations/google/sync", token, nil)
	require.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "not_connected")

	connectUser(t, e, uid)
	start := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	end := start.Add(time.Hour)
	e.f.events = []googlecal.Event{
		{ID: "g1", ETag: "e1", Title: "Imported", Start: start, End: &end},
		{ID: "g2", ETag: "e1", Title: "", Start: start, AllDay: true},
	}
	w = doJSON(t, e.r, http.MethodPost, "/integrations/google/sync", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, googlecal.Stats{Imported: 2}, decodeData[googlecal.Stats](t, w))
	// second call used the stored sync token
	assert.Equal(t, "tok-next", e.f.listCalls[len(e.f.listCalls)-1])

	// same etag again: nothing; changed etag: update; cancelled: delete
	w = doJSON(t, e.r, http.MethodPost, "/integrations/google/sync", token, nil)
	assert.Equal(t, googlecal.Stats{}, decodeData[googlecal.Stats](t, w))
	e.f.events = []googlecal.Event{
		{ID: "g1", ETag: "e2", Title: "Renamed", Start: start, End: &end},
		{ID: "g2", Cancelled: true},
	}
	w = doJSON(t, e.r, http.MethodPost, "/integrations/google/sync", token, nil)
	assert.Equal(t, googlecal.Stats{Updated: 1, Deleted: 1}, decodeData[googlecal.Stats](t, w))

	var title, source string
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT title, source FROM schedules WHERE user_id=$1`, uid).Scan(&title, &source))
	assert.Equal(t, "Renamed", title)
	assert.Equal(t, "google", source)

	// 410: token dropped, full re-sync
	e.f.listErrs = []error{googlecal.ErrSyncTokenExpired}
	e.f.listCalls = nil
	w = doJSON(t, e.r, http.MethodPost, "/integrations/google/sync", token, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"tok-next", ""}, e.f.listCalls)
}

func TestGoogleImportedEditMoveDelete(t *testing.T) {
	requireDB(t)
	uid, token := newUser(t)
	e := newGoogleEnv(t, true)
	connectUser(t, e, uid)

	start := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	e.f.events = []googlecal.Event{{ID: "g1", ETag: "e1", Title: "Imported", Start: start}}
	doJSON(t, e.r, http.MethodPost, "/integrations/google/sync", token, nil)

	var id uuid.UUID
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT id FROM schedules WHERE user_id=$1`, uid).Scan(&id))
	path := "/schedules/" + id.String()

	// edit patches Google and then the local row; instances never get a rule
	w := doJSON(t, e.r, http.MethodPut, path, token, gin.H{"title": "Edited", "start_time": start, "recurrence": "FREQ=DAILY"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{"g1"}, e.f.patched)
	assert.True(t, e.f.patchedEv[0].Instance)
	assert.Equal(t, "Edited", e.f.patchedEv[0].Title)
	got := decodeData[Schedule](t, w)
	assert.Equal(t, "Edited", got.Title)
	assert.Equal(t, "google", got.Source)
	assert.Equal(t, "synced", *got.GoogleSyncState)
	assert.Empty(t, got.Recurrence)

	// our own patch is not re-imported as a change (etag stored)
	e.f.events = []googlecal.Event{{ID: "g1", ETag: "etag-2", Title: "Edited", Start: start}}
	w = doJSON(t, e.r, http.MethodPost, "/integrations/google/sync", token, nil)
	assert.Equal(t, googlecal.Stats{}, decodeData[googlecal.Stats](t, w))

	// move patches too
	end := start.Add(3 * time.Hour)
	w = doJSON(t, e.r, http.MethodPost, path+"/move", token, gin.H{"start_time": start.Add(time.Hour), "end_time": end})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, e.f.patchedEv, 2)
	assert.True(t, e.f.patchedEv[1].Start.Equal(start.Add(time.Hour)))
	assert.Equal(t, "Edited", e.f.patchedEv[1].Title)

	// failed patch: 502 and the local row is unchanged
	e.f.pushErr = errors.New("boom")
	w = doJSON(t, e.r, http.MethodPut, path, token, gin.H{"title": "Nope", "start_time": start})
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), "google_api_error")
	w = doJSON(t, e.r, http.MethodPost, path+"/move", token, gin.H{"start_time": start.Add(5 * time.Hour)})
	assert.Equal(t, http.StatusBadGateway, w.Code)
	var title string
	var st time.Time
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT title, start_time FROM schedules WHERE id=$1`, id).Scan(&title, &st))
	assert.Equal(t, "Edited", title)
	assert.True(t, st.Equal(start.Add(time.Hour)))
	e.f.pushErr = nil

	w = doJSON(t, e.r, http.MethodGet, path, token, nil)
	s := decodeData[Schedule](t, w)
	require.NotNil(t, s.GoogleEventID)
	assert.Equal(t, "g1", *s.GoogleEventID)

	w = doJSON(t, e.r, http.MethodDelete, path, token, nil)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, []string{"g1"}, e.f.deleted)
}

func TestGoogleWriteThroughLifecycle(t *testing.T) {
	requireDB(t)
	uid, token := newUser(t)
	e := newGoogleEnv(t, true)
	connectUser(t, e, uid)

	start := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	// every create goes to Google, no flag needed (push_to_google is ignored)
	s := newPushed(t, e, token, gin.H{"title": "Pushed", "start_time": start, "push_to_google": false, "recurrence": "FREQ=DAILY;COUNT=2"})
	require.NotNil(t, s.GoogleEventID)
	assert.Equal(t, "gid-Pushed", *s.GoogleEventID)
	assert.Nil(t, s.GoogleSyncState, "sync state is no longer written")
	assert.Equal(t, "local", s.Source)
	require.Len(t, e.f.inserted, 1)
	assert.Equal(t, "FREQ=DAILY;COUNT=2", e.f.inserted[0].Recurrence)

	// echo: importer only refreshes the etag, never title/time
	e.f.events = []googlecal.Event{
		{ID: "gid-Pushed", ETag: "etag-X", Title: "Hijacked", Start: start.Add(time.Hour)},
		{ID: "gid-Pushed_20260101", RecurringID: "gid-Pushed", ETag: "i1", Title: "Instance", Start: start},
	}
	w := doJSON(t, e.r, http.MethodPost, "/integrations/google/sync", token, nil)
	assert.Equal(t, googlecal.Stats{Updated: 1}, decodeData[googlecal.Stats](t, w))
	w = doJSON(t, e.r, http.MethodGet, "/schedules/"+s.ID.String(), token, nil)
	got := decodeData[Schedule](t, w)
	assert.Equal(t, "Pushed", got.Title)
	assert.True(t, got.StartTime.Equal(start))
	var n int
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT COUNT(*) FROM schedules WHERE user_id=$1 AND source='google'`, uid).Scan(&n))
	assert.Zero(t, n, "recurring instances of a pushed series are not imported")

	// writing a recurring series triggers an incremental sync so instances appear
	before := len(e.f.listCalls)
	w = doJSON(t, e.r, http.MethodPut, "/schedules/"+s.ID.String(), token,
		gin.H{"title": "Pushed2", "start_time": start, "recurrence": "FREQ=DAILY;COUNT=2"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, []string{"gid-Pushed"}, e.f.patched)
	assert.Greater(t, len(e.f.listCalls), before)

	// duplicate is write-through too
	w = doJSON(t, e.r, http.MethodPost, "/schedules/"+s.ID.String()+"/duplicate", token, nil)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	dup := decodeData[Schedule](t, w)
	require.NotNil(t, dup.GoogleEventID)
	assert.Len(t, e.f.inserted, 2)

	// delete pushes events.delete
	w = doJSON(t, e.r, http.MethodDelete, "/schedules/"+s.ID.String(), token, nil)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, []string{"gid-Pushed"}, e.f.deleted)

	// disconnect: revoke, imported rows gone, pushed rows stay unlinked
	pushed := newPushed(t, e, token, gin.H{"title": "Keep", "start_time": start})
	e.f.events = []googlecal.Event{{ID: "g9", ETag: "e", Title: "Imp", Start: start}}
	doJSON(t, e.r, http.MethodPost, "/integrations/google/sync", token, nil)
	w = doJSON(t, e.r, http.MethodDelete, "/integrations/google", token, nil)
	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, 1, e.f.revoked)
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT COUNT(*) FROM schedules WHERE user_id=$1 AND source='google'`, uid).Scan(&n))
	assert.Zero(t, n)
	w = doJSON(t, e.r, http.MethodGet, "/schedules/"+pushed.ID.String(), token, nil)
	kept := decodeData[Schedule](t, w)
	assert.Nil(t, kept.GoogleEventID)
	w = doJSON(t, e.r, http.MethodGet, "/integrations/google/status", token, nil)
	assert.Equal(t, false, decodeData[map[string]any](t, w)["connected"])
}

func scheduleCount(t *testing.T, uid uuid.UUID) (n int) {
	t.Helper()
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT COUNT(*) FROM schedules WHERE user_id=$1`, uid).Scan(&n))
	return n
}

func TestGoogleWriteThroughFailureLeavesLocalUntouched(t *testing.T) {
	requireDB(t)
	uid, token := newUser(t)
	e := newGoogleEnv(t, true)
	start := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	// not connected: every mutation is 409 and nothing is stored
	w := doJSON(t, e.r, http.MethodPost, "/schedules", token, gin.H{"title": "No", "start_time": start})
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "not_connected")
	assert.Zero(t, scheduleCount(t, uid))

	connectUser(t, e, uid)
	s := newPushed(t, e, token, gin.H{"title": "Orig", "start_time": start})
	path := "/schedules/" + s.ID.String()

	// Google down: create, update, move, delete answer 502 and leave data as it was
	e.f.pushErr = errors.New("secret detail")
	e.f.deleteErr = errors.New("secret detail")
	w = doJSON(t, e.r, http.MethodPost, "/schedules", token, gin.H{"title": "New", "start_time": start})
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.Contains(t, w.Body.String(), "google_api_error")
	assert.NotContains(t, w.Body.String(), "secret")
	assert.Equal(t, 1, scheduleCount(t, uid))

	w = doJSON(t, e.r, http.MethodPut, path, token, gin.H{"title": "Changed", "start_time": start})
	assert.Equal(t, http.StatusBadGateway, w.Code)
	w = doJSON(t, e.r, http.MethodPost, path+"/move", token, gin.H{"start_time": start.Add(time.Hour)})
	assert.Equal(t, http.StatusBadGateway, w.Code)
	w = doJSON(t, e.r, http.MethodPost, path+"/duplicate", token, nil)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	w = doJSON(t, e.r, http.MethodDelete, path, token, nil)
	assert.Equal(t, http.StatusBadGateway, w.Code)

	var title string
	var st time.Time
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT title, start_time FROM schedules WHERE id=$1`, s.ID).Scan(&title, &st))
	assert.Equal(t, "Orig", title)
	assert.True(t, st.Equal(start))
	assert.Equal(t, 1, scheduleCount(t, uid))

	// recovered: the same calls succeed
	e.f.pushErr, e.f.deleteErr = nil, nil
	w = doJSON(t, e.r, http.MethodDelete, path, token, nil)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Zero(t, scheduleCount(t, uid))

	// revoked grant: integration flagged, writes answer 409 needs_reauth
	e.f.pushErr = googlecal.ErrInvalidGrant
	w = doJSON(t, e.r, http.MethodPost, "/schedules", token, gin.H{"title": "Revoked", "start_time": start})
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "needs_reauth")
	w = doJSON(t, e.r, http.MethodGet, "/integrations/google/status", token, nil)
	assert.Equal(t, "needs_reauth", decodeData[map[string]any](t, w)["status"])
	w = doJSON(t, e.r, http.MethodPost, "/schedules", token, gin.H{"title": "Again", "start_time": start})
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "needs_reauth")
	w = doJSON(t, e.r, http.MethodPost, "/integrations/google/sync", token, nil)
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "needs_reauth")
}

func TestGoogleRetryEndpointGone(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	e := newGoogleEnv(t, true)
	w := doJSON(t, e.r, http.MethodPost, "/schedules/"+uuid.NewString()+"/google-retry", token, nil)
	assert.Equal(t, http.StatusGone, w.Code)
}

func TestGoogleUploadLocal(t *testing.T) {
	requireDB(t)
	uid, token := newUser(t)
	e := newGoogleEnv(t, true)
	start := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	w := doJSON(t, e.r, http.MethodPost, "/integrations/google/upload-local", token, nil)
	assert.Equal(t, http.StatusConflict, w.Code, "not connected")

	connectUser(t, e, uid)
	var plain, bad uuid.UUID
	for title, dst := range map[string]*uuid.UUID{"Plain": &plain, "BadRule": &bad} {
		require.NoError(t, testDB.QueryRow(testCtx, `INSERT INTO schedules (user_id, title, start_time)
			VALUES ($1, $2, $3) RETURNING id`, uid, title, start).Scan(dst))
	}
	_, err := testDB.Exec(testCtx, `INSERT INTO schedule_recurrences (schedule_id, rule) VALUES ($1, 'FREQ=SECONDLY')`, bad)
	require.NoError(t, err)

	w = doJSON(t, e.r, http.MethodGet, "/integrations/google/status", token, nil)
	assert.EqualValues(t, 2, decodeData[map[string]any](t, w)["local_only_count"])

	// Google failure: counted as failed, rows stay unlinked
	e.f.pushErr = errors.New("boom")
	w = doJSON(t, e.r, http.MethodPost, "/integrations/google/upload-local", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, googlecal.UploadStats{Skipped: 1, Failed: 1}, decodeData[googlecal.UploadStats](t, w))
	e.f.pushErr = nil

	w = doJSON(t, e.r, http.MethodPost, "/integrations/google/upload-local", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, googlecal.UploadStats{Uploaded: 1, Skipped: 1}, decodeData[googlecal.UploadStats](t, w))
	require.Len(t, e.f.inserted, 1)
	assert.Equal(t, "Plain", e.f.inserted[0].Title)
	assert.Equal(t, "none", e.f.inserted[0].SendUpdates)

	var evID *string
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT google_event_id FROM schedules WHERE id=$1`, plain).Scan(&evID))
	require.NotNil(t, evID)
	assert.Equal(t, "gid-Plain", *evID)

	w = doJSON(t, e.r, http.MethodGet, "/integrations/google/status", token, nil)
	assert.EqualValues(t, 1, decodeData[map[string]any](t, w)["local_only_count"])
}

func TestGoogleCallbackDoesNotLeak(t *testing.T) {
	requireDB(t)
	e := newGoogleEnv(t, true)
	req := httptest.NewRequest(http.MethodGet, "/callback?code=c&state=x", nil)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusFound, w.Code)
	assert.NotContains(t, w.Header().Get("Location"), "x.")
}

func TestScheduleV2PushFields(t *testing.T) {
	requireDB(t)
	uid, token := newUser(t)
	e := newGoogleEnv(t, true)
	connectUser(t, e, uid)
	start := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	s := newPushed(t, e, token, gin.H{
		"title": "Full", "start_time": start, "add_meet": true,
		"location": "Room 1", "color_id": "5", "timezone": "Asia/Jakarta",
		"reminders": []int{10, 30}, "attendees": []string{"A@Example.com", "b@example.com", "a@example.com"},
		"recurrence": "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE;COUNT=4",
	})
	require.Len(t, e.f.inserted, 1)
	in := e.f.inserted[0]
	assert.Equal(t, "Room 1", in.Location)
	assert.Equal(t, "5", in.ColorID)
	assert.Equal(t, "Asia/Jakarta", in.TimeZone)
	assert.Equal(t, []int{10, 30}, in.Reminders)
	assert.Equal(t, "all", in.SendUpdates)
	assert.True(t, in.AddMeet)
	require.Len(t, in.Attendees, 2)
	assert.Equal(t, "a@example.com", in.Attendees[0].Email)

	require.NotNil(t, s.Location)
	assert.Equal(t, "Room 1", *s.Location)
	assert.Equal(t, "5", *s.ColorID)
	assert.Equal(t, "Asia/Jakarta", s.Timezone)
	require.NotNil(t, s.MeetLink)
	assert.Equal(t, "https://meet.example/abc", *s.MeetLink)
	require.Len(t, s.Attendees, 2)
	assert.Equal(t, "needsAction", s.Attendees[0].ResponseStatus)
	path := "/schedules/" + s.ID.String()

	// omitted attendees/reminders/timezone are kept; add_meet=false drops the link
	w := doJSON(t, e.r, http.MethodPut, path, token, gin.H{"title": "Full2", "start_time": start, "add_meet": false,
		"send_updates": "none", "location": "", "recurrence": "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE;COUNT=4"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeData[Schedule](t, w)
	require.Len(t, e.f.patchedEv, 1)
	p := e.f.patchedEv[0]
	assert.True(t, p.RemoveMeet)
	assert.Equal(t, "none", p.SendUpdates)
	assert.Len(t, p.Attendees, 2)
	assert.Equal(t, []int{10, 30}, p.Reminders)
	assert.Equal(t, "Asia/Jakarta", p.TimeZone)
	assert.Nil(t, got.MeetLink)
	assert.Nil(t, got.Location)
	assert.Len(t, got.Attendees, 2)

	// [] clears guests and reminders
	w = doJSON(t, e.r, http.MethodPut, path, token, gin.H{"title": "Full2", "start_time": start, "attendees": []string{}, "reminders": []int{}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got = decodeData[Schedule](t, w)
	assert.NotNil(t, got.Attendees)
	assert.Empty(t, got.Attendees)
	assert.Empty(t, e.f.patchedEv[1].Attendees)
	assert.Empty(t, e.f.patchedEv[1].Reminders)
	assert.Equal(t, "none", e.f.patchedEv[1].SendUpdates)

	// delete notifies guests only when there are some
	_, err := testDB.Exec(testCtx, `INSERT INTO schedule_attendees (schedule_id, email) VALUES ($1, 'x@y.com')`, s.ID)
	require.NoError(t, err)
	doJSON(t, e.r, http.MethodDelete, path, token, nil)
	assert.Equal(t, []string{"all"}, e.f.deleteSU)
}

func TestScheduleV2Validation(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	e := newGoogleEnv(t, false)
	start := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	many := make([]string, 51)
	for i := range many {
		many[i] = fmt.Sprintf("u%d@example.com", i)
	}

	for code, extra := range map[string]gin.H{
		"invalid_attendee":   {"attendees": []string{"not-an-email"}},
		"invalid_timezone":   {"timezone": "Mars/Base"},
		"invalid_color":      {"color_id": "12"},
		"invalid_recurrence": {"recurrence": "FREQ=SECONDLY"},
	} {
		body := gin.H{"title": "x", "start_time": start}
		for k, v := range extra {
			body[k] = v
		}
		w := doJSON(t, e.r, http.MethodPost, "/schedules", token, body)
		assert.Equal(t, http.StatusBadRequest, w.Code, code)
		assert.Contains(t, w.Body.String(), code)
	}
	w := doJSON(t, e.r, http.MethodPost, "/schedules", token, gin.H{"title": "x", "start_time": start, "attendees": many})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "invalid_attendee")
	w = doJSON(t, e.r, http.MethodPost, "/schedules", token, gin.H{"title": "x", "start_time": start, "location": strings.Repeat("a", 301)})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestValidRecurrence(t *testing.T) {
	for _, ok := range []string{"FREQ=DAILY", "FREQ=WEEKLY;BYDAY=MO,WE;COUNT=10", "FREQ=MONTHLY;BYMONTHDAY=3;UNTIL=20271231T000000Z",
		"FREQ=YEARLY;INTERVAL=2;UNTIL=20271231"} {
		assert.True(t, validRecurrence(ok), ok)
	}
	for _, bad := range []string{"", "RRULE:FREQ=DAILY", "FREQ=HOURLY", "BYDAY=MO", "FREQ=DAILY;COUNT=2;UNTIL=20271231",
		"FREQ=DAILY;BYDAY=XX", "FREQ=DAILY;BYSETPOS=1", "FREQ=DAILY;COUNT=0", "FREQ=DAILY;FREQ=WEEKLY", "FREQ=DAILY;"} {
		assert.False(t, validRecurrence(bad), bad)
	}
}

func TestGoogleImportMapsV2Fields(t *testing.T) {
	requireDB(t)
	uid, token := newUser(t)
	e := newGoogleEnv(t, true)
	connectUser(t, e, uid)
	start := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	e.f.events = []googlecal.Event{{
		ID: "g1", ETag: "e1", Title: "Rich", Start: start, Location: "HQ", ColorID: "7", MeetLink: "https://meet.example/x",
		TimeZone: "Europe/Paris", Reminders: []int{15},
		Attendees: []googlecal.Attendee{{Email: "o@example.com", ResponseStatus: "accepted", Organizer: true},
			{Email: "g@example.com", DisplayName: "Guest", ResponseStatus: "declined"}},
	}}
	w := doJSON(t, e.r, http.MethodPost, "/integrations/google/sync", token, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var id uuid.UUID
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT id FROM schedules WHERE user_id=$1`, uid).Scan(&id))
	s := decodeData[Schedule](t, doJSON(t, e.r, http.MethodGet, "/schedules/"+id.String(), token, nil))
	assert.Equal(t, "HQ", *s.Location)
	assert.Equal(t, "7", *s.ColorID)
	assert.Equal(t, "https://meet.example/x", *s.MeetLink)
	assert.Equal(t, "Europe/Paris", s.Timezone)
	assert.Equal(t, []int{15}, s.Reminders)
	require.Len(t, s.Attendees, 2)
	assert.Equal(t, "declined", s.Attendees[0].ResponseStatus) // ordered by email
	assert.True(t, s.Attendees[1].IsOrganizer)
}
