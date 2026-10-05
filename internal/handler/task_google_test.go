package handler

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTasks is an in-memory googlecal.TasksClient; no network. Items carry
// their list in ListID; "def" is the default list.
type fakeTasks struct {
	items   map[string]*googlecal.TaskItem
	lists   []googlecal.TaskListItem
	seq     int
	err     error
	cleared string
}

func newFakeTasks() *fakeTasks {
	return &fakeTasks{items: map[string]*googlecal.TaskItem{}, lists: []googlecal.TaskListItem{{ID: "def", Title: "My Tasks"}}}
}

func (f *fakeTasks) hasList(id string) bool {
	for _, l := range f.lists {
		if l.ID == id {
			return true
		}
	}
	return false
}

// lid maps the alias and reports unknown lists as not found.
func (f *fakeTasks) lid(list string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if list == "@default" {
		return "def", nil
	}
	if !f.hasList(list) {
		return "", googlecal.ErrTaskNotFound
	}
	return list, nil
}

func (f *fakeTasks) put(it googlecal.TaskItem) string {
	f.seq++
	it.ID = fmt.Sprintf("t%d", f.seq)
	if it.Status == "" {
		it.Status = "pending"
	}
	if it.ListID == "" {
		it.ListID = "def"
	}
	it.Position = fmt.Sprintf("%020d", f.seq)
	f.items[it.ID] = &it
	return it.ID
}

func (f *fakeTasks) List(_ context.Context, _, list string, o googlecal.TaskListOpts) ([]googlecal.TaskItem, string, error) {
	list, err := f.lid(list)
	if err != nil {
		return nil, "", err
	}
	var out []googlecal.TaskItem
	for _, it := range f.items {
		if it.ListID != list || it.Status == "completed" && !o.WithCompleted {
			continue
		}
		out = append(out, *it)
	}
	return out, "", nil
}

// in returns the item if it lives in list.
func (f *fakeTasks) in(list, id string) (*googlecal.TaskItem, error) {
	list, err := f.lid(list)
	if err != nil {
		return nil, err
	}
	it, ok := f.items[id]
	if !ok || it.ListID != list {
		return nil, googlecal.ErrTaskNotFound
	}
	return it, nil
}

func (f *fakeTasks) Get(_ context.Context, _, list, id string) (googlecal.TaskItem, error) {
	it, err := f.in(list, id)
	if err != nil {
		return googlecal.TaskItem{}, err
	}
	return *it, nil
}

func (f *fakeTasks) Insert(_ context.Context, _, list string, in googlecal.TaskInput) (googlecal.TaskItem, error) {
	list, err := f.lid(list)
	if err != nil {
		return googlecal.TaskItem{}, err
	}
	it := googlecal.TaskItem{Title: in.Title, Notes: in.Notes, DueDate: in.DueDate, ListID: list}
	if in.Parent != "" {
		p := in.Parent
		it.Parent = &p
	}
	return *f.items[f.put(it)], nil
}

func (f *fakeTasks) Patch(_ context.Context, _, list, id string, u googlecal.TaskUpdate) (googlecal.TaskItem, error) {
	it, err := f.in(list, id)
	if err != nil {
		return googlecal.TaskItem{}, err
	}
	if u.Title != nil {
		it.Title = *u.Title
	}
	if u.Notes != nil {
		it.Notes = *u.Notes
	}
	if u.ClearDue {
		it.DueDate = nil
	} else if u.Due != nil {
		it.DueDate = u.Due
	}
	if u.Status != nil {
		if *u.Status == "completed" {
			now := time.Now()
			it.Status, it.CompletedAt = "completed", &now
		} else {
			it.Status, it.CompletedAt = "pending", nil
		}
	}
	return *it, nil
}

func (f *fakeTasks) Delete(_ context.Context, _, list, id string) error {
	if _, err := f.in(list, id); err != nil {
		return err
	}
	delete(f.items, id)
	for k, it := range f.items {
		if it.Parent != nil && *it.Parent == id {
			delete(f.items, k)
		}
	}
	return nil
}

