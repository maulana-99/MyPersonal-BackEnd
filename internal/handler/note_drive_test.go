package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const folderMimeT = "application/vnd.google-apps.folder"

// fakeDrive is an in-memory googlecal.DriveClient; no network.
type fakeDrive struct {
	files   map[string]*googlecal.DriveFile
	bodies  map[string]string
	seq     int
	queries []string
	err     error // returned by every call when set
}

func newFakeDrive() *fakeDrive {
	return &fakeDrive{files: map[string]*googlecal.DriveFile{}, bodies: map[string]string{}}
}

func (f *fakeDrive) add(df googlecal.DriveFile, body string) string {
	f.seq++
	df.ID = fmt.Sprintf("id%d", f.seq)
	df.Created, df.Modified = time.Now(), time.Now()
	f.files[df.ID] = &df
	f.bodies[df.ID] = body
	return df.ID
}

func (f *fakeDrive) folders() (ids []string) {
	for id, df := range f.files {
		if df.MimeType == folderMimeT && !df.Trashed {
			ids = append(ids, id)
		}
	}
	return
}

func (f *fakeDrive) List(_ context.Context, _ string, o googlecal.DriveListOpts) (googlecal.DriveList, error) {
	if f.err != nil {
		return googlecal.DriveList{}, f.err
	}
	f.queries = append(f.queries, o.Query)
	var out googlecal.DriveList
	for id, df := range f.files {
		isFolderQ := strings.Contains(o.Query, folderMimeT)
		switch {
		case isFolderQ && df.MimeType == folderMimeT && !df.Trashed:
		case !isFolderQ && df.MimeType != folderMimeT && len(df.Parents) > 0 &&
			strings.Contains(o.Query, "'"+df.Parents[0]+"' in parents") &&
			(!strings.Contains(o.Query, "trashed =") || strings.Contains(o.Query, fmt.Sprintf("trashed = %t", df.Trashed))):
		default:
			continue
		}
		_ = id
		out.Files = append(out.Files, *df)
	}
	return out, nil
}

func (f *fakeDrive) Get(_ context.Context, _, id string) (googlecal.DriveFile, error) {
	if f.err != nil {
		return googlecal.DriveFile{}, f.err
	}
	df, ok := f.files[id]
	if !ok {
		return googlecal.DriveFile{}, googlecal.ErrDriveNotFound
	}
	return *df, nil
}

func (f *fakeDrive) Read(_ context.Context, _, id string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.bodies[id], nil
}

func (f *fakeDrive) Create(_ context.Context, _ string, df googlecal.DriveFile, body *string) (googlecal.DriveFile, error) {
	if f.err != nil {
		return googlecal.DriveFile{}, f.err
	}
	b := ""
	if body != nil {
		b = *body
	}
	return *f.files[f.add(df, b)], nil
}

func (f *fakeDrive) Update(_ context.Context, _, id string, u googlecal.DriveUpdate) (googlecal.DriveFile, error) {
	if f.err != nil {
		return googlecal.DriveFile{}, f.err
	}
	df, ok := f.files[id]
	if !ok {
		return googlecal.DriveFile{}, googlecal.ErrDriveNotFound
	}
	if u.Name != nil {
		df.Name = *u.Name
	}
	if u.Description != nil {
		df.Description = *u.Description
	}
	if u.Body != nil {
		f.bodies[id] = *u.Body
	}
	if u.Trashed != nil {
		df.Trashed = *u.Trashed
	}
	if df.Props == nil {
		df.Props = map[string]string{}
	}
	for k, v := range u.Props {
		df.Props[k] = v
	}
	df.Modified = time.Now()
	return *df, nil
}

func (f *fakeDrive) Delete(_ context.Context, _, id string) error {
	if f.err != nil {
		return f.err
	}
	delete(f.files, id)
	return nil
}

type notesEnv struct {
	fd    *fakeDrive
	r     *gin.Engine
	uid   uuid.UUID
	token string
}

