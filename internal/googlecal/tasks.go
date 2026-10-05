package googlecal

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"google.golang.org/api/option"
	tasks "google.golang.org/api/tasks/v1"
)

const (
	tasksScope    = tasks.TasksScope
	DefaultList   = "@default"
	taskPageSize  = 100
	taskMaxPages  = 10 // 1000 tasks per request is plenty for a personal list
	CompletedDays = 30
)

var (
	ErrTaskNotFound  = errors.New("task_not_found")
	ErrTasksScope    = errors.New("tasks_scope_missing") // grant lacks the tasks scope: sign in again
	ErrTasksDisabled = errors.New("tasks_api_disabled")  // Tasks API off in the Cloud project
	ErrInvalidDue    = errors.New("invalid_due_date")
	ErrInvalidParent = errors.New("invalid_parent")
	ErrInvalidMove   = errors.New("invalid_move")
	ErrDefaultList   = errors.New("cannot_delete_default")
)

// TaskItem is the app-side view of a Google task. DueDate is a plain date.
type TaskItem struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Notes       string     `json:"notes"`
	DueDate     *string    `json:"due_date"`
	Status      string     `json:"status"` // pending | completed
	CompletedAt *time.Time `json:"completed_at"`
	Parent      *string    `json:"parent"`
	Position    string     `json:"position"`
	UpdatedAt   time.Time  `json:"updated_at"`
	WebViewLink *string    `json:"web_view_link"`
	ListID      string     `json:"list_id"`
}

type TaskInput struct {
	Title, Notes string
	DueDate      *string // "YYYY-MM-DD" or nil
	Parent       string  // create only
}

// TaskUpdate changes only what is set; ClearDue wins over Due.
type TaskUpdate struct {
	Title, Notes, Due, Status *string // Due: date; Status: needsAction | completed
	ClearDue                  bool
}

type TaskListOpts struct {
	WithCompleted bool      // also completed + hidden tasks
	CompletedMin  time.Time // only with WithCompleted
	PageToken     string
}

// TasksClient is the Google Tasks surface. Lists are real ids or "@default". Refresh tokens
// are passed per call. Get/Patch/Delete report a missing task as ErrTaskNotFound.
type TasksClient interface {
	List(ctx context.Context, rt, list string, o TaskListOpts) ([]TaskItem, string, error)
	Get(ctx context.Context, rt, list, id string) (TaskItem, error)
	Insert(ctx context.Context, rt, list string, in TaskInput) (TaskItem, error)
	Patch(ctx context.Context, rt, list, id string, u TaskUpdate) (TaskItem, error)
	Delete(ctx context.Context, rt, list, id string) error
	Clear(ctx context.Context, rt, list string) error
	// Move repositions a task (tasks.move); a non-empty m.Dest moves it, with its
	// subtasks, to the top of another list.
	Move(ctx context.Context, rt, list, id string, m MoveOpts) (TaskItem, error)

	Lists(ctx context.Context, rt string) ([]TaskListItem, error)
	ListGet(ctx context.Context, rt, id string) (TaskListItem, error) // id may be "@default"
	ListInsert(ctx context.Context, rt, title string) (TaskListItem, error)
	ListRename(ctx context.Context, rt, id, title string) (TaskListItem, error)
	ListDelete(ctx context.Context, rt, id string) error
}

// MoveOpts: empty Previous = first among siblings; empty Parent = top level.
type MoveOpts struct{ Previous, Parent, Dest string }

// TaskListItem is a Google task list.
type TaskListItem struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updated_at"`
	IsDefault bool      `json:"is_default"`
}

type realTasks struct{ d *realDrive } // reuses the refresh-token HTTP client

func NewTasks(clientID, secret string) TasksClient {
	return &realTasks{d: NewDrive(clientID, secret).(*realDrive)}
}

// tasksErr reuses the Drive status mapping (404, accessNotConfigured, scopes).
func tasksErr(err error) error {
	switch e := driveErr(err); {
	case errors.Is(e, ErrDriveNotFound):
		return ErrTaskNotFound
	case errors.Is(e, ErrDriveScope):
		return ErrTasksScope
	case errors.Is(e, ErrDriveDisabled):
		return ErrTasksDisabled
	default:
		return e
	}
}