func (f *fakeTasks) Clear(_ context.Context, _, list string) error {
	list, err := f.lid(list)
	if err == nil {
		f.cleared = list
	}
	return err
}

// Move mimics tasks.move: renumber the siblings of the target level.
func (f *fakeTasks) Move(_ context.Context, _, list, id string, m googlecal.MoveOpts) (googlecal.TaskItem, error) {
	it, err := f.in(list, id)
	if err != nil {
		return googlecal.TaskItem{}, err
	}
	if m.Dest != "" {
		dest, err := f.lid(m.Dest)
		if err != nil {
			return googlecal.TaskItem{}, err
		}
		it.ListID, it.Parent = dest, nil
		for _, k := range f.items {
			if k.Parent != nil && *k.Parent == id {
				k.ListID = dest
			}
		}
		m = googlecal.MoveOpts{}
	}
	it.Parent = nil
	if m.Parent != "" {
		p := m.Parent
		it.Parent = &p
	}
	var sib []*googlecal.TaskItem
	for _, k := range f.items {
		if k.ID != id && k.ListID == it.ListID && (k.Parent == nil) == (it.Parent == nil) && (k.Parent == nil || *k.Parent == m.Parent) {
			sib = append(sib, k)
		}
	}
	sort.Slice(sib, func(a, b int) bool { return sib[a].Position < sib[b].Position })
	at := 0
	for i, k := range sib {
		if k.ID == m.Previous {
			at = i + 1
		}
	}
	sib = append(sib[:at], append([]*googlecal.TaskItem{it}, sib[at:]...)...)
	for i, k := range sib {
		k.Position = fmt.Sprintf("%020d", i+1)
	}
	return *it, nil
}

func (f *fakeTasks) Lists(context.Context, string) ([]googlecal.TaskListItem, error) {
	return append([]googlecal.TaskListItem(nil), f.lists...), f.err
}

func (f *fakeTasks) ListGet(_ context.Context, _, id string) (googlecal.TaskListItem, error) {
	id, err := f.lid(id)
	if err != nil {
		return googlecal.TaskListItem{}, err
	}
	for _, l := range f.lists {
		if l.ID == id {
			return l, nil
		}
	}
	return googlecal.TaskListItem{}, googlecal.ErrTaskNotFound
}

func (f *fakeTasks) ListInsert(_ context.Context, _, title string) (googlecal.TaskListItem, error) {
	if f.err != nil {
		return googlecal.TaskListItem{}, f.err
	}
	l := googlecal.TaskListItem{ID: fmt.Sprintf("L%d", len(f.lists)+1), Title: title}
	f.lists = append(f.lists, l)
	return l, nil
}

func (f *fakeTasks) ListRename(_ context.Context, _, id, title string) (googlecal.TaskListItem, error) {
	if _, err := f.lid(id); err != nil {
		return googlecal.TaskListItem{}, err
	}
	for i := range f.lists {
		if f.lists[i].ID == id {
			f.lists[i].Title = title
			return f.lists[i], nil
		}
	}
	return googlecal.TaskListItem{}, googlecal.ErrTaskNotFound
}

func (f *fakeTasks) ListDelete(_ context.Context, _, id string) error {
	if _, err := f.lid(id); err != nil {
		return err
	}
	for i := range f.lists {
		if f.lists[i].ID == id {
			f.lists = append(f.lists[:i], f.lists[i+1:]...)
		}
	}
	for k, it := range f.items {
		if it.ListID == id {
			delete(f.items, k)
		}
	}
	return nil
}

type tasksEnv struct {
	ft    *fakeTasks
	r     *gin.Engine
	uid   uuid.UUID
	token string
}

func newTasksEnv(t *testing.T) tasksEnv {
	t.Helper()
	requireDB(t)
	uid, token := newUser(t)
	ge := newGoogleEnv(t, true)
	connectUser(t, ge, uid)
	ft := newFakeTasks()
	svc := googlecal.NewTasksService(ge.svc, ft)
	th := NewTaskHandler(testDB)
	th.SetGoogle(svc)
	td := NewTodayHandler(testDB)
	td.SetGoogle(svc)
	r := newProtectedRouter(func(g *gin.RouterGroup) {
		RegisterTaskRoutes(g, th)
		g.GET("/today", td.Get)
	})
	return tasksEnv{ft, r, uid, token}
}

