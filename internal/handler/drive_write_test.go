package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chronaxis/daily-planner-backend/internal/googlecal"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	idC   = "folderCCCC4" // folder under the root, a sibling of A
	idRO  = "readonlyFL5" // folder the user may not add children to
	idTrD = "trashedFLD6" // trashed folder
	idTrF = "trashedFIL7" // trashed file
	idNew = "newTARGET88" // a file in the root
)

// writeEnv is the read-only env plus fixtures for the write routes.
func newWriteEnv(t *testing.T) *driveEnv {
	e := newDriveEnv(t)
	root := []string{"rootREAL001"}
	e.fb.files[idC] = googlecal.FileInfo{ID: idC, Name: "C", MimeType: folderMimeG, Parents: root, Caps: allCaps}
	e.fb.files[idRO] = googlecal.FileInfo{ID: idRO, Name: "RO", MimeType: folderMimeG, Parents: root,
		Caps: googlecal.Caps{CanRename: true, CanMove: true}}
	e.fb.files[idTrD] = googlecal.FileInfo{ID: idTrD, Name: "T", MimeType: folderMimeG, Parents: root, Trashed: true, Caps: allCaps}
	e.fb.files[idTrF] = googlecal.FileInfo{ID: idTrF, Name: "t.txt", MimeType: "text/plain", Size: i64(3), Parents: root, Trashed: true, Caps: allCaps}
	e.fb.files[idNew] = googlecal.FileInfo{ID: idNew, Name: "n.txt", MimeType: "text/plain", Size: i64(3), Parents: root, Caps: allCaps}
	return e
}

// mpPart is one multipart part; disp, when set, replaces the generated Content-Disposition.
type mpPart struct {
	field, filename, ctype, disp string
	data                         []byte
}

func mpBody(parts ...mpPart) (*bytes.Buffer, string) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		d := p.disp
		if d == "" {
			d = `form-data; name="` + p.field + `"`
			if p.filename != "" {
				d += `; filename="` + p.filename + `"`
			}
		}
		h.Set("Content-Disposition", d)
		if p.ctype != "" {
			h.Set("Content-Type", p.ctype)
		}
		w, _ := mw.CreatePart(h)
		_, _ = w.Write(p.data)
	}
	_ = mw.Close()
	return &b, mw.FormDataContentType()
}

func (e *driveEnv) send(method, path string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/v1"+path, body)
	req.Header.Set("Authorization", "Bearer "+e.token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func (e *driveEnv) js(method, path, body string) *httptest.ResponseRecorder {
	return e.send(method, path, strings.NewReader(body), "Content-Type", "application/json")
}

func (e *driveEnv) upload(path string, parts ...mpPart) *httptest.ResponseRecorder {
	b, ct := mpBody(parts...)
	return e.send(http.MethodPost, path, b, "Content-Type", ct)
}

func filePart(name, ctype string, data string) mpPart {
	return mpPart{field: "file", filename: name, ctype: ctype, data: []byte(data)}
}

type envelope struct {
	Data  map[string]any `json:"data"`
	Error string         `json:"error"`
}

func decode(t *testing.T, w *httptest.ResponseRecorder) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), w.Body.String())
	return env
}

func (e *driveEnv) failed(t *testing.T, w *httptest.ResponseRecorder, code int, errCode string) {
	t.Helper()
	assert.Equal(t, code, w.Code, w.Body.String())
	assert.Equal(t, errCode, decode(t, w).Error)
}

// ---- upload -----------------------------------------------------------------

func TestDriveUploadCreatesFile(t *testing.T) {
	e := newWriteEnv(t)
	w := e.upload("/drive/files/upload?parent="+idA, filePart("a.txt", "text/plain; charset=utf-8", "hello, drive"))
	require.Equal(t, 201, w.Code, w.Body.String())
	got := decode(t, w).Data
	assert.Equal(t, "a.txt", got["name"])
	assert.Equal(t, "text/plain", got["mime_type"])
	assert.EqualValues(t, 12, got["size"])
	assert.Equal(t, false, got["trashed"])
	assert.Equal(t, []string{"upload"}, e.fb.calls)
	assert.Equal(t, googlecal.UploadInput{Name: "a.txt", Parent: idA, MimeType: "text/plain"}, e.fb.lastUp)
	assert.Equal(t, "hello, drive", string(e.fb.upBody))

	// no parent = the Drive root, sent as its real id
	require.Equal(t, 201, e.upload("/drive/files/upload", filePart("b.txt", "", "x")).Code)
	assert.Equal(t, "rootREAL001", e.fb.lastUp.Parent)
	assert.Equal(t, "application/octet-stream", e.fb.lastUp.MimeType)
}