// newNotesEnv builds a router with a Google-connected user and a fake Drive.
func newNotesEnv(t *testing.T) notesEnv {
	t.Helper()
	requireDB(t)
	uid, token := newUser(t)
	ge := newGoogleEnv(t, true)
	connectUser(t, ge, uid)
	fd := newFakeDrive()
	nh := NewNoteHandler(testDB)
	nh.SetDrive(googlecal.NewNotesService(ge.svc, fd))
	r := newProtectedRouter(func(g *gin.RouterGroup) { RegisterNoteRoutes(g, nh) })
	return notesEnv{fd, r, uid, token}
}

func (e notesEnv) do(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	w := doJSON(t, e.r, method, path, e.token, body)
	return w.Code, w.Body.String()
}

func (e notesEnv) create(t *testing.T, body gin.H) googlecal.Note {
	t.Helper()
	w := doJSON(t, e.r, http.MethodPost, "/notes", e.token, body)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	return decodeData[googlecal.Note](t, w)
}

type driveList struct {
	Notes    []googlecal.Note `json:"notes"`
	Next     string           `json:"next_page_token"`
	LocalCnt int              `json:"local_count"`
}

func TestDriveNotesCRUD(t *testing.T) {
	e := newNotesEnv(t)

	n := e.create(t, gin.H{"title": "Belanja/minggu", "body": "- [ ] susu\n- [x] roti", "pinned": true,
		"color": "blue", "labels": []string{"Home", "todo"}})
	assert.Len(t, e.fd.folders(), 1, "folder created on first use")
	assert.Equal(t, "Belanjaminggu", n.Title, "unsafe chars stripped from the file name")
	assert.True(t, n.Pinned)
	assert.Equal(t, []string{"home", "todo"}, n.Labels)
	require.NotNil(t, n.Color)
	assert.Equal(t, "blue", *n.Color)
	assert.Equal(t, "- [ ] susu - [x] roti", n.Preview)
	assert.Equal(t, "Belanjaminggu.md", e.fd.files[n.ID].Name)

	code, body := e.do(t, http.MethodGet, "/notes/"+n.ID, nil)
	require.Equal(t, 200, code)
	assert.Contains(t, body, "susu")

	code, body = e.do(t, http.MethodPut, "/notes/"+n.ID, gin.H{"title": "Baru", "body": "isi"})
	require.Equal(t, 200, code, body)
	got := e.fd.files[n.ID]
	assert.Equal(t, "Baru.md", got.Name)
	assert.Equal(t, "isi", e.fd.bodies[n.ID])
	assert.Equal(t, "0", got.Props["pinned"], "PUT resets omitted flags")

	code, body = e.do(t, http.MethodPatch, "/notes/"+n.ID, gin.H{"pinned": true, "color": "pink", "labels": []string{"x"}})
	require.Equal(t, 200, code, body)
	assert.Equal(t, "isi", e.fd.bodies[n.ID], "PATCH does not touch content")
	assert.Equal(t, "pink", e.fd.files[n.ID].Props["color"])
	code, body = e.do(t, http.MethodPatch, "/notes/"+n.ID, gin.H{"color": nil})
	require.Equal(t, 200, code, body)
	assert.Contains(t, body, `"color":null`)

	code, _ = e.do(t, http.MethodDelete, "/notes/"+n.ID, nil)
	assert.Equal(t, 204, code)
	assert.True(t, e.fd.files[n.ID].Trashed)
	code, body = e.do(t, http.MethodPatch, "/notes/"+n.ID, gin.H{"trashed": false})
	require.Equal(t, 200, code, body)
	assert.False(t, e.fd.files[n.ID].Trashed)

	code, _ = e.do(t, http.MethodDelete, "/notes/"+n.ID+"?permanent=true", nil)
	assert.Equal(t, 204, code)
	assert.NotContains(t, e.fd.files, n.ID)
	code, _ = e.do(t, http.MethodGet, "/notes/"+n.ID, nil)
	assert.Equal(t, 404, code)
}

