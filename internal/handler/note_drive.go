package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
)

var driveIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)

// RegisterNoteRoutes is the single place note routes are wired; main.go and
// the wiring test both use it.
func RegisterNoteRoutes(g *gin.RouterGroup, h *NoteHandler) {
	g.GET("/notes", h.List)
	g.POST("/notes", h.Create)
	g.POST("/notes/import-local", h.ImportLocal)
	g.GET("/notes/:id", h.Get)
	g.PUT("/notes/:id", h.Update)
	g.PATCH("/notes/:id", h.Patch)
	g.DELETE("/notes/:id", h.Delete)
}

// SetDrive enables Drive storage for users with a Google connection.
func (h *NoteHandler) SetDrive(n *googlecal.NotesService) { h.drive = n }

// useDrive decides the storage for this request. stop=true means an error
// response was already written. Users without a Google connection (or with
// Google off) keep the local table.
func (h *NoteHandler) useDrive(c *gin.Context) (use, stop bool) {
	if h.drive == nil {
		return false, false
	}
	err := h.drive.Require(c, middleware.GetUserID(c))
	switch {
	case err == nil:
		return true, false
	case errors.Is(err, googlecal.ErrNotConnected):
		return false, false
	}
	googleError(c, err)
	return false, true
}

// noteDriveError maps notes/Drive errors to the contract's status + code pairs.
func noteDriveError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, googlecal.ErrNoteNotFound):
		response.NotFound(c)
	case errors.Is(err, googlecal.ErrInvalidColor), errors.Is(err, googlecal.ErrInvalidLabel):
		response.BadRequest(c, err.Error())
	case errors.Is(err, googlecal.ErrDriveScope):
		response.ErrorMsg(c, http.StatusConflict, "needs_reauth", "drive_scope_missing")
	case errors.Is(err, googlecal.ErrDriveDisabled):
		response.Error(c, http.StatusBadGateway, "drive_api_disabled")
	default:
		googleError(c, err)
	}
}

func driveID(c *gin.Context) (string, bool) {
	id := c.Param("id")
	if !driveIDRe.MatchString(id) {
		response.BadRequest(c, "invalid id")
		return "", false
	}
	return id, true
}

type driveNoteRequest struct {
	Title    string   `json:"title"    binding:"required,max=200"`
	Body     string   `json:"body"     binding:"max=20000"`
	Pinned   bool     `json:"pinned"`
	Archived bool     `json:"archived"`
	Color    *string  `json:"color"`
	Labels   []string `json:"labels"`
}

func (r driveNoteRequest) input() googlecal.NoteInput {
	return googlecal.NoteInput{Title: r.Title, Body: r.Body, Pinned: r.Pinned, Archived: r.Archived, Color: r.Color, Labels: r.Labels}
}

func (h *NoteHandler) driveList(c *gin.Context) {
	userID := middleware.GetUserID(c)
	limit, _ := strconv.Atoi(c.Query("limit"))
	notes, next, err := h.drive.List(c, userID, googlecal.NoteListOpts{
		Q: c.Query("q"), Label: c.Query("label"), PageToken: c.Query("page_token"), Limit: limit,
		Archived: c.Query("archived") == "true", Trashed: c.Query("trashed") == "true",
	})
	if err != nil {
		noteDriveError(c, err)
		return
	}
	var local int
	if err := h.db.QueryRow(c, `SELECT COUNT(*) FROM notes WHERE user_id = $1`, userID).Scan(&local); err != nil {
		respondDBError(c, err)
		return
	}
	response.OK(c, gin.H{"notes": notes, "next_page_token": next, "local_count": local})
}

func (h *NoteHandler) driveGet(c *gin.Context) {
	id, ok := driveID(c)
	if !ok {
		return
	}
	n, err := h.drive.Get(c, middleware.GetUserID(c), id)
	if err != nil {
		noteDriveError(c, err)
		return
	}
	response.OK(c, n)
}

func (h *NoteHandler) driveCreate(c *gin.Context) {
	var req driveNoteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	n, err := h.drive.Create(c, middleware.GetUserID(c), req.input())
	if err != nil {
		noteDriveError(c, err)
		return
	}
	response.Created(c, n)
}

func (h *NoteHandler) driveUpdate(c *gin.Context) {
	id, ok := driveID(c)
	if !ok {
		return
	}
	var req driveNoteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	n, err := h.drive.Update(c, middleware.GetUserID(c), id, req.input())
	if err != nil {
		noteDriveError(c, err)
		return
	}
	response.OK(c, n)
}

func (h *NoteHandler) driveDelete(c *gin.Context) {
	id, ok := driveID(c)
	if !ok {
		return
	}
	if err := h.drive.Delete(c, middleware.GetUserID(c), id, c.Query("permanent") == "true"); err != nil {
		noteDriveError(c, err)
		return
	}
	response.NoContent(c)
}

// Patch: PATCH /notes/:id changes only the provided metadata fields.
func (h *NoteHandler) Patch(c *gin.Context) {
	use, stop := h.useDrive(c)
	if stop {
		return
	}
	if !use {
		response.Error(c, http.StatusConflict, "not_connected")
		return
	}
	id, ok := driveID(c)
	if !ok {
		return
	}
	var req struct {
		Pinned   *bool           `json:"pinned"`
		Archived *bool           `json:"archived"`
		Trashed  *bool           `json:"trashed"`
		Color    json.RawMessage `json:"color"`
		Labels   *[]string       `json:"labels"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	p := googlecal.NotePatch{Pinned: req.Pinned, Archived: req.Archived, Trashed: req.Trashed, Labels: req.Labels}
	if req.Color != nil {
		p.SetColor = true
		if err := json.Unmarshal(req.Color, &p.Color); err != nil {
			response.BadRequest(c, "invalid_color")
			return
		}
	}
	n, err := h.drive.Patch(c, middleware.GetUserID(c), id, p)
	if err != nil {
		noteDriveError(c, err)
		return
	}
	response.OK(c, n)
}

// ImportLocal: POST /notes/import-local copies local notes to Drive (idempotent).
func (h *NoteHandler) ImportLocal(c *gin.Context) {
	if h.drive == nil {
		response.Error(c, http.StatusServiceUnavailable, "google_not_configured")
		return
	}
	use, stop := h.useDrive(c)
	if stop {
		return
	}
	if !use {
		response.Error(c, http.StatusConflict, "not_connected")
		return
	}
	userID := middleware.GetUserID(c)
	rows, err := h.db.Query(c, `SELECT id::text, title, COALESCE(body, '') FROM notes WHERE user_id = $1 ORDER BY created_at`, userID)
	if err != nil {
		respondDBError(c, err)
		return
	}
	defer rows.Close()
	var local []googlecal.LocalNote
	for rows.Next() {
		var ln googlecal.LocalNote
		if err := rows.Scan(&ln.ID, &ln.Title, &ln.Body); err != nil {
			respondDBError(c, err)
			return
		}
		local = append(local, ln)
	}
	if err := rows.Err(); err != nil {
		respondDBError(c, err)
		return
	}
	rows.Close()
	st, err := h.drive.ImportLocal(c, userID, local)
	if err != nil {
		noteDriveError(c, err)
		return
	}
	response.OK(c, st)
}
