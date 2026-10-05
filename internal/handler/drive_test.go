package handler

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	"github.com/chronaxis/daily-planner-backend/internal/middleware"
	pkgjwt "github.com/chronaxis/daily-planner-backend/pkg/jwt"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	idA = "folderAAAA1"
	idB = "folderBBBB2"
	idF = "fileFFFFFF3"
)

// allCaps is a file the user fully controls.
var allCaps = googlecal.Caps{CanRename: true, CanTrash: true, CanDelete: true, CanMove: true, CanAddChildren: true}

// fakeBrowser is an in-memory googlecal.DriveBrowser; no network. err fails
// every call, writeErr only the calls that change something.
type fakeBrowser struct {
	files    map[string]googlecal.FileInfo
	body     string
	ctype    string
	err      error
	lastList googlecal.FilesListOpts
	lastRng  string
	lastExp  string
	about    googlecal.About
	next     string

	writeErr error
	calls    []string // changing calls in order: upload, folder, update, delete
	seq      int

	lastUp   googlecal.UploadInput // metadata of the last upload (Body already consumed)
	upBody   []byte                // what the upload read (first MiB)
	upBytes  int64                 // total bytes the upload read
	upCRC    uint32                // CRC-32 of everything it read
	upErr    error                 // error that ended the upload, if any
	upCtxErr error                 // ctx.Err() at that moment: non-nil = upstream was aborted
	onUpRead func(total int64)     // called after every read of the upload body

	lastUpdID string
	lastUpd   googlecal.FileUpdate
	lastDir   googlecal.FileInfo // last created folder
}

func (f *fakeBrowser) ListFiles(_ context.Context, _ string, o googlecal.FilesListOpts) (googlecal.FileList, error) {
	f.lastList = o
	out := googlecal.FileList{NextPageToken: f.next}
	for _, x := range f.files {
		out.Files = append(out.Files, x)
	}
	return out, f.err
}

func (f *fakeBrowser) GetFile(_ context.Context, _, id string) (googlecal.FileInfo, error) {
	if f.err != nil {
		return googlecal.FileInfo{}, f.err
	}
	if x, ok := f.files[id]; ok {
		return x, nil
	}
	return googlecal.FileInfo{}, googlecal.ErrDriveNotFound
}

func (f *fakeBrowser) stream() *googlecal.Stream {
	return &googlecal.Stream{Body: io.NopCloser(strings.NewReader(f.body)), Status: 200, ContentType: f.ctype, Length: int64(len(f.body))}
}

func (f *fakeBrowser) Download(_ context.Context, _, _, rng string) (*googlecal.Stream, error) {
	f.lastRng = rng
	return f.stream(), f.err
}

func (f *fakeBrowser) Export(_ context.Context, _, _, mime string) (*googlecal.Stream, error) {
	f.lastExp = mime
	return f.stream(), f.err
}

func (f *fakeBrowser) Thumbnail(context.Context, string, string) (*googlecal.Stream, error) {
	return f.stream(), f.err
}

func (f *fakeBrowser) About(context.Context, string) (googlecal.About, error) { return f.about, f.err }

func (f *fakeBrowser) UploadFile(ctx context.Context, _ string, in googlecal.UploadInput) (googlecal.FileInfo, error) {
	f.calls = append(f.calls, "upload")
	f.lastUp = googlecal.UploadInput{Name: in.Name, Parent: in.Parent, MimeType: in.MimeType}
	f.upBody, f.upBytes, f.upCRC, f.upErr, f.upCtxErr = nil, 0, 0, nil, nil
	if f.writeErr != nil {
		return googlecal.FileInfo{}, f.writeErr
	}
	buf := make([]byte, 32<<10) // small reads: nothing here holds the whole body
	for {
		n, err := in.Body.Read(buf)
		f.upBytes += int64(n)
		f.upCRC = crc32.Update(f.upCRC, crc32.IEEETable, buf[:n])
		if len(f.upBody) < 1<<20 {
			f.upBody = append(f.upBody, buf[:n]...)
		}
		if f.onUpRead != nil {
			f.onUpRead(f.upBytes)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			f.upErr, f.upCtxErr = err, ctx.Err()
			return googlecal.FileInfo{}, err
		}
	}
	f.seq++
	size := f.upBytes
	fi := googlecal.FileInfo{ID: fmt.Sprintf("newFILE%04d", f.seq), Name: in.Name, MimeType: in.MimeType,
		Parents: []string{in.Parent}, Size: &size, OwnedByMe: true, Caps: allCaps}
	f.files[fi.ID] = fi
	return fi, nil
}