// The body reaches Drive while the client is still sending: nothing in the
// handler or service waits for the whole request first.
func TestDriveUploadIsStreamed(t *testing.T) {
	e := newWriteEnv(t)
	first := make(chan struct{})
	var once sync.Once
	e.fb.onUpRead = func(n int64) {
		if n > 0 {
			once.Do(func() { close(first) })
		}
	}
	pr, pw := io.Pipe()
	defer pr.Close()
	mw := multipart.NewWriter(pw)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/drive/files/upload", pr)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		e.r.ServeHTTP(w, req)
		done <- w
	}()

	part, err := mw.CreateFormFile("file", "s.bin")
	require.NoError(t, err)
	_, err = part.Write([]byte("AAAA"))
	require.NoError(t, err)
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("the upload body was buffered instead of streamed")
	}
	_, err = part.Write([]byte("BBBB"))
	require.NoError(t, err)
	require.NoError(t, mw.Close())
	require.NoError(t, pw.Close())
	select {
	case w := <-done:
		require.Equal(t, 201, w.Code, w.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not finish")
	}
	assert.Equal(t, "AAAABBBB", string(e.fb.upBody))
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// serveStream sends a multipart body produced by write through the upload route
// without knowing its length, like a browser streaming a File.
func serveStream(e *driveEnv, ctx context.Context, write func(*multipart.Writer) error) *httptest.ResponseRecorder {
	pr, pw := io.Pipe()
	defer pr.Close() // frees the writer goroutine once the handler stops reading
	mw := multipart.NewWriter(pw)
	go func() {
		err := write(mw)
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/drive/files/upload", pr).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, req)
	return w
}

func TestDriveUploadLimit(t *testing.T) {
	e := newWriteEnv(t)
	before := len(e.fb.files)
	w := serveStream(e, context.Background(), func(mw *multipart.Writer) error {
		part, err := mw.CreateFormFile("file", "big.bin")
		if err != nil {
			return err
		}
		_, err = io.CopyN(part, zeroReader{}, maxUpload+1)
		return err
	})
	e.failed(t, w, 413, "too_large")
	assert.Equal(t, int64(maxUpload), int64(200<<20))
	assert.LessOrEqual(t, e.fb.upBytes, int64(maxUpload), "Drive never receives more than the limit")
	assert.ErrorIs(t, e.fb.upErr, googlecal.ErrTooLarge)
	assert.ErrorIs(t, e.fb.upCtxErr, context.Canceled, "the upstream request is aborted")
	assert.Len(t, e.fb.files, before, "nothing was created")
}

func TestDriveUploadClientGone(t *testing.T) {
	e := newWriteEnv(t)
	before := len(e.fb.files)
	first := make(chan struct{})
	var once sync.Once
	e.fb.onUpRead = func(n int64) {
		if n > 0 {
			once.Do(func() { close(first) })
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	defer pr.Close()
	mw := multipart.NewWriter(pw)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/drive/files/upload", pr).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		e.r.ServeHTTP(w, req)
		done <- w
	}()
	part, err := mw.CreateFormFile("file", "x.bin")
	require.NoError(t, err)
	_, err = part.Write([]byte("AAAA"))
	require.NoError(t, err)
	<-first
	cancel() // net/http cancels the request context when the client disconnects...
	pw.CloseWithError(io.ErrUnexpectedEOF)
	select {
	case w := <-done:
		assert.GreaterOrEqual(t, w.Code, 400)
	case <-time.After(5 * time.Second):
		t.Fatal("handler kept running after the client left")
	}
	assert.ErrorIs(t, e.fb.upCtxErr, context.Canceled, "the upstream request is aborted")
	assert.Len(t, e.fb.files, before, "nothing was created")
}

func TestDriveUploadRejectsBadRequests(t *testing.T) {
	e := newWriteEnv(t)
	before := len(e.fb.files)

	w := e.js(http.MethodPost, "/drive/files/upload", `{"file":"x"}`)
	e.failed(t, w, 400, "invalid_upload")
	e.failed(t, e.upload("/drive/files/upload", mpPart{field: "other", filename: "a.txt", data: []byte("x")}), 400, "invalid_upload")
	e.failed(t, e.upload("/drive/files/upload"), 400, "invalid_upload")                                           // no parts at all
	e.failed(t, e.upload("/drive/files/upload", mpPart{field: "file", data: []byte("x")}), 400, "invalid_upload") // no filename
	assert.Empty(t, e.fb.calls, "Drive was not reached")

	// a second part is refused before Drive can commit the first
	w = e.upload("/drive/files/upload", filePart("a.txt", "text/plain", "x"), mpPart{field: "note", data: []byte("y")})
	e.failed(t, w, 400, "invalid_upload")
	assert.Error(t, e.fb.upErr)
	assert.Len(t, e.fb.files, before, "no file was created")

	// a body cut off before the closing boundary never completes
	b, ct := mpBody(filePart("a.txt", "text/plain", "hello"))
	cut := b.Bytes()[:b.Len()-len("--\r\n")-20]
	w = e.send(http.MethodPost, "/drive/files/upload", bytes.NewReader(cut), "Content-Type", ct)
	e.failed(t, w, 400, "invalid_upload")
	assert.Len(t, e.fb.files, before)

	// declared size beyond the limit: refused before anything is read or sent
	e.fb.calls = nil
	req := httptest.NewRequest(http.MethodPost, "/api/v1/drive/files/upload", strings.NewReader("tiny"))
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", ct)
	req.ContentLength = maxUpload + uploadSlack + 1
	rec := httptest.NewRecorder()
	e.r.ServeHTTP(rec, req)
	e.failed(t, rec, 413, "too_large")
	assert.Empty(t, e.fb.calls)

	e.failed(t, e.upload("/drive/files/upload?parent=bad", filePart("a", "", "x")), 400, "invalid_id")
	e.failed(t, e.upload("/drive/files/upload?parent=a%27b", filePart("a", "", "x")), 400, "invalid_id")
}

func TestDriveUploadName(t *testing.T) {
	e := newWriteEnv(t)
	cases := []struct{ name, disp, want string }{
		{"plain", `form-data; name="file"; filename="plain.txt"`, "plain.txt"},
		{"trimmed", `form-data; name="file"; filename="   padded.txt  "`, "padded.txt"},
		{"control chars", `form-data; name="file"; filename*=UTF-8''%00a%0D%0Ab%7F%C2%85.txt`, "ab.txt"},
		{"path stripped", `form-data; name="file"; filename="dir/evil.txt"`, "evil.txt"},
		{"only control", `form-data; name="file"; filename*=UTF-8''%0A%09%00`, "Tanpa nama"},
		{"only spaces", `form-data; name="file"; filename="    "`, "Tanpa nama"},
		{"unicode kept", `form-data; name="file"; filename="日本語 🙂.txt"`, "日本語 🙂.txt"},
		{"254 bytes, rune safe", `form-data; name="file"; filename="` + strings.Repeat("é", 127) + `"`, strings.Repeat("é", 127)},
		{"255 ascii", `form-data; name="file"; filename="` + strings.Repeat("a", 255) + `"`, strings.Repeat("a", 255)},
		{"cut then trimmed", `form-data; name="file"; filename="` + strings.Repeat("a", 254) + ` bbb"`, strings.Repeat("a", 254)},
	}
	for _, tc := range cases {
		w := e.upload("/drive/files/upload", mpPart{disp: tc.disp, data: []byte("x")})
		require.Equal(t, 201, w.Code, tc.name+": "+w.Body.String())
		assert.Equal(t, tc.want, e.fb.lastUp.Name, tc.name)
		assert.LessOrEqual(t, len(e.fb.lastUp.Name), 255, tc.name)
		assert.True(t, utf8.ValidString(e.fb.lastUp.Name), tc.name)
	}
}

func TestDriveUploadContentType(t *testing.T) {
	e := newWriteEnv(t)
	for ct, want := range map[string]string{
		"text/plain; charset=utf-8":            "text/plain",
		"IMAGE/PNG":                            "image/png",
		"":                                     "application/octet-stream",
		"garbage":                              "application/octet-stream",
		"text/plain; charset":                  "application/octet-stream",
		"*/*":                                  "application/octet-stream",
		"text/*":                               "application/octet-stream",
		"application/vnd.google-apps.document": "application/octet-stream",
		"application/vnd.google-apps.folder":   "application/octet-stream",
		"application/vnd.google-apps.shortcut": "application/octet-stream",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	} {
		require.Equal(t, 201, e.upload("/drive/files/upload", filePart("a.bin", ct, "x")).Code, ct)
		assert.Equal(t, want, e.fb.lastUp.MimeType, ct)
	}
}

func TestDriveUploadParentChecks(t *testing.T) {
	e := newWriteEnv(t)
	up := func(parent string) *httptest.ResponseRecorder {
		return e.upload("/drive/files/upload?parent="+parent, filePart("a.txt", "text/plain", "x"))
	}
	e.failed(t, up(idRO), 403, "forbidden")
	e.failed(t, up(idTrD), 403, "forbidden")
	e.failed(t, up(idF), 400, "not_a_folder")
	e.failed(t, up(idNew), 400, "not_a_folder")
	assert.Equal(t, 404, up("zzzzzzzzzz").Code)
	assert.Empty(t, e.fb.calls, "refused before any byte was sent")

	// the root alias is always an allowed destination
	root := e.fb.files["root"]
	root.Caps = googlecal.Caps{}
	e.fb.files["root"] = root
	assert.Equal(t, 201, up("root").Code)
	assert.Equal(t, 201, up(idA).Code)
	assert.Equal(t, 201, up(idC).Code)
}

// ---- folders ----------------------------------------------------------------

func TestDriveCreateFolder(t *testing.T) {
	e := newWriteEnv(t)
	w := e.js(http.MethodPost, "/drive/folders", `{"name":"  New\u0000 folder ","parent":"`+idA+`"}`)
	require.Equal(t, 201, w.Code, w.Body.String())
	got := decode(t, w).Data
	assert.Equal(t, "New folder", got["name"])
	assert.Equal(t, true, got["is_folder"])
	assert.Equal(t, []string{idA}, e.fb.lastDir.Parents)

	require.Equal(t, 201, e.js(http.MethodPost, "/drive/folders", `{"name":"Top"}`).Code)
	assert.Equal(t, []string{"rootREAL001"}, e.fb.lastDir.Parents, "parent defaults to the root, by real id")
	require.Equal(t, 201, e.js(http.MethodPost, "/drive/folders", `{"name":"Top2","parent":"root"}`).Code)

	// 1..255 bytes; 127 multi-byte characters (é = 2 bytes) fit in 255 bytes
	require.Equal(t, 201, e.js(http.MethodPost, "/drive/folders", `{"name":"`+strings.Repeat("é", 127)+`"}`).Code)
	// empty, whitespace-only, control chars only, 256+ bytes all fail with invalid_name
	n := len(e.fb.calls)
	for _, body := range []string{`{"name":""}`, `{"name":"   "}`, `{"name":"\u0000\u0007"}`, `{"name":"` + strings.Repeat("a", 256) + `"}`} {
		e.failed(t, e.js(http.MethodPost, "/drive/folders", body), 400, "invalid_name")
	}
	assert.Len(t, e.fb.calls, n, "no folder was created by invalid_name requests")

	// bidi override chars are stripped before checking length
	require.Equal(t, 201, e.js(http.MethodPost, "/drive/folders", `{"name":"hello‮world"}`).Code)
	assert.Equal(t, "helloworld", e.fb.lastDir.Name, "bidi char stripped (U+202E)")

	n = len(e.fb.calls)
	for _, body := range []string{`{}`, ``, `nope`, `{"name":5}`, `{"name":"x","extra":1}`, `{"name":"x"} {"name":"y"}`, `[]`} {
		assert.Equal(t, 400, e.js(http.MethodPost, "/drive/folders", body).Code, body)
	}
	e.failed(t, e.js(http.MethodPost, "/drive/folders", `{"name":"x","parent":"bad id"}`), 400, "invalid_id")
	e.failed(t, e.js(http.MethodPost, "/drive/folders", `{"name":"x","parent":"`+idF+`"}`), 400, "not_a_folder")
	e.failed(t, e.js(http.MethodPost, "/drive/folders", `{"name":"x","parent":"`+idRO+`"}`), 403, "forbidden")
	e.failed(t, e.js(http.MethodPost, "/drive/folders", `{"name":"x","parent":"`+idTrD+`"}`), 403, "forbidden")
	assert.Equal(t, 404, e.js(http.MethodPost, "/drive/folders", `{"name":"x","parent":"zzzzzzzzzz"}`).Code)
	assert.Len(t, e.fb.calls, n, "no folder was created by a refused request")
}

// ---- rename / star / move ---------------------------------------------------

func (e *driveEnv) patch(id, body string) *httptest.ResponseRecorder {
	return e.js(http.MethodPatch, "/drive/files/"+id, body)
}

func TestDrivePatchRenameAndStar(t *testing.T) {
	e := newWriteEnv(t)
	w := e.patch(idF, `{"name":"  new\u0000.pdf "}`)
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, "new.pdf", decode(t, w).Data["name"])
	assert.Equal(t, "new.pdf", *e.fb.lastUpd.Name)
	assert.Equal(t, idF, e.fb.lastUpdID)
	assert.Nil(t, e.fb.lastUpd.Starred)
	assert.Equal(t, []string{idB}, e.fb.files[idF].Parents, "a rename does not move")

	// empty and whitespace-only renames are rejected with invalid_name
	e.failed(t, e.patch(idF, `{"name":"   "}`), 400, "invalid_name")
	e.failed(t, e.patch(idF, `{"name":""}`), 400, "invalid_name")
	// 255 bytes is OK, 256+ is rejected
	require.Equal(t, 200, e.patch(idF, `{"name":"`+strings.Repeat("é", 127)+`"}`).Code)
	assert.LessOrEqual(t, len(*e.fb.lastUpd.Name), 255)
	e.failed(t, e.patch(idF, `{"name":"`+strings.Repeat("a", 256)+`"}`), 400, "invalid_name")

	w = e.patch(idF, `{"starred":true}`)
	require.Equal(t, 200, w.Code)
	assert.Equal(t, true, decode(t, w).Data["starred"])
	assert.Nil(t, e.fb.lastUpd.Name)
	w = e.patch(idF, `{"starred":false}`)
	require.Equal(t, 200, w.Code)
	require.NotNil(t, e.fb.lastUpd.Starred, "false must still be sent to Drive")
	assert.False(t, *e.fb.lastUpd.Starred)
	assert.Equal(t, false, decode(t, w).Data["starred"])

	// a starred-only change needs no rename right
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, Name: "f", MimeType: "application/pdf", Parents: []string{idB}}
	assert.Equal(t, 200, e.patch(idF, `{"starred":true}`).Code)
	e.failed(t, e.patch(idF, `{"name":"x"}`), 403, "forbidden")
}

func TestDrivePatchMove(t *testing.T) {
	e := newWriteEnv(t)
	n := len(e.fb.calls)

	// file B -> C: one request adds the new parent and removes the old one
	w := e.patch(idF, `{"parent":"`+idC+`"}`)
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, idC, e.fb.lastUpd.AddParent)
	assert.Equal(t, []string{idB}, e.fb.lastUpd.RemoveParents)
	assert.Equal(t, []string{idC}, e.fb.files[idF].Parents)
	assert.Len(t, e.fb.calls, n+1)

	// the root alias resolves to the real root id
	require.Equal(t, 200, e.patch(idF, `{"parent":"root"}`).Code)
	assert.Equal(t, "rootREAL001", e.fb.lastUpd.AddParent)
	assert.Equal(t, []string{idC}, e.fb.lastUpd.RemoveParents)

	// already there: nothing is sent
	n = len(e.fb.calls)
	w = e.patch(idF, `{"parent":"root"}`)
	assert.Equal(t, 200, w.Code)
	assert.Len(t, e.fb.calls, n)

	// several parents collapse into the target
	f := e.fb.files[idF]
	f.Parents = []string{idB, idA}
	e.fb.files[idF] = f
	require.Equal(t, 200, e.patch(idF, `{"parent":"`+idA+`"}`).Code)
	assert.Equal(t, "", e.fb.lastUpd.AddParent)
	assert.Equal(t, []string{idB}, e.fb.lastUpd.RemoveParents)

	// name + star + move are one Drive request
	n = len(e.fb.calls)
	require.Equal(t, 200, e.patch(idNew, `{"name":"m.txt","starred":true,"parent":"`+idA+`"}`).Code)
	assert.Len(t, e.fb.calls, n+1)
	assert.Equal(t, "m.txt", *e.fb.lastUpd.Name)
	assert.True(t, *e.fb.lastUpd.Starred)
	assert.Equal(t, idA, e.fb.lastUpd.AddParent)

	// folder B (inside A) can go up to the root
	require.Equal(t, 200, e.patch(idB, `{"parent":"root"}`).Code)
	assert.Equal(t, []string{idA}, e.fb.lastUpd.RemoveParents)
}