func (e tasksEnv) do(t *testing.T, method, path string, body any) (int, string) {
	t.Helper()
	w := doJSON(t, e.r, method, path, e.token, body)
	return w.Code, w.Body.String()
}

type gTaskList struct {
	Tasks []googlecal.TaskItem `json:"tasks"`
	Local int                  `json:"local_count"`
}

func ds(s string) *string { return &s }

func TestGoogleTasksCRUDToggleDelete(t *testing.T) {
	e := newTasksEnv(t)
	w := doJSON(t, e.r, http.MethodPost, "/tasks", e.token, gin.H{"title": "Bayar", "notes": "n", "due_date": "2026-10-12"})
	require.Equal(t, 201, w.Code, w.Body.String())
	it := decodeData[googlecal.TaskItem](t, w)
	assert.Equal(t, "2026-10-12", *e.ft.items[it.ID].DueDate)

	code, body := e.do(t, http.MethodPost, "/tasks", gin.H{"title": "x", "due_date": "12/10"})
	assert.Equal(t, 400, code)
	assert.Contains(t, body, "invalid_due_date")
	code, _ = e.do(t, http.MethodPost, "/tasks", gin.H{"title": "x", "parent": "nope"})
	assert.Equal(t, 400, code)
	code, _ = e.do(t, http.MethodPost, "/tasks", gin.H{"title": ""})
	assert.Equal(t, 400, code)
	code, _ = e.do(t, http.MethodGet, "/tasks/bad%20id", nil)
	assert.Equal(t, 400, code)

	code, body = e.do(t, http.MethodPut, "/tasks/"+it.ID, gin.H{"title": "Baru", "due_date": nil})
	require.Equal(t, 200, code, body)
	assert.Nil(t, e.ft.items[it.ID].DueDate)
	assert.Equal(t, "", e.ft.items[it.ID].Notes, "PUT replaces notes")

	code, body = e.do(t, http.MethodPost, "/tasks/"+it.ID+"/complete", nil)
	require.Equal(t, 200, code, body)
	assert.Equal(t, "completed", e.ft.items[it.ID].Status)
	e.do(t, http.MethodPost, "/tasks/"+it.ID+"/complete", nil)
	assert.Equal(t, "pending", e.ft.items[it.ID].Status)

	sub := decodeData[googlecal.TaskItem](t, doJSON(t, e.r, http.MethodPost, "/tasks", e.token, gin.H{"title": "anak", "parent": it.ID}))
	require.NotNil(t, sub.Parent)
	code, _ = e.do(t, http.MethodPost, "/tasks", gin.H{"title": "cucu", "parent": sub.ID})
	assert.Equal(t, 400, code, "one level only")

	code, _ = e.do(t, http.MethodDelete, "/tasks/"+it.ID, nil)
	assert.Equal(t, 204, code)
	assert.Empty(t, e.ft.items)
	code, _ = e.do(t, http.MethodDelete, "/tasks/"+it.ID, nil)
	assert.Equal(t, 404, code)

	code, _ = e.do(t, http.MethodPost, "/tasks/clear-completed", nil)
	assert.Equal(t, 204, code)
	assert.Equal(t, "def", e.ft.cleared)
}