func (f *fakeBrowser) CreateFolder(_ context.Context, _, name, parent string) (googlecal.FileInfo, error) {
	f.calls = append(f.calls, "folder")
	if f.writeErr != nil {
		return googlecal.FileInfo{}, f.writeErr
	}
	f.seq++
	fi := googlecal.FileInfo{ID: fmt.Sprintf("newFLDR%05d", f.seq), Name: name, MimeType: folderMimeG,
		Parents: []string{parent}, OwnedByMe: true, Caps: allCaps}
	f.files[fi.ID], f.lastDir = fi, fi
	return fi, nil
}

func (f *fakeBrowser) UpdateFile(_ context.Context, _, id string, u googlecal.FileUpdate) (googlecal.FileInfo, error) {
	f.calls = append(f.calls, "update")
	f.lastUpdID, f.lastUpd = id, u
	if f.writeErr != nil {
		return googlecal.FileInfo{}, f.writeErr
	}
	fi, ok := f.files[id]
	if !ok {
		return googlecal.FileInfo{}, googlecal.ErrDriveNotFound
	}
	if u.Name != nil {
		fi.Name = *u.Name
	}
	if u.Starred != nil {
		fi.Starred = *u.Starred
	}
	if u.Trashed != nil {
		fi.Trashed = *u.Trashed
	}
	if u.AddParent != "" || len(u.RemoveParents) > 0 {
		ps := slices.DeleteFunc(slices.Clone(fi.Parents), func(p string) bool { return slices.Contains(u.RemoveParents, p) })
		if u.AddParent != "" {
			ps = append(ps, u.AddParent)
		}
		fi.Parents = ps
	}
	f.files[id] = fi
	return fi, nil
}

func (f *fakeBrowser) DeleteFile(_ context.Context, _, id string) error {
	f.calls = append(f.calls, "delete")
	if f.writeErr != nil {
		return f.writeErr
	}
	if _, ok := f.files[id]; !ok {
		return googlecal.ErrDriveNotFound
	}
	delete(f.files, id)
	return nil
}

func i64(v int64) *int64 { return &v }

type driveEnv struct {
	fb     *fakeBrowser
	r      *gin.Engine
	token  string
	tokErr error
}

func newDriveEnv(t *testing.T) *driveEnv {
	t.Helper()
	e := &driveEnv{fb: &fakeBrowser{files: map[string]googlecal.FileInfo{
		"root": {ID: "rootREAL001", Name: "My Drive", MimeType: folderMimeG, Caps: allCaps},
		idA:    {ID: idA, Name: "A", MimeType: folderMimeG, Parents: []string{"rootREAL001"}, Caps: allCaps},
		idB:    {ID: idB, Name: "B", MimeType: folderMimeG, Parents: []string{idA}, Caps: allCaps},
		idF:    {ID: idF, Name: "f.pdf", MimeType: "application/pdf", Size: i64(10), Parents: []string{idB}, Caps: allCaps, ThumbnailLink: "https://lh3.googleusercontent.com/x"},
	}, body: "hello", ctype: "image/png"}}
	e.files()
	jwt := pkgjwt.NewManager(strings.Repeat("k", 32), time.Minute, time.Hour)
	svc := googlecal.NewFilesServiceWithToken(func(context.Context, uuid.UUID) (string, error) {
		return "rt", e.tokErr
	}, e.fb)
	r := gin.New()
	g := r.Group("/api/v1")
	g.Use(middleware.Auth(jwt))
	RegisterDriveRoutes(g, NewDriveHandler(svc))
	e.r = r
	var err error
	e.token, err = jwt.GenerateAccess(uuid.New())
	require.NoError(t, err)
	return e
}