func fromTask(t *tasks.Task) TaskItem {
	it := TaskItem{ID: t.Id, Title: t.Title, Notes: t.Notes, Status: "pending", Position: t.Position}
	if t.Status == "completed" {
		it.Status = "completed"
	}
	if len(t.Due) >= 10 {
		d := t.Due[:10] // date part as is: no time-zone shift
		it.DueDate = &d
	}
	if t.Completed != nil {
		if c, err := time.Parse(time.RFC3339, *t.Completed); err == nil {
			it.CompletedAt = &c
		}
	}
	if t.Parent != "" {
		p := t.Parent
		it.Parent = &p
	}
	if t.WebViewLink != "" {
		l := t.WebViewLink
		it.WebViewLink = &l
	}
	it.UpdatedAt, _ = time.Parse(time.RFC3339, t.Updated)
	return it
}

func dueStamp(d string) string { return d + "T00:00:00.000Z" }

func (c *realTasks) svc(ctx context.Context, rt string) (*tasks.Service, error) {
	return tasks.NewService(ctx, option.WithHTTPClient(c.d.client(ctx, rt)))
}

func (c *realTasks) List(ctx context.Context, rt, list string, o TaskListOpts) ([]TaskItem, string, error) {
	svc, err := c.svc(ctx, rt)
	if err != nil {
		return nil, "", tasksErr(err)
	}
	call := svc.Tasks.List(list).MaxResults(taskPageSize).ShowCompleted(o.WithCompleted).
		ShowHidden(o.WithCompleted).Context(ctx)
	if o.WithCompleted && !o.CompletedMin.IsZero() {
		call = call.CompletedMin(o.CompletedMin.UTC().Format(time.RFC3339))
	}
	if o.PageToken != "" {
		call = call.PageToken(o.PageToken)
	}
	res, err := call.Do()
	if err != nil {
		return nil, "", tasksErr(err)
	}
	var out []TaskItem
	for _, t := range res.Items {
		if !t.Deleted {
			out = append(out, fromTask(t))
		}
	}
	return out, res.NextPageToken, nil
}

func (c *realTasks) Get(ctx context.Context, rt, list, id string) (TaskItem, error) {
	svc, err := c.svc(ctx, rt)
	if err != nil {
		return TaskItem{}, tasksErr(err)
	}
	t, err := svc.Tasks.Get(list, id).Context(ctx).Do()
	if err != nil {
		return TaskItem{}, tasksErr(err)
	}
	if t.Deleted {
		return TaskItem{}, ErrTaskNotFound
	}
	return fromTask(t), nil
}

func (c *realTasks) Insert(ctx context.Context, rt, list string, in TaskInput) (TaskItem, error) {
	svc, err := c.svc(ctx, rt)
	if err != nil {
		return TaskItem{}, tasksErr(err)
	}
	g := &tasks.Task{Title: in.Title, Notes: in.Notes}
	if in.DueDate != nil {
		g.Due = dueStamp(*in.DueDate)
	}
	call := svc.Tasks.Insert(list, g).Context(ctx)
	if in.Parent != "" {
		call = call.Parent(in.Parent)
	}
	res, err := call.Do()
	if err != nil {
		return TaskItem{}, tasksErr(err)
	}
	return fromTask(res), nil
}

func (c *realTasks) Patch(ctx context.Context, rt, list, id string, u TaskUpdate) (TaskItem, error) {
	svc, err := c.svc(ctx, rt)
	if err != nil {
		return TaskItem{}, tasksErr(err)
	}
	g := &tasks.Task{}
	if u.Title != nil {
		g.Title = *u.Title
	}
	if u.Notes != nil {
		g.Notes = *u.Notes
		g.ForceSendFields = append(g.ForceSendFields, "Notes")
	}
	switch {
	case u.ClearDue:
		g.NullFields = append(g.NullFields, "Due")
	case u.Due != nil:
		g.Due = dueStamp(*u.Due)
	}
	if u.Status != nil {
		g.Status = *u.Status
		if g.Status == "needsAction" {
			g.NullFields = append(g.NullFields, "Completed")
		}
	}
	res, err := svc.Tasks.Patch(list, id, g).Context(ctx).Do()
	if err != nil {
		return TaskItem{}, tasksErr(err)
	}
	return fromTask(res), nil
}