func TestGoogleTasksListFiltersAndOrder(t *testing.T) {
	e := newTasksEnv(t)
	today := time.Now().UTC().Format("2006-01-02")
	day := func(n int) *string { return ds(time.Now().UTC().AddDate(0, 0, n).Format("2006-01-02")) }
	none := e.ft.put(googlecal.TaskItem{Title: "none"})
	up := e.ft.put(googlecal.TaskItem{Title: "up", DueDate: day(3)})
	td := e.ft.put(googlecal.TaskItem{Title: "today", DueDate: ds(today)})
	od2 := e.ft.put(googlecal.TaskItem{Title: "od2", DueDate: day(-1)})
	od1 := e.ft.put(googlecal.TaskItem{Title: "od1", DueDate: day(-5)})
	kid := e.ft.put(googlecal.TaskItem{Title: "kid", Parent: &od1})
	old := time.Now().AddDate(0, 0, -2)
	recent := time.Now()
	e.ft.put(googlecal.TaskItem{Title: "d-old", Status: "completed", CompletedAt: &old})
	e.ft.put(googlecal.TaskItem{Title: "d-new", Status: "completed", CompletedAt: &recent})
	_, err := testDB.Exec(testCtx, `INSERT INTO tasks (user_id, title) VALUES ($1, 'lokal')`, e.uid)
	require.NoError(t, err)

	ids := func(path string) []string {
		w := doJSON(t, e.r, http.MethodGet, path, e.token, nil)
		require.Equal(t, 200, w.Code, w.Body.String())
		var out []string
		for _, it := range decodeData[gTaskList](t, w).Tasks {
			out = append(out, it.ID)
		}
		return out
	}
	assert.Equal(t, []string{od1, kid, od2, td, up, none}, ids("/tasks"))
	assert.Equal(t, []string{od1, od2}, ids("/tasks?due=overdue"))
	assert.Equal(t, []string{td}, ids("/tasks?due=today"))
	assert.Equal(t, []string{none, kid}, ids("/tasks?due=none"))
	c := ids("/tasks?show=completed")
	require.Len(t, c, 2)
	assert.Equal(t, "d-new", e.ft.items[c[0]].Title, "newest first")
	assert.Len(t, ids("/tasks?show=all"), 8)
	w := doJSON(t, e.r, http.MethodGet, "/tasks", e.token, nil)
	assert.Equal(t, 1, decodeData[gTaskList](t, w).Local)
	for _, p := range []string{"/tasks?show=x", "/tasks?due=x", "/tasks?tz=Mars/Base"} {
		code, _ := e.do(t, http.MethodGet, p, nil)
		assert.Equal(t, 400, code, p)
	}
}

func TestGoogleTasksImportLocalIdempotent(t *testing.T) {
	e := newTasksEnv(t)
	for _, q := range []string{`('a', '2026-10-12')`, `('b', NULL)`} {
		_, err := testDB.Exec(testCtx, `INSERT INTO tasks (user_id, title, due_date) SELECT $1, v.t, v.d::date FROM (VALUES `+q+`) v(t, d)`, e.uid)
		require.NoError(t, err)
	}
	for i, want := range []string{`"imported":2,"skipped":0`, `"imported":0,"skipped":2`} {
		code, body := e.do(t, http.MethodPost, "/tasks/import-local", nil)
		require.Equal(t, 200, code, body)
		assert.Contains(t, body, want, "run %d", i)
	}
	assert.Len(t, e.ft.items, 2)
	var n int
	require.NoError(t, testDB.QueryRow(testCtx, `SELECT COUNT(*) FROM tasks WHERE user_id=$1`, e.uid).Scan(&n))
	assert.Equal(t, 2, n, "local rows stay")
}

func TestGoogleTasksToday(t *testing.T) {
	e := newTasksEnv(t)
	now := time.Now()
	today := now.Format("2006-01-02")
	e.ft.put(googlecal.TaskItem{Title: "late", DueDate: ds(now.AddDate(0, 0, -2).Format("2006-01-02"))})
	e.ft.put(googlecal.TaskItem{Title: "future", DueDate: ds(now.AddDate(0, 0, 2).Format("2006-01-02"))})
	e.ft.put(googlecal.TaskItem{Title: "undated"})
	e.ft.put(googlecal.TaskItem{Title: "now", DueDate: ds(today)})
	e.ft.put(googlecal.TaskItem{Title: "done", Status: "completed", CompletedAt: &now, DueDate: ds(today)})
	y := now.AddDate(0, 0, -3)
	e.ft.put(googlecal.TaskItem{Title: "doneold", Status: "completed", CompletedAt: &y})

	w := doJSON(t, e.r, http.MethodGet, "/today", e.token, nil)
	require.Equal(t, 200, w.Code, w.Body.String())
	d := decodeData[struct {
		Tasks    []googlecal.TaskItem `json:"tasks"`
		Progress TodayProgress        `json:"progress"`
	}](t, w)
	var titles []string
	for _, it := range d.Tasks {
		titles = append(titles, it.Title)
	}
	assert.Equal(t, []string{"late", "now", "done"}, titles)
	assert.Equal(t, TodayProgress{Total: 3, Completed: 1, Percentage: 33}, d.Progress)
	assert.NotContains(t, w.Body.String(), "priority")
}