func TestDrivePatchMoveRefusals(t *testing.T) {
	e := newWriteEnv(t)
	// A holds B holds F: a folder cannot go into itself or any descendant
	e.failed(t, e.patch(idA, `{"parent":"`+idA+`"}`), 400, "invalid_move")
	e.failed(t, e.patch(idA, `{"parent":"`+idB+`"}`), 400, "invalid_move")
	e.fb.files["deepFolder01"] = googlecal.FileInfo{ID: "deepFolder01", Name: "D", MimeType: folderMimeG, Parents: []string{idB}, Caps: allCaps}
	e.failed(t, e.patch(idA, `{"parent":"deepFolder01"}`), 400, "invalid_move")
	e.failed(t, e.patch(idB, `{"parent":"deepFolder01"}`), 400, "invalid_move")
	assert.Empty(t, e.fb.calls, "refused before Drive was asked to change anything")

	// into a file / unreachable / read-only / trashed targets
	e.failed(t, e.patch(idNew, `{"parent":"`+idF+`"}`), 400, "not_a_folder")
	assert.Equal(t, 404, e.patch(idNew, `{"parent":"zzzzzzzzzz"}`).Code)
	e.failed(t, e.patch(idNew, `{"parent":"`+idRO+`"}`), 403, "forbidden")
	e.failed(t, e.patch(idNew, `{"parent":"`+idTrD+`"}`), 403, "forbidden")

	// the file itself must be movable / renamable, and live
	f := e.fb.files[idNew]
	f.Caps = googlecal.Caps{CanRename: true}
	e.fb.files[idNew] = f
	e.failed(t, e.patch(idNew, `{"parent":"`+idC+`"}`), 403, "forbidden")
	f.Caps = googlecal.Caps{CanMove: true}
	e.fb.files[idNew] = f
	e.failed(t, e.patch(idNew, `{"name":"x"}`), 403, "forbidden")
	e.failed(t, e.patch(idTrF, `{"name":"x"}`), 403, "forbidden")
	e.failed(t, e.patch(idTrF, `{"starred":true}`), 403, "forbidden")
	e.failed(t, e.patch(idTrF, `{"parent":"`+idC+`"}`), 403, "forbidden")
	assert.Empty(t, e.fb.calls)
}

