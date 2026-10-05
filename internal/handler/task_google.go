package handler

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
)

var googleTaskIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

// RegisterTaskRoutes is the single place task routes are wired; main.go and
// tests both use it.
func RegisterTaskRoutes(g *gin.RouterGroup, h *TaskHandler) {
	g.GET("/tasks", h.List)
	g.POST("/tasks", h.Create)
	g.POST("/tasks/clear-completed", h.ClearCompleted)
	g.POST("/tasks/import-local", h.ImportLocal)
	g.GET("/tasks/:id", h.Get)
	g.PUT("/tasks/:id", h.Update)
	g.DELETE("/tasks/:id", h.Delete)
	g.POST("/tasks/:id/complete", h.Complete)
	g.POST("/tasks/:id/move", h.Move)
	g.GET("/tasklists", h.Lists)
	g.POST("/tasklists", h.CreateList)
	g.PATCH("/tasklists/:id", h.RenameList)
	g.DELETE("/tasklists/:id", h.DeleteList)
	g.POST("/tasks/:id/subtasks", h.AddSubtask)
	g.PUT("/tasks/:id/subtasks/:sid", h.UpdateSubtask)
	g.DELETE("/tasks/:id/subtasks/:sid", h.DeleteSubtask)
}

// SetGoogle enables Google Tasks storage for users with a Google connection.
func (h *TaskHandler) SetGoogle(t *googlecal.TasksService) { h.google = t }

// useGoogle decides the storage for this request. stop=true means an error
// response was already written. Unconnected users keep the local table.
func (h *TaskHandler) useGoogle(c *gin.Context) (use, stop bool) {
	if h.google == nil {
		return false, false
	}
	err := h.google.Require(c, middleware.GetUserID(c))
	switch {
	case err == nil:
		return true, false
	case errors.Is(err, googlecal.ErrNotConnected):
		return false, false
	}
	googleError(c, err)
	return false, true
}

// tasksErrorCode is the machine-readable code for Tasks failures shown in /today.
func tasksErrorCode(err error) string {
	switch {
	case errors.Is(err, googlecal.ErrTasksScope):
		return "tasks_scope_missing"
	case errors.Is(err, googlecal.ErrTasksDisabled):
		return "tasks_api_disabled"
	case errors.Is(err, googlecal.ErrNeedsReauth):
		return "needs_reauth"
	}
	return "google_api_error"
}

func taskGoogleError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, googlecal.ErrTaskNotFound):
		response.NotFound(c)
	case errors.Is(err, googlecal.ErrInvalidDue), errors.Is(err, googlecal.ErrInvalidParent),
		errors.Is(err, googlecal.ErrInvalidMove), errors.Is(err, googlecal.ErrDefaultList):
		response.BadRequest(c, err.Error())
	case errors.Is(err, googlecal.ErrTasksScope):
		response.ErrorMsg(c, http.StatusConflict, "needs_reauth", "tasks_scope_missing")
	case errors.Is(err, googlecal.ErrTasksDisabled):
		response.Error(c, http.StatusBadGateway, "tasks_api_disabled")
	default:
		googleError(c, err)
	}
}

func validListID(id string) bool {
	return id == googlecal.DefaultList || googleTaskIDRe.MatchString(id)
}

// queryList reads ?list= (default @default).
func queryList(c *gin.Context) (string, bool) {
	l := c.DefaultQuery("list", googlecal.DefaultList)
	if !validListID(l) {
		response.BadRequest(c, "invalid list")
		return "", false
	}
	return l, true
}

func googleTaskID(c *gin.Context) (string, bool) {
	id := c.Param("id")
	if !googleTaskIDRe.MatchString(id) {
		response.BadRequest(c, "invalid id")
		return "", false
	}
	return id, true
}

type googleTaskRequest struct {
	Title   string  `json:"title"    binding:"required,max=500"`
	Notes   string  `json:"notes"    binding:"max=8000"`
	DueDate *string `json:"due_date"`
	Parent  *string `json:"parent"`
	List    string  `json:"list"`
}

func (r googleTaskRequest) input() (googlecal.TaskInput, string) {
	in := googlecal.TaskInput{Title: strings.TrimSpace(r.Title), Notes: r.Notes, DueDate: r.DueDate}
	if in.Title == "" {
		return in, "title is required"
	}
	if r.Parent != nil {
		if !googleTaskIDRe.MatchString(*r.Parent) {
			return in, googlecal.ErrInvalidParent.Error()
		}
		in.Parent = *r.Parent
	}
	return in, ""
}