func TestGoogleTasksErrors(t *testing.T) {
	e := newTasksEnv(t)
	e.ft.err = googlecal.ErrTasksScope
	code, body := e.do(t, http.MethodGet, "/tasks", nil)
	assert.Equal(t, 409, code)
	assert.Contains(t, body, "tasks_scope_missing")
	e.ft.err = googlecal.ErrTasksDisabled
	code, body = e.do(t, http.MethodGet, "/tasks", nil)
	assert.Equal(t, 502, code)
	assert.Contains(t, body, "tasks_api_disabled")
	e.ft.err = fmt.Errorf("boom")
	code, body = e.do(t, http.MethodGet, "/tasks", nil)
	assert.Equal(t, 502, code)
	assert.Contains(t, body, "google_api_error")
	code, body = e.do(t, http.MethodGet, "/today", nil)
	assert.Equal(t, 200, code, "today survives a Tasks failure")
	assert.Contains(t, body, "tasks_error")
}

// Google configured but not connected: the local table and its shape stay.
func TestGoogleTasksLocalFallback(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	ge := newGoogleEnv(t, true)
	th := NewTaskHandler(testDB)
	th.SetGoogle(googlecal.NewTasksService(ge.svc, newFakeTasks()))
	r := newProtectedRouter(func(g *gin.RouterGroup) { RegisterTaskRoutes(g, th) })
	w := doJSON(t, r, http.MethodPost, "/tasks", token, gin.H{"title": "lokal", "priority": "high"})
	require.Equal(t, 201, w.Code, w.Body.String())
	assert.Equal(t, "high", decodeData[Task](t, w).Priority)
	w = doJSON(t, r, http.MethodGet, "/tasks", token, nil)
	assert.Len(t, decodeData[[]Task](t, w), 1)
	assert.Equal(t, 409, doJSON(t, r, http.MethodPost, "/tasks/import-local", token, nil).Code)
}

func TestTaskRoutesRegistered(t *testing.T) {
	r := gin.New()
	RegisterTaskRoutes(r.Group("/api/v1"), NewTaskHandler(nil))
	got := map[string]bool{}
	for _, ri := range r.Routes() {
		got[ri.Method+" "+ri.Path] = true
	}
	for _, want := range []string{"GET /api/v1/tasks", "POST /api/v1/tasks", "POST /api/v1/tasks/clear-completed",
		"POST /api/v1/tasks/import-local", "POST /api/v1/tasks/:id/move", "GET /api/v1/tasklists", "POST /api/v1/tasklists",
		"PATCH /api/v1/tasklists/:id", "DELETE /api/v1/tasklists/:id", "PUT /api/v1/tasks/:id", "DELETE /api/v1/tasks/:id", "POST /api/v1/tasks/:id/complete"} {
		assert.True(t, got[want], want)
	}
}