func (e *driveEnv) files() { e.fb.files["rootREAL001"] = e.fb.files["root"] }

func (e *driveEnv) get(path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func TestDriveViewQueries(t *testing.T) {
	e := newDriveEnv(t)
	cases := map[string][2]string{
		"/drive/files":                            {"'root' in parents and trashed=false", "folder,name"},
		"/drive/files?parent=" + idA:              {"'" + idA + "' in parents and trashed=false", "folder,name"},
		"/drive/files?view=recent":                {"trashed=false", "viewedByMeTime desc"},
		"/drive/files?view=starred":               {"starred=true and trashed=false", "folder,name"},
		"/drive/files?view=shared&order=modified": {"sharedWithMe=true and trashed=false", "folder,modifiedTime desc"},
		"/drive/files?order=size&dir=desc":        {"'root' in parents and trashed=false", "folder,quotaBytesUsed desc"},
		"/drive/files?order=modified&dir=asc":     {"'root' in parents and trashed=false", "folder,modifiedTime"},
		"/drive/files?q=cat&parent=" + idA:        {"(name contains 'cat' or fullText contains 'cat') and trashed=false", "folder,name"},
		"/drive/files?view=starred&q=x":           {"(name contains 'x' or fullText contains 'x') and starred=true and trashed=false", "folder,name"},
	}
	for path, want := range cases {
		require.Equal(t, 200, e.get(path).Code, path)
		assert.Equal(t, want[0], e.fb.lastList.Query, path)
		assert.Equal(t, want[1], e.fb.lastList.OrderBy, path)
	}
}

func TestDriveSearchEscapingAndLimits(t *testing.T) {
	e := newDriveEnv(t)
	require.Equal(t, 200, e.get("/drive/files?q=o%27b%5Cc+%C3%A9%F0%9F%99%82").Code)
	assert.Contains(t, e.fb.lastList.Query, `contains 'o\'b\\c é🙂'`)
	assert.Equal(t, 50, e.fb.lastList.PageSize)
	e.get("/drive/files?limit=1000&page_token=tok")
	assert.Equal(t, 100, e.fb.lastList.PageSize)
	assert.Equal(t, "tok", e.fb.lastList.PageToken)
	for _, bad := range []string{"view=x", "order=x", "dir=up", "q=" + strings.Repeat("a", 101)} {
		assert.Equal(t, 400, e.get("/drive/files?"+bad).Code, bad)
	}
	e.fb.next = "n2"
	w := e.get("/drive/files")
	assert.Contains(t, w.Body.String(), `"next_page_token":"n2"`)
}

func TestDriveInvalidID(t *testing.T) {
	e := newDriveEnv(t)
	for _, p := range []string{"/drive/files/short", "/drive/files/bad%20idxxxxxxx", "/drive/files/short/path", "/drive/files/x/content", "/drive/files?parent=a%27b"} {
		w := e.get(p)
		assert.Equal(t, 400, w.Code, p)
		assert.Contains(t, w.Body.String(), "invalid_id", p)
	}
	assert.Equal(t, 404, e.get("/drive/files/zzzzzzzzzz").Code)
	assert.Equal(t, 200, e.get("/drive/files/root").Code)
}

func TestDriveMetadataMapping(t *testing.T) {
	e := newDriveEnv(t)
	owner := "Ana"
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, Name: "x", MimeType: "image/png", Size: i64(5), OwnerName: &owner,
		ThumbnailLink: "t", WebViewLink: "https://drive/x", Parents: []string{idA}, Description: "d", OwnedByMe: true}
	body := e.get("/drive/files/" + idF).Body.String()
	for _, s := range []string{`"is_folder":false`, `"is_google_doc":false`, `"size":5`, `"has_thumbnail":true`, `"owner_name":"Ana"`,
		`"description":"d"`, `"parents":["folderAAAA1"]`, `"web_view_link":"https://drive/x"`, `"icon_link":null`} {
		assert.Contains(t, body, s)
	}
	assert.NotContains(t, body, "thumbnailLink")
	assert.NotContains(t, body, "webContentLink")
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, MimeType: gNative + "document"}
	body = e.get("/drive/files/" + idF).Body.String()
	assert.Contains(t, body, `"is_google_doc":true`)
	assert.Contains(t, body, `"size":null`)
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, MimeType: folderMimeG}
	body = e.get("/drive/files/" + idF).Body.String()
	assert.Contains(t, body, `"is_folder":true`)
	assert.Contains(t, body, `"is_google_doc":false`)
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, MimeType: shortcutG, ShortcutTargetID: "targetID123", ShortcutMime: "application/pdf"}
	body = e.get("/drive/files/" + idF).Body.String()
	assert.Contains(t, body, `"is_shortcut":true`)
	assert.Contains(t, body, `"shortcut_target_id":"targetID123"`)
	assert.Contains(t, body, `"is_google_doc":false`)
}