// A target nested deeper than the path walker's cap cannot be proven safe, so
// moving into it is refused rather than guessed.
func TestDrivePatchMoveDepthGuard(t *testing.T) {
	e := newWriteEnv(t)
	prev := "rootREAL001"
	var ids []string
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("deepNODE%03d", i)
		e.fb.files[id] = googlecal.FileInfo{ID: id, Name: id, MimeType: folderMimeG, Parents: []string{prev}, Caps: allCaps}
		ids, prev = append(ids, id), id
	}
	e.failed(t, e.patch(ids[0], `{"parent":"`+ids[24]+`"}`), 400, "invalid_move")
	e.failed(t, e.patch(ids[3], `{"parent":"`+ids[10]+`"}`), 400, "invalid_move")
	assert.Empty(t, e.fb.calls)
	// a shallow target is fine
	assert.Equal(t, 200, e.patch(ids[24], `{"parent":"`+idC+`"}`).Code)
}

func TestDrivePatchRejectsBadInput(t *testing.T) {
	e := newWriteEnv(t)
	for _, body := range []string{`{}`, `{"trashed":true}`, `{"name":"x","description":"y"}`, `{"name":5}`, `{"starred":"yes"}`, ``, `nope`, `[]`,
		`{"name":"x"} trailing`} {
		assert.Equal(t, 400, e.patch(idF, body).Code, body)
	}
	e.failed(t, e.patch(idF, `{}`), 400, "no_changes")
	e.failed(t, e.patch(idF, `{"parent":""}`), 400, "invalid_id")
	e.failed(t, e.patch(idF, `{"parent":"bad id"}`), 400, "invalid_id")
	e.failed(t, e.patch("root", `{"name":"x"}`), 400, "invalid_id")
	e.failed(t, e.patch("short", `{"name":"x"}`), 400, "invalid_id")
	assert.Equal(t, 404, e.patch("zzzzzzzzzz", `{"name":"x"}`).Code)
	assert.Empty(t, e.fb.calls)
}