func TestDriveNotesValidation(t *testing.T) {
	e := newNotesEnv(t)
	for name, b := range map[string]gin.H{
		"invalid_color": {"title": "a", "color": "red"},
		"invalid_label": {"title": "a", "labels": []string{"Bad Label"}},
	} {
		code, body := e.do(t, http.MethodPost, "/notes", b)
		assert.Equal(t, 400, code)
		assert.Contains(t, body, name)
	}
	code, _ := e.do(t, http.MethodPost, "/notes", gin.H{"title": ""})
	assert.Equal(t, 400, code)
	nine := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	code, body := e.do(t, http.MethodPost, "/notes", gin.H{"title": "a", "labels": nine})
	assert.Equal(t, 400, code, body)
	assert.Empty(t, e.fd.folders(), "nothing reaches Drive when validation fails")
	code, _ = e.do(t, http.MethodGet, "/notes/bad%20id", nil)
	assert.Equal(t, 400, code)
}

func TestDriveNotesList(t *testing.T) {
	e := newNotesEnv(t)
	e.create(t, gin.H{"title": "satu"})
	e.create(t, gin.H{"title": "dua", "pinned": true, "labels": []string{"kerja"}})
	arch := e.create(t, gin.H{"title": "tiga"})
	e.do(t, http.MethodPatch, "/notes/"+arch.ID, gin.H{"archived": true})

	_, _ = e.do(t, http.MethodPost, "/notes", gin.H{"title": "x"})
	var lc int
	require.NoError(t, testDB.QueryRow(testCtx, `INSERT INTO notes (user_id, title) VALUES ($1, 'lokal') RETURNING 1`, e.uid).Scan(&lc))

	w := doJSON(t, e.r, http.MethodGet, "/notes?label=kerja", e.token, nil)
	require.Equal(t, 200, w.Code, w.Body.String())
	l := decodeData[driveList](t, w)
	require.Len(t, l.Notes, 1)
	assert.Equal(t, "dua", l.Notes[0].Title)
	assert.Equal(t, 1, l.LocalCnt)
	assert.Empty(t, l.Notes[0].Body)
	last := e.fd.queries[len(e.fd.queries)-1]
	assert.Contains(t, last, "not appProperties has", "archived hidden by default")
	assert.Contains(t, last, "trashed = false")

	w = doJSON(t, e.r, http.MethodGet, "/notes", e.token, nil)
	l = decodeData[driveList](t, w)
	require.GreaterOrEqual(t, len(l.Notes), 3)
	assert.True(t, l.Notes[0].Pinned, "pinned first")

	doJSON(t, e.r, http.MethodGet, "/notes?archived=true", e.token, nil)
	last = e.fd.queries[len(e.fd.queries)-1]
	assert.Contains(t, last, "and appProperties has")
	assert.NotContains(t, last, "not appProperties")

	doJSON(t, e.r, http.MethodGet, "/notes?trashed=true", e.token, nil)
	assert.Contains(t, e.fd.queries[len(e.fd.queries)-1], "trashed = true")

	doJSON(t, e.r, http.MethodGet, `/notes?q=it%27s%5Cx`, e.token, nil)
	assert.Contains(t, e.fd.queries[len(e.fd.queries)-1], `fullText contains 'it\'s\\x' or name contains 'it\'s\\x'`)
}