func TestDrivePath(t *testing.T) {
	e := newDriveEnv(t)
	body := e.get("/drive/files/" + idF + "/path").Body.String()
	assert.Contains(t, body, `[{"id":"root","name":"Drive Saya"},{"id":"folderAAAA1","name":"A"},{"id":"folderBBBB2","name":"B"},{"id":"fileFFFFFF3","name":"f.pdf"}]`)
	assert.Contains(t, e.get("/drive/files/root/path").Body.String(), `[{"id":"root","name":"Drive Saya"}]`)
	// partial chain: ancestor not accessible
	e.fb.files[idB] = googlecal.FileInfo{ID: idB, Name: "B", MimeType: folderMimeG, Parents: []string{"goneGONE001"}}
	body = e.get("/drive/files/" + idB + "/path").Body.String()
	assert.Contains(t, body, `[{"id":"folderBBBB2","name":"B"}]`)
	// cycle guard
	e.fb.files[idA] = googlecal.FileInfo{ID: idA, Name: "A", Parents: []string{idB}}
	e.fb.files[idB] = googlecal.FileInfo{ID: idB, Name: "B", Parents: []string{idA}}
	w := e.get("/drive/files/" + idB + "/path")
	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), `[{"id":"folderAAAA1","name":"A"},{"id":"folderBBBB2","name":"B"}]`)
	// depth guard: a 30-long chain yields at most 20 segments
	for i := 0; i < 30; i++ {
		id, parent := "chainNODE"+string(rune('a'+i%26))+string(rune('A'+i/26)), "chainNODE"+string(rune('a'+(i+1)%26))+string(rune('A'+(i+1)/26))
		e.fb.files[id] = googlecal.FileInfo{ID: id, Name: id, Parents: []string{parent}}
	}
	w = e.get("/drive/files/chainNODEaA/path")
	assert.Equal(t, 20, strings.Count(w.Body.String(), `"id"`))
}

func TestDriveThumbnail(t *testing.T) {
	e := newDriveEnv(t)
	w := e.get("/drive/files/" + idF + "/thumbnail")
	require.Equal(t, 200, w.Code)
	assert.Equal(t, "private, max-age=300", w.Header().Get("Cache-Control"))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
	e.fb.ctype = "text/html"
	assert.Equal(t, 404, e.get("/drive/files/"+idF+"/thumbnail").Code)
	e.fb.files[idF] = googlecal.FileInfo{ID: idF}
	assert.Equal(t, 404, e.get("/drive/files/"+idF+"/thumbnail").Code)
}

func TestDriveContentInlineAllowlist(t *testing.T) {
	e := newDriveEnv(t)
	for mt, wantKind := range map[string]string{"application/pdf": "inline", "image/png": "inline", "text/markdown": "inline",
		"image/svg+xml": "attachment", "text/html": "attachment", "application/javascript": "attachment", "": "attachment"} {
		e.fb.files[idF] = googlecal.FileInfo{ID: idF, Name: "n", MimeType: mt, Size: i64(5)}
		w := e.get("/drive/files/" + idF + "/content?disposition=inline")
		require.Equal(t, 200, w.Code, mt)
		assert.True(t, strings.HasPrefix(w.Header().Get("Content-Disposition"), wantKind+";"), mt)
		assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
		assert.Equal(t, "sandbox", w.Header().Get("Content-Security-Policy"))
		assert.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
		assert.Equal(t, "hello", w.Body.String())
	}
	// default is attachment; junk disposition rejected
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, Name: "n", MimeType: "application/pdf"}
	assert.True(t, strings.HasPrefix(e.get("/drive/files/"+idF+"/content").Header().Get("Content-Disposition"), "attachment;"))
	assert.Equal(t, 400, e.get("/drive/files/"+idF+"/content?disposition=x").Code)
}