func TestGoogleTaskListsCRUD(t *testing.T) {
	e := newTasksEnv(t)
	code, body := e.do(t, http.MethodGet, "/tasklists", nil)
	require.Equal(t, 200, code, body)
	assert.Contains(t, body, `"is_default":true`)

	w := doJSON(t, e.r, http.MethodPost, "/tasklists", e.token, gin.H{"title": " Kerja "})
	require.Equal(t, 201, w.Code, w.Body.String())
	l := decodeData[googlecal.TaskListItem](t, w)
	assert.Equal(t, "Kerja", l.Title)
	assert.False(t, l.IsDefault)
	for _, bad := range []gin.H{{"title": ""}, {"title": "   "}, {"title": string(make([]byte, 101))}} {
		code, _ = e.do(t, http.MethodPost, "/tasklists", bad)
		assert.Equal(t, 400, code)
	}

	code, body = e.do(t, http.MethodPatch, "/tasklists/"+l.ID, gin.H{"title": "Rumah"})
	require.Equal(t, 200, code, body)
	assert.Equal(t, "Rumah", e.ft.lists[1].Title)
	code, _ = e.do(t, http.MethodPatch, "/tasklists/nope", gin.H{"title": "x"})
	assert.Equal(t, 404, code)
	code, _ = e.do(t, http.MethodPatch, "/tasklists/bad%20id", gin.H{"title": "x"})
	assert.Equal(t, 400, code)

	for _, id := range []string{"def", "@default"} {
		code, body = e.do(t, http.MethodDelete, "/tasklists/"+id, nil)
		assert.Equal(t, 400, code)
		assert.Contains(t, body, "cannot_delete_default")
	}
	e.ft.put(googlecal.TaskItem{Title: "di L", ListID: l.ID})
	code, _ = e.do(t, http.MethodDelete, "/tasklists/"+l.ID, nil)
	assert.Equal(t, 204, code)
	assert.Len(t, e.ft.lists, 1)
	assert.Empty(t, e.ft.items)
	code, _ = e.do(t, http.MethodDelete, "/tasklists/"+l.ID, nil)
	assert.Equal(t, 404, code)
}

func TestGoogleTasksListParamPlumbing(t *testing.T) {
	e := newTasksEnv(t)
	l := decodeData[googlecal.TaskListItem](t, doJSON(t, e.r, http.MethodPost, "/tasklists", e.token, gin.H{"title": "B"}))
	q := "?list=" + l.ID

	w := doJSON(t, e.r, http.MethodPost, "/tasks", e.token, gin.H{"title": "satu", "list": l.ID})
	require.Equal(t, 201, w.Code, w.Body.String())
	a := decodeData[googlecal.TaskItem](t, w)
	assert.Equal(t, l.ID, a.ListID)
	d := decodeData[googlecal.TaskItem](t, doJSON(t, e.r, http.MethodPost, "/tasks", e.token, gin.H{"title": "default"}))
	assert.Equal(t, "def", d.ListID, "default list id, not the alias")

	ids := func(path string) []string {
		var out []string
		for _, it := range decodeData[gTaskList](t, doJSON(t, e.r, http.MethodGet, path, e.token, nil)).Tasks {
			out = append(out, it.ID)
		}
		return out
	}
	assert.Equal(t, []string{a.ID}, ids("/tasks"+q))
	assert.Equal(t, []string{d.ID}, ids("/tasks"))

	// a task is invisible through the wrong list
	code, _ := e.do(t, http.MethodGet, "/tasks/"+a.ID, nil)
	assert.Equal(t, 404, code)
	code, _ = e.do(t, http.MethodGet, "/tasks/"+a.ID+q, nil)
	assert.Equal(t, 200, code)
	code, _ = e.do(t, http.MethodPut, "/tasks/"+a.ID, gin.H{"title": "dua", "list": l.ID})
	assert.Equal(t, 200, code)
	assert.Equal(t, "dua", e.ft.items[a.ID].Title)
	code, _ = e.do(t, http.MethodPut, "/tasks/"+a.ID, gin.H{"title": "tiga"})
	assert.Equal(t, 404, code)
	code, _ = e.do(t, http.MethodPost, "/tasks/"+a.ID+"/complete"+q, nil)
	assert.Equal(t, 200, code)
	assert.Equal(t, "completed", e.ft.items[a.ID].Status)
	code, _ = e.do(t, http.MethodPost, "/tasks/clear-completed"+q, nil)
	assert.Equal(t, 204, code)
	assert.Equal(t, l.ID, e.ft.cleared)
	sub := decodeData[googlecal.TaskItem](t, doJSON(t, e.r, http.MethodPost, "/tasks", e.token, gin.H{"title": "anak", "list": l.ID, "parent": a.ID}))
	code, _ = e.do(t, http.MethodPost, "/tasks", gin.H{"title": "anak", "parent": a.ID})
	assert.Equal(t, 400, code, "parent lives in another list")
	code, _ = e.do(t, http.MethodDelete, "/tasks/"+a.ID, nil)
	assert.Equal(t, 404, code)
	code, _ = e.do(t, http.MethodDelete, "/tasks/"+a.ID+q, nil)
	assert.Equal(t, 204, code)
	assert.NotContains(t, e.ft.items, sub.ID)

	for _, p := range []string{"/tasks?list=bad%20id", "/tasks/x?list=a/b", "/tasks?order=x"} {
		code, _ = e.do(t, http.MethodGet, p, nil)
		assert.Equal(t, 400, code, p)
	}
	code, _ = e.do(t, http.MethodGet, "/tasks?list=ghost", nil)
	assert.Equal(t, 404, code)
}