func TestDriveNotesFolderRecreate(t *testing.T) {
	e := newNotesEnv(t)
	e.create(t, gin.H{"title": "a"})
	old := e.fd.folders()
	require.Len(t, old, 1)
	delete(e.fd.files, old[0]) // folder removed in Drive
	e.create(t, gin.H{"title": "b"})
	now := e.fd.folders()
	require.Len(t, now, 1)
	assert.NotEqual(t, old[0], now[0])
	var stored string
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT notes_folder_id FROM integrations_google WHERE user_id=$1`, e.uid).Scan(&stored))
	assert.Equal(t, now[0], stored)

	e.fd.files[now[0]].Trashed = true // trashed folder: reuse an existing live one or create anew
	e.create(t, gin.H{"title": "c"})
	assert.Len(t, e.fd.folders(), 1)
}

func TestDriveNotesOutsideFolder(t *testing.T) {
	e := newNotesEnv(t)
	e.create(t, gin.H{"title": "a"})
	other := e.fd.add(googlecal.DriveFile{Name: "x.md", Parents: []string{"elsewhere"}}, "secret")
	for _, c := range []struct {
		m    string
		body any
	}{
		{http.MethodGet, nil}, {http.MethodPut, gin.H{"title": "t"}},
		{http.MethodPatch, gin.H{"pinned": true}}, {http.MethodDelete, nil},
	} {
		code, body := e.do(t, c.m, "/notes/"+other, c.body)
		assert.Equal(t, 404, code, c.m)
		assert.NotContains(t, body, "secret")
	}
	assert.Contains(t, e.fd.files, other)
	assert.False(t, e.fd.files[other].Trashed)
}

func TestDriveNotesErrors(t *testing.T) {
	e := newNotesEnv(t)
	e.fd.err = googlecal.ErrDriveScope
	code, body := e.do(t, http.MethodGet, "/notes", nil)
	assert.Equal(t, 409, code)
	assert.Contains(t, body, "needs_reauth")
	assert.Contains(t, body, "drive_scope_missing")

	e.fd.err = googlecal.ErrDriveDisabled
	code, body = e.do(t, http.MethodGet, "/notes", nil)
	assert.Equal(t, 502, code)
	assert.Contains(t, body, "drive_api_disabled")

	e.fd.err = fmt.Errorf("boom")
	code, body = e.do(t, http.MethodGet, "/notes", nil)
	assert.Equal(t, 502, code)
	assert.Contains(t, body, "google_api_error")

	e.fd.err = googlecal.ErrInvalidGrant
	code, body = e.do(t, http.MethodGet, "/notes", nil)
	assert.Equal(t, 409, code)
	assert.Contains(t, body, "needs_reauth")
}

func TestDriveNotesImportLocalIdempotent(t *testing.T) {
	e := newNotesEnv(t)
	for _, title := range []string{"lama1", "lama2"} {
		_, err := testDB.Exec(testCtx, `INSERT INTO notes (user_id, title, body) VALUES ($1, $2, 'isi')`, e.uid, title)
		require.NoError(t, err)
	}
	for i, want := range []string{`"imported":2,"skipped":0`, `"imported":0,"skipped":2`} {
		code, body := e.do(t, http.MethodPost, "/notes/import-local", nil)
		require.Equal(t, 200, code, body)
		assert.Contains(t, body, want, "run %d", i)
	}
	var cnt int
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT COUNT(*) FROM notes WHERE user_id=$1`, e.uid).Scan(&cnt))
	assert.Equal(t, 2, cnt, "local rows stay")
}

// Google configured but the user never connected: notes stay local.
func TestDriveNotesLocalFallback(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	ge := newGoogleEnv(t, true)
	nh := NewNoteHandler(testDB)
	nh.SetDrive(googlecal.NewNotesService(ge.svc, newFakeDrive()))
	r := newProtectedRouter(func(g *gin.RouterGroup) { RegisterNoteRoutes(g, nh) })

	w := doJSON(t, r, http.MethodPost, "/notes", token, gin.H{"title": "lokal", "body": "isi"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	n := decodeData[Note](t, w)
	assert.False(t, n.Pinned)
	assert.NotNil(t, n.Labels)
	w = doJSON(t, r, http.MethodGet, "/notes", token, nil)
	assert.Len(t, decodeData[[]Note](t, w), 1)
	w = doJSON(t, r, http.MethodPost, "/notes/import-local", token, nil)
	assert.Equal(t, 409, w.Code)
}

func TestNoteRoutesRegistered(t *testing.T) {
	r := gin.New()
	RegisterNoteRoutes(r.Group("/api/v1"), NewNoteHandler(nil))
	got := map[string]bool{}
	for _, ri := range r.Routes() {
		got[ri.Method+" "+ri.Path] = true
	}
	for _, want := range []string{"GET /api/v1/notes", "POST /api/v1/notes", "POST /api/v1/notes/import-local",
		"GET /api/v1/notes/:id", "PUT /api/v1/notes/:id", "PATCH /api/v1/notes/:id", "DELETE /api/v1/notes/:id"} {
		assert.True(t, got[want], want)
	}
}