func TestDriveContentExportAndErrors(t *testing.T) {
	e := newDriveEnv(t)
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, Name: "Doc", MimeType: gNative + "document"}
	w := e.get("/drive/files/" + idF + "/content?disposition=inline")
	require.Equal(t, 200, w.Code)
	assert.Equal(t, "application/pdf", e.fb.lastExp)
	assert.Equal(t, "application/pdf", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Header().Get("Content-Disposition"), `inline; filename="Doc.pdf"`)
	e.get("/drive/files/" + idF + "/content?format=docx")
	assert.Contains(t, e.fb.lastExp, "wordprocessingml")
	assert.Equal(t, 400, e.get("/drive/files/"+idF+"/content?format=xlsx").Code)
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, Name: "S", MimeType: gNative + "spreadsheet"}
	e.get("/drive/files/" + idF + "/content?format=xlsx")
	assert.Contains(t, e.fb.lastExp, "spreadsheetml")
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, Name: "D", MimeType: gNative + "drawing"}
	e.get("/drive/files/" + idF + "/content")
	assert.Equal(t, "image/png", e.fb.lastExp)
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, Name: "F", MimeType: gNative + "form"}
	assert.Equal(t, 400, e.get("/drive/files/"+idF+"/content").Code)

	for _, mt := range []string{folderMimeG, shortcutG} {
		e.fb.files[idF] = googlecal.FileInfo{ID: idF, MimeType: mt}
		w = e.get("/drive/files/" + idF + "/content")
		assert.Equal(t, 400, w.Code)
		assert.Contains(t, w.Body.String(), "not_a_file")
	}
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, MimeType: "video/mp4", Size: i64(200<<20 + 1)}
	w = e.get("/drive/files/" + idF + "/content")
	assert.Equal(t, 413, w.Code)
	assert.Contains(t, w.Body.String(), "too_large")
}

func TestDriveContentRange(t *testing.T) {
	e := newDriveEnv(t)
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, Name: "v", MimeType: "video/mp4", Size: i64(5)}
	e.get("/drive/files/"+idF+"/content", "Range", "bytes=0-1")
	assert.Equal(t, "bytes=0-1", e.fb.lastRng)
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, Name: "v", MimeType: "application/pdf", Size: i64(5)}
	e.get("/drive/files/"+idF+"/content", "Range", "bytes=0-1")
	assert.Equal(t, "", e.fb.lastRng)
}

func TestDriveFilenameSanitising(t *testing.T) {
	assert.Equal(t, `attachment; filename="a_b"; filename*=UTF-8''a%C3%A9b`, strings.Replace(disposition("attachment", "aéb"), "a__b", "a_b", 1))
	d := disposition("inline", "x\"y\r\nz\\/.pdf")
	assert.Equal(t, `inline; filename="xyz.pdf"; filename*=UTF-8''xyz.pdf`, d)
	assert.NotContains(t, disposition("inline", "a\nb;c%d"), "\n")
	assert.Equal(t, `attachment; filename="download"; filename*=UTF-8''download`, disposition("attachment", "\r\n\""))
	assert.Contains(t, disposition("attachment", "日本語.txt"), "filename*=UTF-8''%E6%97%A5")
}

func TestDriveAbout(t *testing.T) {
	e := newDriveEnv(t)
	e.fb.about = googlecal.About{Name: "Ana", Email: "a@x.id", Usage: 5, UsageInDrive: 4, InTrash: 1}
	body := e.get("/drive/about").Body.String()
	assert.Contains(t, body, `"limit":null`)
	assert.Contains(t, body, `"usage":5`)
	assert.NotContains(t, body, "photo")
	e.fb.about.Limit = 100
	assert.Contains(t, e.get("/drive/about").Body.String(), `"limit":100`)
}