// ---- trash / restore / delete -----------------------------------------------

func (e *driveEnv) post(path string) *httptest.ResponseRecorder {
	return e.send(http.MethodPost, "/drive/files/"+path, nil)
}

func TestDriveTrashAndRestore(t *testing.T) {
	e := newWriteEnv(t)
	w := e.post(idF + "/trash")
	require.Equal(t, 200, w.Code, w.Body.String())
	got := decode(t, w).Data
	assert.Equal(t, true, got["trashed"])
	assert.Equal(t, false, got["capabilities"].(map[string]any)["can_star"], "a trashed file cannot be starred")
	require.NotNil(t, e.fb.lastUpd.Trashed)
	assert.True(t, *e.fb.lastUpd.Trashed)
	assert.True(t, e.fb.files[idF].Trashed)
	assert.Equal(t, []string{"update"}, e.fb.calls)

	// repeating is a no-op
	require.Equal(t, 200, e.post(idF+"/trash").Code)
	assert.Len(t, e.fb.calls, 1)

	w = e.post(idF + "/restore")
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, false, decode(t, w).Data["trashed"])
	require.NotNil(t, e.fb.lastUpd.Trashed, "restore must send trashed=false explicitly")
	assert.False(t, *e.fb.lastUpd.Trashed)
	assert.False(t, e.fb.files[idF].Trashed)
	require.Equal(t, 200, e.post(idF+"/restore").Code)
	assert.Len(t, e.fb.calls, 2)

	// the Sampah view restores a file it did not trash itself
	require.Equal(t, 200, e.post(idTrF+"/restore").Code)
	assert.False(t, e.fb.files[idTrF].Trashed)

	// trashing needs the capability
	f := e.fb.files[idNew]
	f.Caps = googlecal.Caps{CanRename: true, CanMove: true}
	e.fb.files[idNew] = f
	n := len(e.fb.calls)
	e.failed(t, e.post(idNew+"/trash"), 403, "forbidden")
	assert.Len(t, e.fb.calls, n)

	e.failed(t, e.post("root/trash"), 400, "invalid_id")
	e.failed(t, e.post("root/restore"), 400, "invalid_id")
	e.failed(t, e.post("short/trash"), 400, "invalid_id")
	assert.Equal(t, 404, e.post("zzzzzzzzzz/trash").Code)
	assert.Equal(t, 404, e.post("zzzzzzzzzz/restore").Code)
}