func TestGoogleTasksPositionOrder(t *testing.T) {
	e := newTasksEnv(t)
	today := time.Now().UTC().Format("2006-01-02")
	a := e.ft.put(googlecal.TaskItem{Title: "a", DueDate: ds(today)})
	b := e.ft.put(googlecal.TaskItem{Title: "b"})
	c := e.ft.put(googlecal.TaskItem{Title: "c", DueDate: ds("2020-01-01")})
	k2 := e.ft.put(googlecal.TaskItem{Title: "k2", Parent: &a})
	k1 := e.ft.put(googlecal.TaskItem{Title: "k1", Parent: &a})
	e.ft.items[k1].Position, e.ft.items[k2].Position = "5", "9"
	done := time.Now()
	dn := e.ft.put(googlecal.TaskItem{Title: "dn", Status: "completed", CompletedAt: &done})
	ids := func(path string) []string {
		var out []string
		for _, it := range decodeData[gTaskList](t, doJSON(t, e.r, http.MethodGet, path, e.token, nil)).Tasks {
			out = append(out, it.ID)
		}
		return out
	}
	assert.Equal(t, []string{a, k1, k2, b, c}, ids("/tasks?order=position"), "by position, not date")
	assert.Equal(t, []string{dn}, ids("/tasks?order=position&show=completed"))
	assert.Equal(t, []string{c, a, k1, k2, b}, ids("/tasks?order=date"))
}

func TestGoogleTasksMoveWithinList(t *testing.T) {
	e := newTasksEnv(t)
	a := e.ft.put(googlecal.TaskItem{Title: "a"})
	b := e.ft.put(googlecal.TaskItem{Title: "b"})
	c := e.ft.put(googlecal.TaskItem{Title: "c"})
	ka := e.ft.put(googlecal.TaskItem{Title: "ka", Parent: &a})
	kb := e.ft.put(googlecal.TaskItem{Title: "kb", Parent: &a})
	order := func() []string {
		var out []string
		for _, it := range decodeData[gTaskList](t, doJSON(t, e.r, http.MethodGet, "/tasks?order=position", e.token, nil)).Tasks {
			out = append(out, it.ID)
		}
		return out
	}
	move := func(id string, body gin.H) (int, string) { return e.do(t, http.MethodPost, "/tasks/"+id+"/move", body) }

	code, body := move(c, gin.H{"previous": nil, "parent": nil})
	require.Equal(t, 200, code, body)
	assert.Equal(t, []string{c, a, ka, kb, b}, order(), "top")
	code, _ = move(c, gin.H{"previous": a})
	assert.Equal(t, 200, code)
	assert.Equal(t, []string{a, ka, kb, c, b}, order(), "after a")
	code, _ = move(kb, gin.H{"parent": a})
	assert.Equal(t, 200, code)
	assert.Equal(t, []string{a, kb, ka, c, b}, order(), "top within parent")
	code, _ = move(ka, gin.H{"previous": kb, "parent": a})
	assert.Equal(t, 200, code)
	assert.Equal(t, []string{a, kb, ka, c, b}, order(), "after sibling within parent")

	for name, req := range map[string]gin.H{
		"previous other level": {"previous": ka},
		"previous wrong level": {"previous": b, "parent": a},
		"self previous":        {"previous": c},
		"self parent":          {"parent": c},
		"unknown previous":     {"previous": "ghost"},
		"unknown parent":       {"parent": "ghost"},
		"parent is subtask":    {"parent": ka},
		"bad id":               {"previous": "a b"},
	} {
		code, body = move(c, req)
		assert.Equal(t, 400, code, name)
		assert.Contains(t, body, "invalid_move", name)
	}
	code, _ = move("ghost", gin.H{})
	assert.Equal(t, 404, code)
	_ = b
}

