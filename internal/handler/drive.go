package handler

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
)

const (
	maxDownload = 200 << 20
	maxQuery    = 100
	gNative     = "application/vnd.google-apps."
	folderMimeG = gNative + "folder"
	shortcutG   = gNative + "shortcut"
)

var (
	fileIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{10,100}$`)

	// inlineOK lists the only types that may render in the browser.
	inlineOK = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
		"image/avif": true, "application/pdf": true, "text/plain": true, "text/markdown": true,
		"text/csv": true, "audio/mpeg": true, "video/mp4": true}

	// exports maps a Google-native type to its allowed formats (first = default).
	exports = map[string][]string{
		gNative + "document":     {"pdf", "docx", "txt"},
		gNative + "presentation": {"pdf", "pptx", "txt"},
		gNative + "spreadsheet":  {"pdf", "xlsx"},
		gNative + "drawing":      {"png"},
	}
	exportMime = map[string]string{
		"pdf": "application/pdf", "png": "image/png", "txt": "text/plain",
		"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	}
)

// DriveHandler is the Google Drive browser: browse, upload, rename, move,
// star, trash and (from the trash only) delete.
type DriveHandler struct{ svc *googlecal.FilesService }

// NewDriveHandler: a nil service means Google is off (routes answer 503).
func NewDriveHandler(svc *googlecal.FilesService) *DriveHandler { return &DriveHandler{svc: svc} }

// RegisterDriveRoutes is the single place Drive routes are wired; main.go and
// the tests use it. The write routes below are the complete list: there is no
// emptyTrash, copy or share route, and DELETE only reaches trashed files.
func RegisterDriveRoutes(g *gin.RouterGroup, h *DriveHandler) {
	g.GET("/drive/about", h.About)
	g.GET("/drive/files", h.List)
	g.POST("/drive/files/upload", h.Upload)
	g.GET("/drive/files/:id", h.Get)
	g.PATCH("/drive/files/:id", h.Update)
	g.DELETE("/drive/files/:id", h.Delete)
	g.GET("/drive/files/:id/path", h.Path)
	g.GET("/drive/files/:id/thumbnail", h.Thumbnail)
	g.GET("/drive/files/:id/content", h.Content)
	g.POST("/drive/files/:id/trash", h.Trash)
	g.POST("/drive/files/:id/restore", h.Restore)
	g.POST("/drive/folders", h.CreateFolder)
}

func driveFail(c *gin.Context, err error) {
	switch {
	case errors.Is(err, googlecal.ErrDriveNotFound):
		response.NotFound(c)
	case errors.Is(err, googlecal.ErrDriveScope):
		response.ErrorMsg(c, http.StatusConflict, "needs_reauth", "drive_scope_missing")
	case errors.Is(err, googlecal.ErrDriveDisabled):
		response.Error(c, http.StatusBadGateway, "drive_api_disabled")
	case errors.Is(err, googlecal.ErrDriveForbidden):
		response.Forbidden(c)
	case errors.Is(err, googlecal.ErrDriveQuota):
		response.Error(c, http.StatusConflict, "quota_exceeded")
	case errors.Is(err, googlecal.ErrNotFolder):
		response.Error(c, http.StatusBadRequest, "not_a_folder")
	case errors.Is(err, googlecal.ErrInvalidMove):
		response.Error(c, http.StatusBadRequest, "invalid_move")
	case errors.Is(err, googlecal.ErrNotInTrash):
		response.Error(c, http.StatusConflict, "not_in_trash")
	case errors.Is(err, googlecal.ErrTooLarge):
		response.Error(c, http.StatusRequestEntityTooLarge, "too_large")
	case errors.Is(err, googlecal.ErrBadUpload):
		response.Error(c, http.StatusBadRequest, "invalid_upload")
	case errors.Is(err, context.Canceled):
		c.AbortWithStatus(499) // the client went away: nobody is left to answer
	default:
		googleError(c, err)
	}
}

// ready answers 503 when Google is off.
func (h *DriveHandler) ready(c *gin.Context) bool {
	if h.svc == nil {
		response.Error(c, http.StatusServiceUnavailable, "google_not_configured")
		return false
	}
	return true
}

func validFileID(c *gin.Context, id string) bool {
	if id == "root" || fileIDRe.MatchString(id) {
		return true
	}
	response.Error(c, http.StatusBadRequest, "invalid_id")
	return false
}

// pathID reads and validates :id; ok=false means a response was written.
func (h *DriveHandler) pathID(c *gin.Context) (string, bool) {
	if !h.ready(c) {
		return "", false
	}
	id := c.Param("id")
	return id, validFileID(c, id)
}

// DriveCaps is what the user may do with a file. can_star holds for any live
// (not trashed) file the user can see.
type DriveCaps struct {
	CanRename      bool `json:"can_rename"`
	CanTrash       bool `json:"can_trash"`
	CanDelete      bool `json:"can_delete"`
	CanMove        bool `json:"can_move"`
	CanAddChildren bool `json:"can_add_children"`
	CanStar        bool `json:"can_star"`
}

// DriveFile is the JSON item of the browser.
type DriveFile struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	MimeType       string    `json:"mime_type"`
	IsFolder       bool      `json:"is_folder"`
	IsGoogleDoc    bool      `json:"is_google_doc"`
	IsShortcut     bool      `json:"is_shortcut,omitempty"`
	ShortcutTarget string    `json:"shortcut_target_id,omitempty"`
	ShortcutMime   string    `json:"shortcut_target_mime,omitempty"`
	Size           *int64    `json:"size"`
	ModifiedTime   string    `json:"modified_time"`
	CreatedTime    string    `json:"created_time"`
	Starred        bool      `json:"starred"`
	OwnedByMe      bool      `json:"owned_by_me"`
	Shared         bool      `json:"shared"`
	Trashed        bool      `json:"trashed"`
	Capabilities   DriveCaps `json:"capabilities"`
	OwnerName      *string   `json:"owner_name"`
	HasThumbnail   bool      `json:"has_thumbnail"`
	WebViewLink    string    `json:"web_view_link"`
	IconLink       *string   `json:"icon_link"`
	Description    *string   `json:"description,omitempty"`
	Parents        *[]string `json:"parents,omitempty"`
}

func toDriveFile(f googlecal.FileInfo, detail bool) DriveFile {
	folder, shortcut := f.MimeType == folderMimeG, f.MimeType == shortcutG
	out := DriveFile{ID: f.ID, Name: f.Name, MimeType: f.MimeType, IsFolder: folder,
		IsGoogleDoc: strings.HasPrefix(f.MimeType, gNative) && !folder && !shortcut,
		IsShortcut:  shortcut, ShortcutTarget: f.ShortcutTargetID, ShortcutMime: f.ShortcutMime,
		Size: f.Size, Starred: f.Starred, OwnedByMe: f.OwnedByMe, Shared: f.Shared, OwnerName: f.OwnerName,
		HasThumbnail: f.ThumbnailLink != "", WebViewLink: f.WebViewLink, Trashed: f.Trashed,
		Capabilities: DriveCaps{f.Caps.CanRename, f.Caps.CanTrash, f.Caps.CanDelete, f.Caps.CanMove,
			f.Caps.CanAddChildren, !f.Trashed}}
	if !f.Modified.IsZero() {
		out.ModifiedTime = f.Modified.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if !f.Created.IsZero() {
		out.CreatedTime = f.Created.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if f.IconLink != "" {
		out.IconLink = &f.IconLink
	}
	if detail {
		out.Description = &f.Description
		p := f.Parents
		if p == nil {
			p = []string{}
		}
		out.Parents = &p
	}
	return out
}

// buildListQuery turns validated request parameters into a Drive q string.
// Every view but "trash" excludes trashed items; "trash" lists only the user's own.
func buildListQuery(view, parent, q string, foldersOnly bool) string {
	var cond []string
	if q != "" {
		e := googlecal.Query(q)
		cond = append(cond, "(name contains "+e+" or fullText contains "+e+")")
	}
	switch view {
	case "mydrive":
		if q == "" {
			cond = append(cond, googlecal.Query(parent)+" in parents")
		}
	case "starred":
		cond = append(cond, "starred=true")
	case "shared":
		cond = append(cond, "sharedWithMe=true")
	}
	if foldersOnly {
		cond = append(cond, "mimeType='"+folderMimeG+"'")
	}
	if view == "trash" {
		return strings.Join(append(cond, "trashed=true", "'me' in owners"), " and ")
	}
	return strings.Join(append(cond, "trashed=false"), " and ")
}

func (h *DriveHandler) List(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	view := c.DefaultQuery("view", "mydrive")
	parent := c.DefaultQuery("parent", "root")
	q := strings.TrimSpace(c.Query("q"))
	order := c.DefaultQuery("order", "name")
	dir, folders := c.Query("dir"), c.Query("folders_only")
	keys := map[string]string{"name": "name", "modified": "modifiedTime", "size": "quotaBytesUsed"}
	switch {
	case view != "mydrive" && view != "recent" && view != "starred" && view != "shared" && view != "trash",
		keys[order] == "", dir != "" && dir != "asc" && dir != "desc", folders != "" && folders != "true" && folders != "false",
		utf8.RuneCountInString(q) > maxQuery, len(c.Query("page_token")) > 2048:
		response.BadRequest(c, "invalid query")
		return
	}
	if !validFileID(c, parent) {
		return
	}
	if dir == "" {
		dir = "asc"
		if order == "modified" {
			dir = "desc"
		}
	}
	orderBy := "folder," + keys[order]
	if dir == "desc" {
		orderBy += " desc"
	}
	switch view {
	case "recent":
		orderBy = "viewedByMeTime desc"
	case "trash":
		orderBy = "modifiedTime desc" // Drive has no trashed-time sort key
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 100)
	res, err := h.svc.List(c.Request.Context(), middleware.GetUserID(c), googlecal.FilesListOpts{
		Query: buildListQuery(view, parent, q, folders == "true"), OrderBy: orderBy, PageToken: c.Query("page_token"), PageSize: limit})
	if err != nil {
		driveFail(c, err)
		return
	}
	files := make([]DriveFile, 0, len(res.Files))
	for _, f := range res.Files {
		files = append(files, toDriveFile(f, false))
	}
	response.OK(c, gin.H{"files": files, "next_page_token": res.NextPageToken})
}

func (h *DriveHandler) Get(c *gin.Context) {
	id, ok := h.pathID(c)
	if !ok {
		return
	}
	f, err := h.svc.Get(c.Request.Context(), middleware.GetUserID(c), id)
	if err != nil {
		driveFail(c, err)
		return
	}
	response.OK(c, toDriveFile(f, true))
}

func (h *DriveHandler) Path(c *gin.Context) {
	id, ok := h.pathID(c)
	if !ok {
		return
	}
	p, err := h.svc.Path(c.Request.Context(), middleware.GetUserID(c), id)
	if err != nil {
		driveFail(c, err)
		return
	}
	response.OK(c, p)
}

func (h *DriveHandler) Thumbnail(c *gin.Context) {
	id, ok := h.pathID(c)
	if !ok {
		return
	}
	s, err := h.svc.Thumbnail(c.Request.Context(), middleware.GetUserID(c), id)
	if err != nil {
		driveFail(c, err)
		return
	}
	defer s.Body.Close()
	ct, _, _ := mime.ParseMediaType(s.ContentType)
	if !strings.HasPrefix(ct, "image/") || ct == "image/svg+xml" {
		response.NotFound(c)
		return
	}
	c.Header("Cache-Control", "private, max-age=300")
	c.Header("X-Content-Type-Options", "nosniff")
	c.DataFromReader(http.StatusOK, s.Length, ct, s.Body, nil)
}

func (h *DriveHandler) About(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	a, err := h.svc.About(c.Request.Context(), middleware.GetUserID(c))
	if err != nil {
		driveFail(c, err)
		return
	}
	user := gin.H{"name": a.Name, "email": a.Email}
	if a.Photo != "" {
		user["photo"] = a.Photo
	}
	var limit *int64
	if a.Limit > 0 {
		limit = &a.Limit
	}
	response.OK(c, gin.H{"user": user, "storage": gin.H{"limit": limit, "usage": a.Usage,
		"usage_in_drive": a.UsageInDrive, "usage_in_trash": a.InTrash}})
}

// safeName strips control characters and path/quote characters from a file
// name used in Content-Disposition.
func safeName(name string) string {
	n := strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' || r == '/' || r == ' ' || r == ' ' {
			return -1
		}
		return r
	}, name))
	if r := []rune(n); len(r) > 150 {
		n = string(r[:150])
	}
	if n == "" {
		return "download"
	}
	return n
}

// disposition builds an RFC 6266 header with an ASCII fallback and an
// RFC 5987 UTF-8 filename*.
func disposition(kind, name string) string {
	name = safeName(name)
	var ascii, enc strings.Builder
	for _, r := range name {
		if r < 0x7f && r != '%' && r != ';' {
			ascii.WriteRune(r)
		} else {
			ascii.WriteByte('_')
		}
	}
	for _, b := range []byte(name) {
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.IndexByte("!#$&+-.^_`|~", b) >= 0 {
			enc.WriteByte(b)
		} else {
			enc.WriteString("%" + strings.ToUpper(strconv.FormatUint(uint64(b)+0x100, 16)[1:]))
		}
	}
	return kind + `; filename="` + ascii.String() + `"; filename*=UTF-8''` + enc.String()
}