// bindGoogleTask returns the input and the body's list (default @default).
func bindGoogleTask(c *gin.Context) (googlecal.TaskInput, string, bool) {
	var req googleTaskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return googlecal.TaskInput{}, "", false
	}
	if req.List == "" {
		req.List = googlecal.DefaultList
	}
	in, msg := req.input()
	if msg == "" && !validListID(req.List) {
		msg = "invalid list"
	}
	if msg != "" {
		response.BadRequest(c, msg)
		return in, "", false
	}
	return in, req.List, true
}

// gList: GET /tasks?show=&due=&tz= (Google mode).
func (h *TaskHandler) gList(c *gin.Context) {
	list, ok := queryList(c)
	if !ok {
		return
	}
	q := googlecal.TaskQuery{Show: c.DefaultQuery("show", "pending"), Due: c.DefaultQuery("due", "all"), Order: c.DefaultQuery("order", "date")}
	if q.Order != "date" && q.Order != "position" {
		response.BadRequest(c, "order must be date or position")
		return
	}
	if q.Show != "pending" && q.Show != "completed" && q.Show != "all" {
		response.BadRequest(c, "show must be pending, completed or all")
		return
	}
	switch q.Due {
	case "overdue", "today", "upcoming", "none", "all":
	default:
		response.BadRequest(c, "invalid due filter")
		return
	}
	loc := time.UTC
	if tz := c.Query("tz"); tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil || tz == "Local" {
			response.BadRequest(c, "invalid_timezone")
			return
		}
		loc = l
	}
	now := time.Now().In(loc)
	q.Today = now.Format("2006-01-02")
	userID := middleware.GetUserID(c)
	items, err := h.google.List(c, userID, list, q, now)
	if err != nil {
		taskGoogleError(c, err)
		return
	}
	local, err := h.google.LocalCount(c, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	// Ordering needs the whole list, so every page is fetched: no next page.
	response.OK(c, gin.H{"tasks": items, "next_page_token": "", "local_count": local})
}

func (h *TaskHandler) gGet(c *gin.Context) {
	id, ok := googleTaskID(c)
	if !ok {
		return
	}
	list, ok := queryList(c)
	if !ok {
		return
	}
	it, err := h.google.Get(c, middleware.GetUserID(c), list, id)
	if err != nil {
		taskGoogleError(c, err)
		return
	}
	response.OK(c, it)
}

func (h *TaskHandler) gCreate(c *gin.Context) {
	in, list, ok := bindGoogleTask(c)
	if !ok {
		return
	}
	it, err := h.google.Create(c, middleware.GetUserID(c), list, in)
	if err != nil {
		taskGoogleError(c, err)
		return
	}
	response.Created(c, it)
}

func (h *TaskHandler) gUpdate(c *gin.Context) {
	id, ok := googleTaskID(c)
	if !ok {
		return
	}
	in, list, ok := bindGoogleTask(c)
	if !ok {
		return
	}
	it, err := h.google.Update(c, middleware.GetUserID(c), list, id, in)
	if err != nil {
		taskGoogleError(c, err)
		return
	}
	response.OK(c, it)
}

func (h *TaskHandler) gDelete(c *gin.Context) {
	id, ok := googleTaskID(c)
	if !ok {
		return
	}
	list, ok := queryList(c)
	if !ok {
		return
	}
	if err := h.google.Delete(c, middleware.GetUserID(c), list, id); err != nil {
		taskGoogleError(c, err)
		return
	}
	response.NoContent(c)
}

func (h *TaskHandler) gComplete(c *gin.Context) {
	id, ok := googleTaskID(c)
	if !ok {
		return
	}
	list, ok := queryList(c)
	if !ok {
		return
	}
	it, err := h.google.Toggle(c, middleware.GetUserID(c), list, id)
	if err != nil {
		taskGoogleError(c, err)
		return
	}
	response.OK(c, it)
}

// requireGoogle guards the Google-only endpoints.
func (h *TaskHandler) requireGoogle(c *gin.Context) bool {
	if h.google == nil {
		response.Error(c, http.StatusServiceUnavailable, "google_not_configured")
		return false
	}
	use, stop := h.useGoogle(c)
	if stop {
		return false
	}
	if !use {
		response.Error(c, http.StatusConflict, "not_connected")
		return false
	}
	return true
}