func (c *realTasks) Delete(ctx context.Context, rt, list, id string) error {
	svc, err := c.svc(ctx, rt)
	if err != nil {
		return tasksErr(err)
	}
	return tasksErr(svc.Tasks.Delete(list, id).Context(ctx).Do())
}

func (c *realTasks) Move(ctx context.Context, rt, list, id string, m MoveOpts) (TaskItem, error) {
	svc, err := c.svc(ctx, rt)
	if err != nil {
		return TaskItem{}, tasksErr(err)
	}
	call := svc.Tasks.Move(list, id).Context(ctx)
	if m.Previous != "" {
		call = call.Previous(m.Previous)
	}
	if m.Parent != "" {
		call = call.Parent(m.Parent)
	}
	if m.Dest != "" {
		call = call.DestinationTasklist(m.Dest)
	}
	res, err := call.Do()
	if err != nil {
		return TaskItem{}, tasksErr(err)
	}
	return fromTask(res), nil
}

func fromList(l *tasks.TaskList) TaskListItem {
	u, _ := time.Parse(time.RFC3339, l.Updated)
	return TaskListItem{ID: l.Id, Title: l.Title, UpdatedAt: u}
}

func (c *realTasks) Lists(ctx context.Context, rt string) ([]TaskListItem, error) {
	svc, err := c.svc(ctx, rt)
	if err != nil {
		return nil, tasksErr(err)
	}
	out := []TaskListItem{}
	tok := ""
	for i := 0; i < taskMaxPages; i++ {
		call := svc.Tasklists.List().MaxResults(taskPageSize).Context(ctx)
		if tok != "" {
			call = call.PageToken(tok)
		}
		res, err := call.Do()
		if err != nil {
			return nil, tasksErr(err)
		}
		for _, l := range res.Items {
			out = append(out, fromList(l))
		}
		if tok = res.NextPageToken; tok == "" {
			break
		}
	}
	return out, nil
}

func (c *realTasks) ListGet(ctx context.Context, rt, id string) (TaskListItem, error) {
	svc, err := c.svc(ctx, rt)
	if err != nil {
		return TaskListItem{}, tasksErr(err)
	}
	l, err := svc.Tasklists.Get(id).Context(ctx).Do()
	if err != nil {
		return TaskListItem{}, tasksErr(err)
	}
	return fromList(l), nil
}

func (c *realTasks) ListInsert(ctx context.Context, rt, title string) (TaskListItem, error) {
	svc, err := c.svc(ctx, rt)
	if err != nil {
		return TaskListItem{}, tasksErr(err)
	}
	l, err := svc.Tasklists.Insert(&tasks.TaskList{Title: title}).Context(ctx).Do()
	if err != nil {
		return TaskListItem{}, tasksErr(err)
	}
	return fromList(l), nil
}

func (c *realTasks) ListRename(ctx context.Context, rt, id, title string) (TaskListItem, error) {
	svc, err := c.svc(ctx, rt)
	if err != nil {
		return TaskListItem{}, tasksErr(err)
	}
	l, err := svc.Tasklists.Patch(id, &tasks.TaskList{Title: title}).Context(ctx).Do()
	if err != nil {
		return TaskListItem{}, tasksErr(err)
	}
	return fromList(l), nil
}

func (c *realTasks) ListDelete(ctx context.Context, rt, id string) error {
	svc, err := c.svc(ctx, rt)
	if err != nil {
		return tasksErr(err)
	}
	return tasksErr(svc.Tasklists.Delete(id).Context(ctx).Do())
}

func (c *realTasks) Clear(ctx context.Context, rt, list string) error {
	svc, err := c.svc(ctx, rt)
	if err != nil {
		return tasksErr(err)
	}
	return tasksErr(svc.Tasks.Clear(list).Context(ctx).Do())
}

// ---- ordering --------------------------------------------------------------

// TaskQuery filters and orders a task list. Today is the user's local date.
type TaskQuery struct{ Show, Due, Today, Order string } // Order: date (default) | position