func (h *DriveHandler) Content(c *gin.Context) {
	id, ok := h.pathID(c)
	if !ok {
		return
	}
	kind := c.DefaultQuery("disposition", "attachment")
	if kind != "inline" && kind != "attachment" {
		response.BadRequest(c, "invalid disposition")
		return
	}
	ctx, uid := c.Request.Context(), middleware.GetUserID(c)
	f, err := h.svc.Get(ctx, uid, id)
	if err != nil {
		driveFail(c, err)
		return
	}
	var export, outMime, name, rng = "", "", f.Name, ""
	switch {
	case f.MimeType == folderMimeG, f.MimeType == shortcutG:
		response.Error(c, http.StatusBadRequest, "not_a_file")
		return
	case strings.HasPrefix(f.MimeType, gNative):
		formats := exports[f.MimeType]
		if formats == nil {
			response.Error(c, http.StatusBadRequest, "not_a_file")
			return
		}
		format := c.DefaultQuery("format", formats[0])
		if !slices.Contains(formats, format) {
			response.Error(c, http.StatusBadRequest, "invalid_format")
			return
		}
		export, outMime = exportMime[format], exportMime[format]
		if !strings.HasSuffix(strings.ToLower(name), "."+format) {
			name += "." + format
		}
	default:
		if f.Size != nil && *f.Size > maxDownload {
			response.Error(c, http.StatusRequestEntityTooLarge, "too_large")
			return
		}
		outMime = f.MimeType
		if m, _, err := mime.ParseMediaType(outMime); err == nil {
			outMime = m
		} else {
			outMime = "application/octet-stream"
		}
		if strings.HasPrefix(outMime, "audio/") || strings.HasPrefix(outMime, "video/") {
			rng = c.GetHeader("Range")
		}
	}
	if kind == "inline" && !inlineOK[outMime] {
		kind = "attachment"
	}
	s, err := h.svc.Content(ctx, uid, id, export, rng)
	if err != nil {
		driveFail(c, err)
		return
	}
	defer s.Body.Close()
	hd := c.Writer.Header()
	hd.Set("Content-Disposition", disposition(kind, name))
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "sandbox")
	hd.Set("Cache-Control", "private, no-store")
	if export == "" {
		hd.Set("Accept-Ranges", "bytes")
	}
	if s.ContentRange != "" && s.Status == http.StatusPartialContent {
		hd.Set("Content-Range", s.ContentRange)
	}
	status := http.StatusOK
	if s.Status == http.StatusPartialContent && s.ContentRange != "" {
		status = s.Status
	}
	c.DataFromReader(status, s.Length, outMime, s.Body, nil)
}