func (e *driveEnv) del(id string) *httptest.ResponseRecorder {
	return e.send(http.MethodDelete, "/drive/files/"+id, nil)
}

func TestDriveDeleteOnlyFromTrash(t *testing.T) {
	e := newWriteEnv(t)
	// a live file is never deleted
	e.failed(t, e.del(idF), 409, "not_in_trash")
	e.failed(t, e.del(idA), 409, "not_in_trash")
	assert.Empty(t, e.fb.calls, "Drive was not asked to delete")
	assert.Contains(t, e.fb.files, idF)

	w := e.del(idTrF)
	assert.Equal(t, 204, w.Code)
	assert.Empty(t, w.Body.String())
	assert.NotContains(t, e.fb.files, idTrF)
	assert.Equal(t, []string{"delete"}, e.fb.calls)

	// trashed, but Drive says the user may not delete it
	f := e.fb.files[idTrD]
	f.Caps = googlecal.Caps{CanTrash: true}
	e.fb.files[idTrD] = f
	e.failed(t, e.del(idTrD), 403, "forbidden")
	assert.Contains(t, e.fb.files, idTrD)

	// trash then delete works end to end
	require.Equal(t, 200, e.post(idNew+"/trash").Code)
	assert.Equal(t, 204, e.del(idNew).Code)
	assert.NotContains(t, e.fb.files, idNew)

	e.failed(t, e.del("root"), 400, "invalid_id")
	e.failed(t, e.del("short"), 400, "invalid_id")
	assert.Equal(t, 404, e.del("zzzzzzzzzz").Code)
	e.fb.writeErr = googlecal.ErrDriveForbidden
	f.Caps = allCaps
	e.fb.files[idTrD] = f
	e.failed(t, e.del(idTrD), 403, "forbidden")
}