func dueMatch(due *string, today, mode string) bool {
	switch mode {
	case "overdue":
		return due != nil && *due < today
	case "today":
		return due != nil && *due == today
	case "upcoming":
		return due != nil && *due > today
	case "none":
		return due == nil
	}
	return true
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Arrange applies the due filter and orders: pending (overdue, today, upcoming,
// undated by position; subtasks right after their parent), then completed
// newest first.
func Arrange(items []TaskItem, q TaskQuery) []TaskItem {
	var pend, done []TaskItem
	for _, t := range items {
		switch {
		case !dueMatch(t.DueDate, q.Today, q.Due):
		case t.Status == "completed":
			done = append(done, t)
		default:
			pend = append(pend, t)
		}
	}
	out := []TaskItem{}
	if q.Show != "completed" {
		out = append(out, nest(pend, q.Today, q.Order == "position")...)
	}
	if q.Show != "pending" {
		sort.SliceStable(done, func(i, j int) bool {
			a, b := done[i].CompletedAt, done[j].CompletedAt
			return a != nil && (b == nil || a.After(*b))
		})
		out = append(out, done...)
	}
	return out
}

func rank(t TaskItem, today string) int {
	switch {
	case t.DueDate == nil:
		return 3
	case *t.DueDate < today:
		return 0
	case *t.DueDate == today:
		return 1
	}
	return 2
}

func nest(pend []TaskItem, today string, byPos bool) []TaskItem {
	ids := map[string]bool{}
	for _, t := range pend {
		ids[t.ID] = true
	}
	var tops []TaskItem
	kids := map[string][]TaskItem{}
	for _, t := range pend {
		if t.Parent != nil && ids[*t.Parent] {
			kids[*t.Parent] = append(kids[*t.Parent], t)
		} else {
			tops = append(tops, t) // also orphans whose parent is filtered out
		}
	}
	sort.SliceStable(tops, func(i, j int) bool {
		a, b := tops[i], tops[j]
		if byPos {
			return a.Position < b.Position
		}
		if ra, rb := rank(a, today), rank(b, today); ra != rb {
			return ra < rb
		}
		if da, db := strOr(a.DueDate), strOr(b.DueDate); da != db {
			return da < db
		}
		return a.Position < b.Position
	})
	out := make([]TaskItem, 0, len(pend))
	for _, t := range tops {
		out = append(out, t)
		k := kids[t.ID]
		sort.SliceStable(k, func(i, j int) bool { return k[i].Position < k[j].Position })
		out = append(out, k...)
	}
	return out
}

// ForToday keeps pending tasks due on or before today (oldest first, then by
// position) and tasks completed since dayStart.
func ForToday(items []TaskItem, today string, dayStart time.Time) []TaskItem {
	var pend, done []TaskItem
	for _, t := range items {
		switch {
		case t.Status == "completed":
			if t.CompletedAt != nil && !t.CompletedAt.Before(dayStart) {
				done = append(done, t)
			}
		case t.DueDate != nil && *t.DueDate <= today:
			pend = append(pend, t)
		}
	}
	sort.SliceStable(pend, func(i, j int) bool {
		if a, b := *pend[i].DueDate, *pend[j].DueDate; a != b {
			return a < b
		}
		return pend[i].Position < pend[j].Position
	})
	sort.SliceStable(done, func(i, j int) bool { return done[i].CompletedAt.After(*done[j].CompletedAt) })
	return append(pend, done...)
}

// ---- service ---------------------------------------------------------------

// TasksService stores tasks in the user's default Google Tasks list.
type TasksService struct {
	s *Service
	c TasksClient
}

func NewTasksService(s *Service, c TasksClient) *TasksService { return &TasksService{s: s, c: c} }

// Require reports ErrNotConnected / ErrNeedsReauth unless Tasks can be used.
func (t *TasksService) Require(ctx context.Context, userID uuid.UUID) error {
	return t.s.Require(ctx, userID)
}

func (t *TasksService) fail(ctx context.Context, userID uuid.UUID, err error) error {
	switch {
	case errors.Is(err, ErrTaskNotFound), errors.Is(err, ErrTasksScope), errors.Is(err, ErrTasksDisabled):
		return err
	}
	return t.s.classify(ctx, userID, err)
}

func validDue(d *string) error {
	if d != nil {
		if _, err := time.Parse("2006-01-02", *d); err != nil {
			return ErrInvalidDue
		}
	}
	return nil
}

// list resolves "@default" (or empty) to the real default list id.
func (t *TasksService) list(ctx context.Context, userID uuid.UUID, rt, list string) (string, error) {
	if list != "" && list != DefaultList {
		return list, nil
	}
	l, err := t.c.ListGet(ctx, rt, DefaultList)
	if err != nil {
		return "", t.fail(ctx, userID, err)
	}
	return l.ID, nil
}

func stamp(list string, it TaskItem) TaskItem {
	it.ListID = list
	return it
}

// all pages of the list, capped.
func (t *TasksService) fetch(ctx context.Context, userID uuid.UUID, rt, list string, o TaskListOpts) ([]TaskItem, error) {
	var out []TaskItem
	for i := 0; i < taskMaxPages; i++ {
		items, next, err := t.c.List(ctx, rt, list, o)
		if err != nil {
			return nil, t.fail(ctx, userID, err)
		}
		for _, it := range items {
			out = append(out, stamp(list, it))
		}
		if next == "" {
			break
		}
		o.PageToken = next
	}
	return out, nil
}

func (t *TasksService) List(ctx context.Context, userID uuid.UUID, list string, q TaskQuery, now time.Time) ([]TaskItem, error) {
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return nil, err
	}
	if list, err = t.list(ctx, userID, rt, list); err != nil {
		return nil, err
	}
	o := TaskListOpts{}
	if q.Show != "pending" {
		o = TaskListOpts{WithCompleted: true, CompletedMin: now.AddDate(0, 0, -CompletedDays)}
	}
	items, err := t.fetch(ctx, userID, rt, list, o)
	if err != nil {
		return nil, err
	}
	return Arrange(items, q), nil
}

// Today gathers what the Today view needs from every list; dayStart is the
// start of the local day.
func (t *TasksService) Today(ctx context.Context, userID uuid.UUID, today string, dayStart time.Time) ([]TaskItem, error) {
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return nil, err
	}
	lists, err := t.c.Lists(ctx, rt)
	if err != nil {
		return nil, t.fail(ctx, userID, err)
	}
	var items []TaskItem
	for _, l := range lists {
		got, err := t.fetch(ctx, userID, rt, l.ID, TaskListOpts{WithCompleted: true, CompletedMin: dayStart})
		if err != nil {
			return nil, err
		}
		items = append(items, got...)
	}
	return ForToday(items, today, dayStart), nil
}