func TestGoogleTasksMoveAcrossLists(t *testing.T) {
	e := newTasksEnv(t)
	l := decodeData[googlecal.TaskListItem](t, doJSON(t, e.r, http.MethodPost, "/tasklists", e.token, gin.H{"title": "B"}))
	a := e.ft.put(googlecal.TaskItem{Title: "a"})
	k := e.ft.put(googlecal.TaskItem{Title: "k", Parent: &a})
	x := e.ft.put(googlecal.TaskItem{Title: "x", ListID: l.ID})

	w := doJSON(t, e.r, http.MethodPost, "/tasks/"+a+"/move", e.token, gin.H{"destination_list": l.ID})
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, l.ID, decodeData[googlecal.TaskItem](t, w).ListID)
	assert.Equal(t, l.ID, e.ft.items[a].ListID)
	assert.Equal(t, l.ID, e.ft.items[k].ListID, "subtasks follow")
	assert.Equal(t, a, func() string {
		var first string
		for _, it := range decodeData[gTaskList](t, doJSON(t, e.r, http.MethodGet, "/tasks?order=position&list="+l.ID, e.token, nil)).Tasks {
			first = it.ID
			break
		}
		return first
	}(), "lands at the top")

	code, body := e.do(t, http.MethodPost, "/tasks/"+x+"/move?list="+l.ID, gin.H{"destination_list": "ghost"})
	assert.Equal(t, 404, code, body)
	code, _ = e.do(t, http.MethodPost, "/tasks/"+x+"/move?list="+l.ID, gin.H{"destination_list": "@default", "previous": a})
	assert.Equal(t, 400, code, "no placement across lists")
	code, _ = e.do(t, http.MethodPost, "/tasks/"+x+"/move?list="+l.ID, gin.H{"destination_list": "@default"})
	assert.Equal(t, 200, code)
	assert.Equal(t, "def", e.ft.items[x].ListID)
	code, _ = e.do(t, http.MethodPost, "/tasks/"+x+"/move", gin.H{"destination_list": "bad id"})
	assert.Equal(t, 400, code)
}

func TestGoogleTasksTodayAllLists(t *testing.T) {
	e := newTasksEnv(t)
	l := decodeData[googlecal.TaskListItem](t, doJSON(t, e.r, http.MethodPost, "/tasklists", e.token, gin.H{"title": "B"}))
	today := time.Now().Format("2006-01-02")
	e.ft.put(googlecal.TaskItem{Title: "d", DueDate: ds(today)})
	e.ft.put(googlecal.TaskItem{Title: "o", DueDate: ds(today), ListID: l.ID})
	e.ft.put(googlecal.TaskItem{Title: "none", ListID: l.ID})
	w := doJSON(t, e.r, http.MethodGet, "/today", e.token, nil)
	require.Equal(t, 200, w.Code, w.Body.String())
	d := decodeData[struct {
		Tasks []googlecal.TaskItem `json:"tasks"`
	}](t, w)
	got := map[string]string{}
	for _, it := range d.Tasks {
		got[it.Title] = it.ListID
	}
	assert.Equal(t, map[string]string{"d": "def", "o": l.ID}, got)
}

func TestGoogleTaskListsLocalMode(t *testing.T) {
	requireDB(t)
	_, token := newUser(t)
	ge := newGoogleEnv(t, true)
	th := NewTaskHandler(testDB)
	th.SetGoogle(googlecal.NewTasksService(ge.svc, newFakeTasks()))
	r := newProtectedRouter(func(g *gin.RouterGroup) { RegisterTaskRoutes(g, th) })
	assert.Equal(t, 409, doJSON(t, r, http.MethodGet, "/tasklists", token, nil).Code)
	assert.Equal(t, 409, doJSON(t, r, http.MethodPost, "/tasks/x/move", token, gin.H{}).Code)
}