// ---- listing ----------------------------------------------------------------

func TestDriveListTrashAndFoldersOnly(t *testing.T) {
	e := newWriteEnv(t)
	cases := []struct{ path, query, order string }{
		{"/drive/files?view=trash", "trashed=true and 'me' in owners", "modifiedTime desc"},
		{"/drive/files?view=trash&order=name&dir=asc", "trashed=true and 'me' in owners", "modifiedTime desc"},
		{"/drive/files?view=trash&q=cat", "(name contains 'cat' or fullText contains 'cat') and trashed=true and 'me' in owners", "modifiedTime desc"},
		{"/drive/files?folders_only=true", "'root' in parents and mimeType='application/vnd.google-apps.folder' and trashed=false", "folder,name"},
		{"/drive/files?folders_only=true&parent=" + idA, "'" + idA + "' in parents and mimeType='application/vnd.google-apps.folder' and trashed=false", "folder,name"},
		{"/drive/files?folders_only=false", "'root' in parents and trashed=false", "folder,name"},
		{"/drive/files?folders_only=true&q=x", "(name contains 'x' or fullText contains 'x') and mimeType='application/vnd.google-apps.folder' and trashed=false", "folder,name"},
		{"/drive/files?view=starred&folders_only=true", "starred=true and mimeType='application/vnd.google-apps.folder' and trashed=false", "folder,name"},
		{"/drive/files?view=trash&folders_only=true", "mimeType='application/vnd.google-apps.folder' and trashed=true and 'me' in owners", "modifiedTime desc"},
	}
	for _, tc := range cases {
		require.Equal(t, 200, e.get(tc.path).Code, tc.path)
		assert.Equal(t, tc.query, e.fb.lastList.Query, tc.path)
		assert.Equal(t, tc.order, e.fb.lastList.OrderBy, tc.path)
	}
	// only the trash view reaches trashed items
	for _, v := range []string{"mydrive", "recent", "starred", "shared"} {
		e.get("/drive/files?view=" + v)
		assert.NotContains(t, e.fb.lastList.Query, "trashed=true", v)
		assert.Contains(t, e.fb.lastList.Query, "trashed=false", v)
	}
	for _, bad := range []string{"folders_only=1", "folders_only=yes", "folders_only=TRUE", "view=bin"} {
		assert.Equal(t, 400, e.get("/drive/files?"+bad).Code, bad)
	}
	// trashed items are flagged in the list
	w := e.get("/drive/files?view=trash")
	assert.Contains(t, w.Body.String(), `"trashed":true`)
}

func TestDriveCapabilitiesMapping(t *testing.T) {
	e := newWriteEnv(t)
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, Name: "x", MimeType: "image/png", Caps: googlecal.Caps{CanRename: true, CanMove: true}}
	caps := `"capabilities":{"can_rename":true,"can_trash":false,"can_delete":false,"can_move":true,"can_add_children":false,"can_star":true}`
	for _, p := range []string{"/drive/files/" + idF, "/drive/files"} {
		body := e.get(p).Body.String()
		assert.Contains(t, body, caps, p)
		assert.Contains(t, body, `"trashed":false`, p)
	}
	assert.Contains(t, e.patch(idF, `{"starred":true}`).Body.String(), caps, "write responses carry them too")

	e.fb.files[idF] = googlecal.FileInfo{ID: idF, Name: "x", MimeType: "image/png", Trashed: true, Caps: allCaps}
	body := e.get("/drive/files/" + idF).Body.String()
	assert.Contains(t, body, `"trashed":true`)
	assert.Contains(t, body, `"capabilities":{"can_rename":true,"can_trash":true,"can_delete":true,"can_move":true,"can_add_children":true,"can_star":false}`)

	// no capabilities from Drive = nothing allowed
	e.fb.files[idF] = googlecal.FileInfo{ID: idF, Name: "x", MimeType: "image/png"}
	assert.Contains(t, e.get("/drive/files/"+idF).Body.String(),
		`"capabilities":{"can_rename":false,"can_trash":false,"can_delete":false,"can_move":false,"can_add_children":false,"can_star":true}`)
}

// ---- errors, auth, wiring ---------------------------------------------------

// writeCalls runs every write route once; restore and delete target trashed files.
func writeCalls(e *driveEnv) map[string]func() *httptest.ResponseRecorder {
	return map[string]func() *httptest.ResponseRecorder{
		"upload": func() *httptest.ResponseRecorder {
			return e.upload("/drive/files/upload", filePart("a.txt", "text/plain", "x"))
		},
		"folder":  func() *httptest.ResponseRecorder { return e.js(http.MethodPost, "/drive/folders", `{"name":"x"}`) },
		"patch":   func() *httptest.ResponseRecorder { return e.patch(idNew, `{"name":"x"}`) },
		"trash":   func() *httptest.ResponseRecorder { return e.post(idNew + "/trash") },
		"restore": func() *httptest.ResponseRecorder { return e.post(idTrF + "/restore") },
		"delete":  func() *httptest.ResponseRecorder { return e.del(idTrF) },
	}
}

func TestDriveWriteErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		code int
		body string
	}{
		{googlecal.ErrDriveForbidden, 403, `"error":"forbidden"`},
		{googlecal.ErrDriveQuota, 409, `"error":"quota_exceeded"`},
		{googlecal.ErrDriveScope, 409, "drive_scope_missing"},
		{googlecal.ErrDriveDisabled, 502, "drive_api_disabled"},
		{googlecal.ErrDriveNotFound, 404, ""},
		{errors.New("boom"), 502, "google_api_error"},
	}
	for _, tc := range cases {
		e := newWriteEnv(t)
		e.fb.writeErr = tc.err // Drive refuses the change itself
		for name, call := range writeCalls(e) {
			w := call()
			assert.Equal(t, tc.code, w.Code, fmt.Sprint(name, " ", tc.err))
			assert.Contains(t, w.Body.String(), tc.body, name)
		}
		e = newWriteEnv(t)
		e.fb.err = tc.err // Drive refuses even the lookup
		for name, call := range writeCalls(e) {
			assert.Equal(t, tc.code, call().Code, fmt.Sprint(name, " ", tc.err))
		}
	}
	for _, tc := range []struct {
		err  error
		body string
	}{{googlecal.ErrNotConnected, "not_connected"}, {googlecal.ErrNeedsReauth, "needs_reauth"}} {
		e := newWriteEnv(t)
		e.tokErr = tc.err
		for name, call := range writeCalls(e) {
			w := call()
			assert.Equal(t, 409, w.Code, name)
			assert.Contains(t, w.Body.String(), tc.body, name)
		}
		assert.Empty(t, e.fb.calls)
	}
}

func TestDriveWriteRoutesRequireAuth(t *testing.T) {
	e := newWriteEnv(t)
	for _, rt := range [][2]string{{"POST", "/drive/files/upload"}, {"POST", "/drive/folders"}, {"PATCH", "/drive/files/" + idF},
		{"POST", "/drive/files/" + idF + "/trash"}, {"POST", "/drive/files/" + idF + "/restore"}, {"DELETE", "/drive/files/" + idTrF}} {
		w := httptest.NewRecorder()
		e.r.ServeHTTP(w, httptest.NewRequest(rt[0], "/api/v1"+rt[1], strings.NewReader("{}")))
		assert.Equal(t, 401, w.Code, rt[0]+" "+rt[1])
	}
	assert.Empty(t, e.fb.calls)
}

func TestDriveWriteRoutesNotConfigured(t *testing.T) {
	r := gin.New()
	RegisterDriveRoutes(r.Group("/api/v1"), NewDriveHandler(nil))
	for _, rt := range [][2]string{{"POST", "/drive/files/upload"}, {"POST", "/drive/folders"}, {"PATCH", "/drive/files/" + idF},
		{"POST", "/drive/files/" + idF + "/trash"}, {"POST", "/drive/files/" + idF + "/restore"}, {"DELETE", "/drive/files/" + idTrF}} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(rt[0], "/api/v1"+rt[1], strings.NewReader("{}")))
		assert.Equal(t, 503, w.Code, rt[0]+" "+rt[1])
	}
}

// Names and contents are user data: no response may echo them back in an error.
func TestDriveErrorsDoNotEchoUserData(t *testing.T) {
	e := newWriteEnv(t)
	secret := "SECRET-name-123"
	for _, w := range []*httptest.ResponseRecorder{
		e.js(http.MethodPost, "/drive/folders", `{"name":"`+secret+`","extra":1}`),
		e.js(http.MethodPost, "/drive/folders", `{"name":"`+secret+`","parent":"bad id"}`),
		e.patch(idF, `{"name":"`+secret+`","trashed":true}`),
		e.upload("/drive/files/upload?parent="+idF, filePart(secret, "text/plain", secret)),
		e.upload("/drive/files/upload?parent="+idRO, filePart(secret, "text/plain", secret)),
	} {
		assert.GreaterOrEqual(t, w.Code, 400)
		assert.NotContains(t, w.Body.String(), secret)
	}
}

func TestDriveUploadBytesAreIntact(t *testing.T) {
	e := newWriteEnv(t)
	data := bytes.Repeat([]byte("0123456789abcdef"), 1<<14) // 256 KiB, several reads
	require.Equal(t, 201, e.upload("/drive/files/upload", mpPart{field: "file", filename: "d.bin", data: data}).Code)
	assert.Equal(t, int64(len(data)), e.fb.upBytes)
	assert.Equal(t, crc32.ChecksumIEEE(data), e.fb.upCRC)
	assert.Equal(t, data, e.fb.upBody)
}