func (t *TasksService) Get(ctx context.Context, userID uuid.UUID, list, id string) (TaskItem, error) {
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return TaskItem{}, err
	}
	if list, err = t.list(ctx, userID, rt, list); err != nil {
		return TaskItem{}, err
	}
	it, err := t.c.Get(ctx, rt, list, id)
	if err != nil {
		return TaskItem{}, t.fail(ctx, userID, err)
	}
	return stamp(list, it), nil
}

func (t *TasksService) Create(ctx context.Context, userID uuid.UUID, list string, in TaskInput) (TaskItem, error) {
	if err := validDue(in.DueDate); err != nil {
		return TaskItem{}, err
	}
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return TaskItem{}, err
	}
	if list, err = t.list(ctx, userID, rt, list); err != nil {
		return TaskItem{}, err
	}
	if in.Parent != "" {
		p, err := t.c.Get(ctx, rt, list, in.Parent)
		switch {
		case errors.Is(err, ErrTaskNotFound):
			return TaskItem{}, ErrInvalidParent
		case err != nil:
			return TaskItem{}, t.fail(ctx, userID, err)
		case p.Parent != nil: // Google allows one level only
			return TaskItem{}, ErrInvalidParent
		}
	}
	it, err := t.c.Insert(ctx, rt, list, in)
	if err != nil {
		return TaskItem{}, t.fail(ctx, userID, err)
	}
	return stamp(list, it), nil
}

// Update replaces title, notes and due date (nil clears it).
func (t *TasksService) Update(ctx context.Context, userID uuid.UUID, list, id string, in TaskInput) (TaskItem, error) {
	if err := validDue(in.DueDate); err != nil {
		return TaskItem{}, err
	}
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return TaskItem{}, err
	}
	if list, err = t.list(ctx, userID, rt, list); err != nil {
		return TaskItem{}, err
	}
	it, err := t.c.Patch(ctx, rt, list, id, TaskUpdate{Title: &in.Title, Notes: &in.Notes, Due: in.DueDate, ClearDue: in.DueDate == nil})
	if err != nil {
		return TaskItem{}, t.fail(ctx, userID, err)
	}
	return stamp(list, it), nil
}