func TestDriveErrorMapping(t *testing.T) {
	e := newDriveEnv(t)
	cases := []struct {
		err  error
		code int
		body string
	}{
		{googlecal.ErrDriveScope, 409, "drive_scope_missing"},
		{googlecal.ErrDriveDisabled, 502, "drive_api_disabled"},
		{googlecal.ErrDriveNotFound, 404, ""},
		{errors.New("boom"), 502, "google_api_error"},
	}
	for _, tc := range cases {
		e.fb.err = tc.err
		for _, p := range []string{"/drive/files", "/drive/files/" + idF, "/drive/about", "/drive/files/" + idF + "/content"} {
			w := e.get(p)
			assert.Equal(t, tc.code, w.Code, p)
			assert.Contains(t, w.Body.String(), tc.body)
		}
	}
	e.fb.err = nil
	e.tokErr = googlecal.ErrNotConnected
	w := e.get("/drive/files")
	assert.Equal(t, 409, w.Code)
	assert.Contains(t, w.Body.String(), "not_connected")
	e.tokErr = googlecal.ErrNeedsReauth
	w = e.get("/drive/files")
	assert.Equal(t, 409, w.Code)
	assert.Contains(t, w.Body.String(), "needs_reauth")
}

func TestDriveRequiresAuth(t *testing.T) {
	e := newDriveEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/drive/files", nil)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	assert.Equal(t, 401, w.Code)
}

func TestDriveNotConfigured(t *testing.T) {
	e := newDriveEnv(t)
	r := gin.New()
	g := r.Group("/api/v1")
	RegisterDriveRoutes(g, NewDriveHandler(nil))
	for _, p := range []string{"/drive/files", "/drive/about", "/drive/files/" + idF, "/drive/files/" + idF + "/content"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1"+p, nil))
		assert.Equal(t, 503, w.Code, p)
	}
	_ = e
}

// The Drive surface is exactly these routes: nothing else may write, and there
// is deliberately no emptyTrash, copy or share route.
func TestDriveRoutesAllowList(t *testing.T) {
	e := newDriveEnv(t)
	want := []string{
		"DELETE /api/v1/drive/files/:id",
		"GET /api/v1/drive/about",
		"GET /api/v1/drive/files",
		"GET /api/v1/drive/files/:id",
		"GET /api/v1/drive/files/:id/content",
		"GET /api/v1/drive/files/:id/path",
		"GET /api/v1/drive/files/:id/thumbnail",
		"PATCH /api/v1/drive/files/:id",
		"POST /api/v1/drive/files/:id/restore",
		"POST /api/v1/drive/files/:id/trash",
		"POST /api/v1/drive/files/upload",
		"POST /api/v1/drive/folders",
	}
	var got []string
	for _, ri := range e.r.Routes() {
		if strings.Contains(ri.Path, "/drive") {
			got = append(got, ri.Method+" "+ri.Path)
		}
	}
	slices.Sort(got)
	assert.Equal(t, want, got)

	// every other write verb/path combination is unrouted
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, p := range []string{"/drive/files", "/drive/files/" + idF + "/content", "/drive/files/" + idF + "/path",
			"/drive/files/" + idF + "/thumbnail", "/drive/about", "/drive/trash", "/drive/trash/empty", "/drive/emptyTrash",
			"/drive/files/" + idF + "/copy", "/drive/files/" + idF + "/share", "/drive/files/" + idF + "/permissions"} {
			req := httptest.NewRequest(m, "/api/v1"+p, nil)
			req.Header.Set("Authorization", "Bearer "+e.token)
			w := httptest.NewRecorder()
			e.r.ServeHTTP(w, req)
			assert.Contains(t, []int{404, 405}, w.Code, m+" "+p)
		}
	}
	// a PUT on a file is not routed either (the allow-list has PATCH only)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/drive/files/"+idF, nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	assert.Contains(t, []int{404, 405}, w.Code)
	assert.Empty(t, e.fb.calls, "no Drive write was reached")
}
