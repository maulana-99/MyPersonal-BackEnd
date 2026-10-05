package handler

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	"github.com/chronaxis/daily-planner-backend/pkg/response"
	"github.com/gin-gonic/gin"
)

const (
	maxUpload   = 200 << 20 // bytes of file content per upload
	uploadSlack = 1 << 20   // multipart framing allowed on top of that
	maxName     = 255
	unnamed     = "Tanpa nama"
)

var errExtraPart = errors.New("unexpected extra part")

// writeID is pathID for mutating routes: the "root" alias is never a target.
func (h *DriveHandler) writeID(c *gin.Context) (string, bool) {
	id, ok := h.pathID(c)
	if ok && id == "root" {
		response.Error(c, http.StatusBadRequest, "invalid_id")
		return "", false
	}
	return id, ok
}

// bindStrict reads one small JSON object and rejects unknown fields, so a
// client cannot slip an unsupported change (say "trashed") through PATCH. The
// error text is fixed: it never echoes what the client sent.
func bindStrict(c *gin.Context, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		response.BadRequest(c, "invalid body")
		return false
	}
	return true
}

// tidyName strips control characters, bidi overrides, zero-width chars,
// line/paragraph separators, and surrounding space.
func tidyName(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) ||
			unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "")))
}

// fileNameForUpload makes an uploaded file's name safe: tidy, at most 255
// bytes (cut on a rune boundary), never empty (falls back to unnamed).
func fileNameForUpload(raw string) string {
	n := tidyName(raw)
	if len(n) > maxName {
		cut := maxName
		for !utf8.RuneStart(n[cut]) {
			cut--
		}
		n = strings.TrimSpace(n[:cut])
	}
	if n == "" {
		return unnamed
	}
	return n
}

// uploadMime returns the part's media type when it is well formed, else
// octet-stream. Wildcards and Google-native types are refused: sending one
// would ask Drive to convert the upload into a Doc, a folder or a shortcut.
func uploadMime(header string) string {
	m, _, err := mime.ParseMediaType(header)
	if err != nil || !strings.Contains(m, "/") || strings.Contains(m, "*") || strings.HasPrefix(m, gNative) {
		return "application/octet-stream"
	}
	return m
}

// onlyPart is the single file part. When it ends it insists that nothing
// follows, so a request with extra parts (or a cut-off tail) fails before Drive
// commits the file.
type onlyPart struct {
	p    *multipart.Part
	mr   *multipart.Reader
	done bool
}

func (o *onlyPart) Read(b []byte) (int, error) {
	n, err := o.p.Read(b)
	if err == io.EOF && !o.done {
		o.done = true
		if _, e := o.mr.NextPart(); e != io.EOF {
			if e == nil {
				e = errExtraPart
			}
			return n, e
		}
	}
	return n, err
}

// Upload: POST /drive/files/upload?parent=<id|root>, multipart/form-data with a
// single part named "file", streamed straight to Drive.
func (h *DriveHandler) Upload(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	parent := c.DefaultQuery("parent", "root")
	if !validFileID(c, parent) {
		return
	}
	if c.Request.ContentLength > maxUpload+uploadSlack {
		response.Error(c, http.StatusRequestEntityTooLarge, "too_large")
		return
	}
	// Bounds everything read from the client, including framing and junk.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUpload+uploadSlack)
	mr, err := c.Request.MultipartReader()
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalid_upload")
		return
	}
	part, err := mr.NextPart()
	var big *http.MaxBytesError
	switch {
	case errors.As(err, &big):
		response.Error(c, http.StatusRequestEntityTooLarge, "too_large")
		return
	case err != nil, part.FormName() != "file":
		response.Error(c, http.StatusBadRequest, "invalid_upload")
		return
	case part.FileName() == "":
		response.Error(c, http.StatusBadRequest, "invalid_upload")
		return
	}
	f, err := h.svc.Upload(c.Request.Context(), middleware.GetUserID(c), googlecal.UploadReq{
		Parent: parent, Name: fileNameForUpload(part.FileName()), MimeType: uploadMime(part.Header.Get("Content-Type")),
		Body: &onlyPart{p: part, mr: mr}, Max: maxUpload})
	if err != nil {
		driveFail(c, err)
		return
	}
	response.Created(c, toDriveFile(f, false))
}

// CreateFolder: POST /drive/folders {name (1..255), parent (id or root)}.
func (h *DriveHandler) CreateFolder(c *gin.Context) {
	if !h.ready(c) {
		return
	}
	var req struct {
		Name   string `json:"name"`
		Parent string `json:"parent"`
	}
	if !bindStrict(c, &req) {
		return
	}
	name := tidyName(req.Name)
	if name == "" || len(name) > maxName {
		response.Error(c, http.StatusBadRequest, "invalid_name")
		return
	}
	if req.Parent == "" {
		req.Parent = "root"
	}
	if !validFileID(c, req.Parent) {
		return
	}
	f, err := h.svc.CreateFolder(c.Request.Context(), middleware.GetUserID(c), name, req.Parent)
	if err != nil {
		driveFail(c, err)
		return
	}
	response.Created(c, toDriveFile(f, false))
}

// Update: PATCH /drive/files/:id with any of {name, starred, parent}.
func (h *DriveHandler) Update(c *gin.Context) {
	id, ok := h.writeID(c)
	if !ok {
		return
	}
	var req struct {
		Name    *string `json:"name"`
		Starred *bool   `json:"starred"`
		Parent  *string `json:"parent"`
	}
	if !bindStrict(c, &req) {
		return
	}
	p := googlecal.FilePatch{Starred: req.Starred}
	if req.Name != nil {
		tidy := tidyName(*req.Name)
		if tidy == "" || len(tidy) > maxName {
			response.Error(c, http.StatusBadRequest, "invalid_name")
			return
		}
		p.Name = &tidy
	}
	if req.Parent != nil {
		if !validFileID(c, *req.Parent) {
			return
		}
		p.Parent = *req.Parent
	}
	if p.Name == nil && p.Starred == nil && p.Parent == "" {
		response.Error(c, http.StatusBadRequest, "no_changes")
		return
	}
	f, err := h.svc.Update(c.Request.Context(), middleware.GetUserID(c), id, p)
	if err != nil {
		driveFail(c, err)
		return
	}
	response.OK(c, toDriveFile(f, false))
}

func (h *DriveHandler) Trash(c *gin.Context)   { h.setTrashed(c, true) }
func (h *DriveHandler) Restore(c *gin.Context) { h.setTrashed(c, false) }

func (h *DriveHandler) setTrashed(c *gin.Context, trashed bool) {
	id, ok := h.writeID(c)
	if !ok {
		return
	}
	f, err := h.svc.SetTrashed(c.Request.Context(), middleware.GetUserID(c), id, trashed)
	if err != nil {
		driveFail(c, err)
		return
	}
	response.OK(c, toDriveFile(f, false))
}

// Delete: DELETE /drive/files/:id removes a file permanently, only from the trash.
func (h *DriveHandler) Delete(c *gin.Context) {
	id, ok := h.writeID(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), middleware.GetUserID(c), id); err != nil {
		driveFail(c, err)
		return
	}
	response.NoContent(c)
}