// Toggle flips pending <-> completed.
func (t *TasksService) Toggle(ctx context.Context, userID uuid.UUID, list, id string) (TaskItem, error) {
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return TaskItem{}, err
	}
	if list, err = t.list(ctx, userID, rt, list); err != nil {
		return TaskItem{}, err
	}
	cur, err := t.c.Get(ctx, rt, list, id)
	if err != nil {
		return TaskItem{}, t.fail(ctx, userID, err)
	}
	st := "completed"
	if cur.Status == "completed" {
		st = "needsAction"
	}
	it, err := t.c.Patch(ctx, rt, list, id, TaskUpdate{Status: &st})
	if err != nil {
		return TaskItem{}, t.fail(ctx, userID, err)
	}
	return stamp(list, it), nil
}

func (t *TasksService) Delete(ctx context.Context, userID uuid.UUID, list, id string) error {
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return err
	}
	if list, err = t.list(ctx, userID, rt, list); err != nil {
		return err
	}
	if err := t.c.Delete(ctx, rt, list, id); err != nil {
		return t.fail(ctx, userID, err)
	}
	return nil
}

func (t *TasksService) ClearCompleted(ctx context.Context, userID uuid.UUID, list string) error {
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return err
	}
	if list, err = t.list(ctx, userID, rt, list); err != nil {
		return err
	}
	if err := t.c.Clear(ctx, rt, list); err != nil {
		return t.fail(ctx, userID, err)
	}
	return nil
}

// MoveInput: Previous/Parent nil = first / top level. Dest (another list) moves
// the task to the top of that list and takes no Previous/Parent.
type MoveInput struct {
	Previous, Parent *string
	Dest             string
}

// Move reorders a task inside its list (tasks.move) or moves it, with its
// subtasks, to another list (tasks.move with destinationTasklist; the library
// supports it, so no copy+delete).
func (t *TasksService) Move(ctx context.Context, userID uuid.UUID, list, id string, in MoveInput) (TaskItem, error) {
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return TaskItem{}, err
	}
	if list, err = t.list(ctx, userID, rt, list); err != nil {
		return TaskItem{}, err
	}
	if _, err := t.c.Get(ctx, rt, list, id); err != nil {
		return TaskItem{}, t.fail(ctx, userID, err)
	}
	m := MoveOpts{Previous: strOr(in.Previous), Parent: strOr(in.Parent)}
	if in.Dest != "" {
		if in.Dest, err = t.list(ctx, userID, rt, in.Dest); err != nil {
			return TaskItem{}, err
		}
	}
	if in.Dest != "" && in.Dest != list {
		if in.Previous != nil || in.Parent != nil {
			return TaskItem{}, ErrInvalidMove
		}
		if _, err := t.c.ListGet(ctx, rt, in.Dest); err != nil {
			return TaskItem{}, t.fail(ctx, userID, err)
		}
		m.Dest = in.Dest
		it, err := t.c.Move(ctx, rt, list, id, m)
		if err != nil {
			return TaskItem{}, t.fail(ctx, userID, err)
		}
		return stamp(in.Dest, it), nil
	}
	if in.Parent != nil {
		p, err := t.c.Get(ctx, rt, list, *in.Parent)
		switch {
		case errors.Is(err, ErrTaskNotFound):
			return TaskItem{}, ErrInvalidMove
		case err != nil:
			return TaskItem{}, t.fail(ctx, userID, err)
		case *in.Parent == id, p.Parent != nil:
			return TaskItem{}, ErrInvalidMove
		}
	}
	if in.Previous != nil {
		p, err := t.c.Get(ctx, rt, list, *in.Previous)
		switch {
		case errors.Is(err, ErrTaskNotFound):
			return TaskItem{}, ErrInvalidMove
		case err != nil:
			return TaskItem{}, t.fail(ctx, userID, err)
		case *in.Previous == id, strOr(p.Parent) != m.Parent:
			return TaskItem{}, ErrInvalidMove
		}
	}
	it, err := t.c.Move(ctx, rt, list, id, m)
	if err != nil {
		return TaskItem{}, t.fail(ctx, userID, err)
	}
	return stamp(list, it), nil
}