// ClearCompleted: POST /tasks/clear-completed.
func (h *TaskHandler) ClearCompleted(c *gin.Context) {
	if !h.requireGoogle(c) {
		return
	}
	list, ok := queryList(c)
	if !ok {
		return
	}
	if err := h.google.ClearCompleted(c, middleware.GetUserID(c), list); err != nil {
		taskGoogleError(c, err)
		return
	}
	response.NoContent(c)
}

// ImportLocal: POST /tasks/import-local copies local pending tasks to Google (idempotent).
func (h *TaskHandler) ImportLocal(c *gin.Context) {
	if !h.requireGoogle(c) {
		return
	}
	st, err := h.google.ImportLocal(c, middleware.GetUserID(c))
	if err != nil {
		if errors.Is(err, googlecal.ErrTasksScope) || errors.Is(err, googlecal.ErrTasksDisabled) ||
			errors.Is(err, googlecal.ErrNeedsReauth) || errors.Is(err, googlecal.ErrAPI) {
			taskGoogleError(c, err)
		} else {
			respondDBError(c, err)
		}
		return
	}
	response.OK(c, st)
}

type taskMoveRequest struct {
	Previous        *string `json:"previous"`
	Parent          *string `json:"parent"`
	DestinationList string  `json:"destination_list"`
}

// Move: POST /tasks/:id/move?list= reorders within the list or moves the task
// to another list.
func (h *TaskHandler) Move(c *gin.Context) {
	if !h.requireGoogle(c) {
		return
	}
	id, ok := googleTaskID(c)
	if !ok {
		return
	}
	list, ok := queryList(c)
	if !ok {
		return
	}
	var req taskMoveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	for _, p := range []*string{req.Previous, req.Parent} {
		if p != nil && !googleTaskIDRe.MatchString(*p) {
			response.BadRequest(c, googlecal.ErrInvalidMove.Error())
			return
		}
	}
	if req.DestinationList != "" && !validListID(req.DestinationList) {
		response.BadRequest(c, "invalid list")
		return
	}
	it, err := h.google.Move(c, middleware.GetUserID(c), list, id,
		googlecal.MoveInput{Previous: req.Previous, Parent: req.Parent, Dest: req.DestinationList})
	if err != nil {
		taskGoogleError(c, err)
		return
	}
	response.OK(c, it)
}

type listRequest struct {
	Title string `json:"title" binding:"required,max=100"`
}

func bindList(c *gin.Context) (string, bool) {
	var req listRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return "", false
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		response.BadRequest(c, "title is required")
		return "", false
	}
	return title, true
}

func listParam(c *gin.Context) (string, bool) {
	id := c.Param("id")
	if !validListID(id) {
		response.BadRequest(c, "invalid id")
		return "", false
	}
	return id, true
}

// Lists: GET /tasklists.
func (h *TaskHandler) Lists(c *gin.Context) {
	if !h.requireGoogle(c) {
		return
	}
	ls, err := h.google.Lists(c, middleware.GetUserID(c))
	if err != nil {
		taskGoogleError(c, err)
		return
	}
	response.OK(c, ls)
}

// CreateList: POST /tasklists.
func (h *TaskHandler) CreateList(c *gin.Context) {
	if !h.requireGoogle(c) {
		return
	}
	title, ok := bindList(c)
	if !ok {
		return
	}
	l, err := h.google.CreateList(c, middleware.GetUserID(c), title)
	if err != nil {
		taskGoogleError(c, err)
		return
	}
	response.Created(c, l)
}

// RenameList: PATCH /tasklists/:id.
func (h *TaskHandler) RenameList(c *gin.Context) {
	if !h.requireGoogle(c) {
		return
	}
	id, ok := listParam(c)
	if !ok {
		return
	}
	title, ok := bindList(c)
	if !ok {
		return
	}
	l, err := h.google.RenameList(c, middleware.GetUserID(c), id, title)
	if err != nil {
		taskGoogleError(c, err)
		return
	}
	response.OK(c, l)
}

// DeleteList: DELETE /tasklists/:id (not the default list).
func (h *TaskHandler) DeleteList(c *gin.Context) {
	if !h.requireGoogle(c) {
		return
	}
	id, ok := listParam(c)
	if !ok {
		return
	}
	if err := h.google.DeleteList(c, middleware.GetUserID(c), id); err != nil {
		taskGoogleError(c, err)
		return
	}
	response.NoContent(c)
}