// ---- lists -----------------------------------------------------------------

func (t *TasksService) Lists(ctx context.Context, userID uuid.UUID) ([]TaskListItem, error) {
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return nil, err
	}
	def, err := t.list(ctx, userID, rt, DefaultList)
	if err != nil {
		return nil, err
	}
	ls, err := t.c.Lists(ctx, rt)
	if err != nil {
		return nil, t.fail(ctx, userID, err)
	}
	for i := range ls {
		ls[i].IsDefault = ls[i].ID == def
	}
	return ls, nil
}

func (t *TasksService) CreateList(ctx context.Context, userID uuid.UUID, title string) (TaskListItem, error) {
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return TaskListItem{}, err
	}
	l, err := t.c.ListInsert(ctx, rt, title)
	if err != nil {
		return TaskListItem{}, t.fail(ctx, userID, err)
	}
	return l, nil
}

func (t *TasksService) RenameList(ctx context.Context, userID uuid.UUID, id, title string) (TaskListItem, error) {
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return TaskListItem{}, err
	}
	def, err := t.list(ctx, userID, rt, DefaultList)
	if err != nil {
		return TaskListItem{}, err
	}
	if id, err = t.list(ctx, userID, rt, id); err != nil {
		return TaskListItem{}, err
	}
	l, err := t.c.ListRename(ctx, rt, id, title)
	if err != nil {
		return TaskListItem{}, t.fail(ctx, userID, err)
	}
	l.IsDefault = l.ID == def
	return l, nil
}

// DeleteList removes a list and its tasks; the default list is protected.
func (t *TasksService) DeleteList(ctx context.Context, userID uuid.UUID, id string) error {
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return err
	}
	def, err := t.list(ctx, userID, rt, DefaultList)
	if err != nil {
		return err
	}
	if id == DefaultList || id == def {
		return ErrDefaultList
	}
	if err := t.c.ListDelete(ctx, rt, id); err != nil {
		return t.fail(ctx, userID, err)
	}
	return nil
}

// LocalCount is the number of local pending tasks not yet imported.
func (t *TasksService) LocalCount(ctx context.Context, userID uuid.UUID) (n int, err error) {
	err = t.s.db.QueryRow(ctx, `SELECT COUNT(*) FROM tasks
		WHERE user_id = $1 AND status = 'pending' AND google_task_id IS NULL`, userID).Scan(&n)
	return
}

// ImportLocal copies local pending tasks to Google once; google_task_id marks
// the imported ones, so repeated calls skip them. Local rows stay.
func (t *TasksService) ImportLocal(ctx context.Context, userID uuid.UUID) (ImportStats, error) {
	var st ImportStats
	rt, err := t.s.rt(ctx, userID)
	if err != nil {
		return st, err
	}
	rows, err := t.s.db.Query(ctx, `SELECT id, title, COALESCE(description, ''), due_date::text,
		google_task_id IS NOT NULL FROM tasks WHERE user_id = $1 AND status = 'pending' ORDER BY created_at`, userID)
	if err != nil {
		return st, err
	}
	type local struct {
		id       uuid.UUID
		in       TaskInput
		imported bool
	}
	var all []local
	for rows.Next() {
		var l local
		if err := rows.Scan(&l.id, &l.in.Title, &l.in.Notes, &l.in.DueDate, &l.imported); err != nil {
			rows.Close()
			return st, err
		}
		all = append(all, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}
	list, err := t.list(ctx, userID, rt, DefaultList)
	if err != nil {
		return st, err
	}
	for _, l := range all {
		if l.imported {
			st.Skipped++
			continue
		}
		it, err := t.c.Insert(ctx, rt, list, l.in)
		if err != nil {
			return st, t.fail(ctx, userID, err)
		}
		if _, err := t.s.db.Exec(ctx, `UPDATE tasks SET google_task_id = $1 WHERE id = $2`, it.ID, l.id); err != nil {
			return st, err
		}
		st.Imported++
	}
	return st, nil
}
