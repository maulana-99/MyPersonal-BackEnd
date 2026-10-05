package googlecal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

// ---- a local stand-in for the Drive API -----------------------------------------

type gdReq struct {
	Method, Path string
	Query        url.Values
	Header       http.Header
	Body         []byte
}

// gdrive serves just enough of Drive v3 (files.get/create/update/delete, token
// refresh, multipart and resumable uploads) to exercise the real client over
// loopback. It records every request and never sees the network.
type gdrive struct {
	srv *httptest.Server

	mu       sync.Mutex
	files    map[string]*drive.File
	reqs     []gdReq
	sessions int
	chunks   [][]byte
	ranges   []string
	finished int
	initMeta []byte
	fail     *googleapi.Error // answers the next upload/create/update with this error

	hold      func(*http.Request) bool // a matching request is left unanswered until the client goes away
	holding   chan struct{}            // closed when a held request arrived
	aborted   chan struct{}            // closed when the client dropped it
	firstSeen chan struct{}            // closed when the first resumable chunk arrived
	once      sync.Once
}

func newGDrive(t *testing.T) *gdrive {
	g := &gdrive{files: map[string]*drive.File{
		"root": {Id: "rootREAL001", Name: "My Drive", MimeType: folderMime,
			Capabilities: &drive.FileCapabilities{CanAddChildren: true}},
	}, holding: make(chan struct{}), aborted: make(chan struct{}), firstSeen: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		gjson(w, map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600})
	})
	mux.HandleFunc("POST /upload/drive/v3/files", g.start)
	mux.HandleFunc("POST /session/{n}", g.chunk)
	mux.HandleFunc("GET /drive/v3/files/{id}", g.get)
	mux.HandleFunc("POST /drive/v3/files", g.create)
	mux.HandleFunc("PATCH /drive/v3/files/{id}", g.patch)
	mux.HandleFunc("DELETE /drive/v3/files/{id}", g.del)
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		g.mu.Lock()
		g.reqs = append(g.reqs, gdReq{r.Method, r.URL.Path, r.URL.Query(), r.Header.Clone(), body})
		held := g.hold != nil && g.hold(r)
		g.mu.Unlock()
		if held {
			close(g.holding)
			<-r.Context().Done() // the client aborts: that is what the test waits for
			close(g.aborted)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { g.srv.CloseClientConnections(); g.srv.Close() }) // a held request must not hang the suite
	return g
}

func gjson(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func gerr(w http.ResponseWriter, e *googleapi.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Code)
	items := make([]map[string]string, 0, len(e.Errors))
	for _, it := range e.Errors {
		items = append(items, map[string]string{"reason": it.Reason, "message": e.Message, "domain": "global"})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": e.Code, "message": e.Message, "errors": items}})
}

// failing reports (and consumes) a queued error.
func (g *gdrive) failing(w http.ResponseWriter) bool {
	g.mu.Lock()
	e := g.fail
	g.fail = nil
	g.mu.Unlock()
	if e != nil {
		gerr(w, e)
	}
	return e != nil
}

func (g *gdrive) service() *FilesService {
	d := &realDrive{
		cfg:  &oauth2.Config{ClientID: "id", ClientSecret: "secret", Endpoint: oauth2.Endpoint{TokenURL: g.srv.URL + "/token"}},
		http: g.srv.Client(),
		base: g.srv.URL + "/drive/v3/",
	}
	return NewFilesServiceWithToken(func(context.Context, uuid.UUID) (string, error) { return "refresh-token", nil }, d)
}

// done stores and returns the file an upload or create produced.
func (g *gdrive) done(w http.ResponseWriter, meta []byte, mimeType string) {
	var f drive.File
	_ = json.Unmarshal(meta, &f)
	f.Id = "newFileID001"
	if f.MimeType == "" {
		f.MimeType = mimeType
	}
	f.Capabilities = &drive.FileCapabilities{CanRename: true, CanTrash: true, CanDelete: true, CanMoveItemWithinDrive: true}
	g.mu.Lock()
	g.files[f.Id] = &f
	g.finished++
	g.mu.Unlock()
	gjson(w, f)
}

func (g *gdrive) start(w http.ResponseWriter, r *http.Request) {
	if g.failing(w) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	switch r.URL.Query().Get("uploadType") {
	case "multipart":
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		meta, _ := mr.NextPart()
		metaBytes, _ := io.ReadAll(meta)
		media, _ := mr.NextPart()
		g.done(w, metaBytes, media.Header.Get("Content-Type"))
	case "resumable":
		g.mu.Lock()
		g.sessions++
		g.initMeta = body
		g.mu.Unlock()
		w.Header().Set("Location", g.srv.URL+"/session/1")
	default:
		http.Error(w, "unknown uploadType", 400)
	}
}

func (g *gdrive) chunk(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	cr := r.Header.Get("Content-Range")
	g.mu.Lock()
	g.chunks, g.ranges = append(g.chunks, body), append(g.ranges, cr)
	meta := g.initMeta
	g.mu.Unlock()
	g.once.Do(func() { close(g.firstSeen) })
	if !strings.HasSuffix(cr, "/*") { // the total is known: this was the last chunk
		g.done(w, meta, "application/octet-stream")
		return
	}
	w.Header().Set("X-Http-Status-Code-Override", "308")
	w.WriteHeader(http.StatusOK)
}

func (g *gdrive) get(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	f, ok := g.files[r.PathValue("id")]
	g.mu.Unlock()
	if !ok {
		gerr(w, &googleapi.Error{Code: 404, Message: "File not found", Errors: []googleapi.ErrorItem{{Reason: "notFound"}}})
		return
	}
	gjson(w, f)
}

func (g *gdrive) create(w http.ResponseWriter, r *http.Request) {
	if g.failing(w) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	g.done(w, body, "")
}

func (g *gdrive) patch(w http.ResponseWriter, r *http.Request) {
	if g.failing(w) {
		return
	}
	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	f := g.files[r.PathValue("id")]
	if f != nil {
		_ = json.Unmarshal(body, f)
	}
	g.mu.Unlock()
	gjson(w, f)
}

func (g *gdrive) del(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	delete(g.files, r.PathValue("id"))
	g.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// find returns the recorded requests with this method and path.
func (g *gdrive) find(method, path string) []gdReq {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []gdReq
	for _, r := range g.reqs {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// gateReader serves data but stalls at stall until open is closed, so a client
// that read ahead of what it had already sent upstream would block (and fail) here.
type gateReader struct {
	data  []byte
	off   int
	stall int
	open  <-chan struct{}
}

func (g *gateReader) Read(p []byte) (int, error) {
	if g.off >= len(g.data) {
		return 0, io.EOF
	}
	if g.off >= g.stall {
		select {
		case <-g.open:
		case <-time.After(5 * time.Second):
			return 0, errors.New("read ahead: the previous chunk was never sent")
		}
	} else if rem := g.stall - g.off; len(p) > rem {
		p = p[:rem]
	}
	n := copy(p, g.data[g.off:])
	g.off += n
	return n, nil
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

var testUser = uuid.New()

// ---- uploads through the real client --------------------------------------------

func TestRealUploadSmallFileIsOneRequest(t *testing.T) {
	g := newGDrive(t)
	info, err := g.service().Upload(context.Background(), testUser, UploadReq{
		Parent: "root", Name: "n.txt", MimeType: "text/plain", Body: strings.NewReader("hello"), Max: 1 << 20})
	require.NoError(t, err)
	assert.Equal(t, "newFileID001", info.ID)
	assert.Equal(t, "n.txt", info.Name)
	assert.True(t, info.Caps.CanRename && info.Caps.CanMove && info.Caps.CanTrash && info.Caps.CanDelete)

	reqs := g.find("POST", "/upload/drive/v3/files")
	require.Len(t, reqs, 1)
	assert.Equal(t, "multipart", reqs[0].Query.Get("uploadType"))
	assert.Equal(t, "Bearer at", reqs[0].Header.Get("Authorization"))
	assert.Contains(t, reqs[0].Query.Get("fields"), "capabilities(canRename,canTrash,canDelete,canMoveItemWithinDrive,canAddChildren)")
	assert.Contains(t, reqs[0].Query.Get("fields"), "trashed")
	_, params, err := mime.ParseMediaType(reqs[0].Header.Get("Content-Type"))
	require.NoError(t, err)
	mr := multipart.NewReader(bytes.NewReader(reqs[0].Body), params["boundary"])
	meta, err := mr.NextPart()
	require.NoError(t, err)
	metaBytes, _ := io.ReadAll(meta)
	assert.JSONEq(t, `{"name":"n.txt","parents":["rootREAL001"]}`, string(metaBytes))
	media, err := mr.NextPart()
	require.NoError(t, err)
	assert.Equal(t, "text/plain", media.Header.Get("Content-Type"))
	mediaBytes, _ := io.ReadAll(media)
	assert.Equal(t, "hello", string(mediaBytes))
}

// A big file goes up chunk by chunk, resumably, and the client never reads more
// of the source than it has already sent upstream plus one chunk.
func TestRealUploadStreamsChunksWithoutBufferingTheFile(t *testing.T) {
	g := newGDrive(t)
	data := pattern(uploadChunk + 1<<20) // 9 MiB: two chunks
	src := &gateReader{data: data, stall: uploadChunk, open: g.firstSeen}
	info, err := g.service().Upload(context.Background(), testUser, UploadReq{
		Parent: "root", Name: "big.bin", MimeType: "application/octet-stream", Body: src, Max: 64 << 20})
	require.NoError(t, err)
	assert.Equal(t, "newFileID001", info.ID)

	g.mu.Lock()
	defer g.mu.Unlock()
	assert.Equal(t, []string{"bytes 0-8388607/*", "bytes 8388608-9437183/9437184"}, g.ranges)
	all := bytes.Join(g.chunks, nil)
	assert.Equal(t, len(data), len(all))
	assert.Equal(t, crc32.ChecksumIEEE(data), crc32.ChecksumIEEE(all), "every byte arrives, in order")
	assert.Equal(t, 1, g.sessions)
	assert.Equal(t, 1, g.finished)
	assert.JSONEq(t, `{"name":"big.bin","parents":["rootREAL001"]}`, string(g.initMeta))
	init := g.reqs[len(g.reqs)-3] // session start, chunk, chunk
	assert.Equal(t, "resumable", init.Query.Get("uploadType"))
	assert.Equal(t, "application/octet-stream", init.Header.Get("X-Upload-Content-Type"))
}

func TestRealUploadStopsAtLimit(t *testing.T) {
	g := newGDrive(t)
	data := pattern(uploadChunk + 2<<20) // 10 MiB against a 9 MiB cap
	_, err := g.service().Upload(context.Background(), testUser, UploadReq{
		Parent: "root", Name: "big.bin", MimeType: "application/octet-stream", Body: bytes.NewReader(data), Max: uploadChunk + 1<<20})
	assert.ErrorIs(t, err, ErrTooLarge)

	g.mu.Lock()
	defer g.mu.Unlock()
	assert.Len(t, g.chunks, 1, "the chunk that crossed the cap was never sent")
	assert.Zero(t, g.finished, "Drive never got a completed file")
	assert.Len(t, g.files, 1, "only the root exists")
}

// Tiny cap: the very first chunk crosses it, so not a single file byte leaves.
func TestRealUploadOverLimitSendsNothing(t *testing.T) {
	g := newGDrive(t)
	_, err := g.service().Upload(context.Background(), testUser, UploadReq{
		Parent: "root", Name: "x", MimeType: "text/plain", Body: strings.NewReader("0123456789"), Max: 9})
	assert.ErrorIs(t, err, ErrTooLarge)
	_, err = g.service().Upload(context.Background(), testUser, UploadReq{
		Parent: "root", Name: "x", MimeType: "text/plain", Body: strings.NewReader("x"), Max: -1}) // a bad cap must not panic
	assert.ErrorIs(t, err, ErrTooLarge)
	g.mu.Lock()
	defer g.mu.Unlock()
	assert.Empty(t, g.chunks)
	assert.Zero(t, g.finished)
}

func TestRealUploadSourceFailure(t *testing.T) {
	g := newGDrive(t)
	src := io.MultiReader(bytes.NewReader(pattern(100<<10)), failingReader{io.ErrUnexpectedEOF})
	_, err := g.service().Upload(context.Background(), testUser, UploadReq{
		Parent: "root", Name: "x", MimeType: "text/plain", Body: src, Max: 1 << 20})
	assert.ErrorIs(t, err, ErrBadUpload)
	g.mu.Lock()
	defer g.mu.Unlock()
	assert.Zero(t, g.finished, "a broken body never becomes a file")
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

// The client leaving while Drive is still receiving a chunk aborts that
// upstream request instead of letting it run on.
func TestRealUploadAbortsUpstreamWhenClientLeaves(t *testing.T) {
	for name, tc := range map[string]struct {
		size int
		hold func(*http.Request) bool
	}{
		"resumable chunk": {uploadChunk + 1<<20, func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/session/") }},
		"single request":  {1 << 10, func(r *http.Request) bool { return r.URL.Query().Get("uploadType") == "multipart" }},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGDrive(t)
			g.hold = tc.hold
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			errc := make(chan error, 1)
			go func() {
				_, err := g.service().Upload(ctx, testUser, UploadReq{Parent: "root", Name: "x", MimeType: "text/plain",
					Body: bytes.NewReader(pattern(tc.size)), Max: 64 << 20})
				errc <- err
			}()
			select {
			case <-g.holding:
			case <-time.After(5 * time.Second):
				t.Fatal("the upload never reached Drive")
			}
			cancel() // what net/http does when the browser disconnects
			select {
			case err := <-errc:
				assert.ErrorIs(t, err, context.Canceled)
			case <-time.After(5 * time.Second):
				t.Fatal("Upload kept running after the client left")
			}
			select {
			case <-g.aborted:
			case <-time.After(5 * time.Second):
				t.Fatal("the upstream request was not aborted")
			}
			assert.Zero(t, g.finished)
		})
	}
}

func TestRealUploadMapsDriveErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		e    *googleapi.Error
		want error
	}{
		"quota":      {&googleapi.Error{Code: 403, Message: "The user's Drive storage quota has been exceeded.", Errors: []googleapi.ErrorItem{{Reason: "storageQuotaExceeded"}}}, ErrDriveQuota},
		"permission": {&googleapi.Error{Code: 403, Message: "The user does not have sufficient permissions for this file.", Errors: []googleapi.ErrorItem{{Reason: "insufficientFilePermissions"}}}, ErrDriveForbidden},
		"scope":      {&googleapi.Error{Code: 403, Message: "Insufficient Permission", Errors: []googleapi.ErrorItem{{Reason: "insufficientPermissions"}}}, ErrDriveScope},
		"disabled":   {&googleapi.Error{Code: 403, Message: "Drive API has not been used in project 1 before", Errors: []googleapi.ErrorItem{{Reason: "accessNotConfigured"}}}, ErrDriveDisabled},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGDrive(t)
			g.fail = tc.e
			_, err := g.service().Upload(context.Background(), testUser, UploadReq{
				Parent: "root", Name: "x", MimeType: "text/plain", Body: strings.NewReader("x"), Max: 1 << 20})
			assert.ErrorIs(t, err, tc.want)
		})
	}
}

// ---- metadata changes through the real client ------------------------------------

func fileWith(id, name, mt string, parents []string, c drive.FileCapabilities, trashed bool) *drive.File {
	return &drive.File{Id: id, Name: name, MimeType: mt, Parents: parents, Trashed: trashed, Capabilities: &c}
}

func TestRealUpdateSendsExactChanges(t *testing.T) {
	g := newGDrive(t)
	full := drive.FileCapabilities{CanRename: true, CanTrash: true, CanDelete: true, CanMoveItemWithinDrive: true, CanAddChildren: true}
	g.files["fileFFFFFF3"] = fileWith("fileFFFFFF3", "f.pdf", "application/pdf", []string{"folderBBBB2", "folderAAAA1"}, full, false)
	g.files["folderCCCC4"] = fileWith("folderCCCC4", "C", folderMime, []string{"rootREAL001"}, full, false)
	svc := g.service()

	name, star := "x.pdf", false
	_, err := svc.Update(context.Background(), testUser, "fileFFFFFF3", FilePatch{Name: &name, Starred: &star, Parent: "folderCCCC4"})
	require.NoError(t, err)
	reqs := g.find("PATCH", "/drive/v3/files/fileFFFFFF3")
	require.Len(t, reqs, 1)
	assert.Equal(t, "folderCCCC4", reqs[0].Query.Get("addParents"))
	assert.Equal(t, "folderBBBB2,folderAAAA1", reqs[0].Query.Get("removeParents"))
	assert.JSONEq(t, `{"name":"x.pdf","starred":false}`, string(reqs[0].Body), "false is sent, not dropped")
	assert.Equal(t, drive.FileCapabilities{}.CanRename, false)
	assert.Contains(t, reqs[0].Query.Get("fields"), "capabilities(")

	// trash and restore: trashed=false must reach Drive or "undo" does nothing
	_, err = svc.SetTrashed(context.Background(), testUser, "fileFFFFFF3", true)
	require.NoError(t, err)
	_, err = svc.SetTrashed(context.Background(), testUser, "fileFFFFFF3", false)
	require.NoError(t, err)
	reqs = g.find("PATCH", "/drive/v3/files/fileFFFFFF3")
	require.Len(t, reqs, 3)
	assert.JSONEq(t, `{"trashed":true}`, string(reqs[1].Body))
	assert.JSONEq(t, `{"trashed":false}`, string(reqs[2].Body))
	assert.Empty(t, reqs[2].Query.Get("addParents"))
	assert.Empty(t, reqs[2].Query.Get("removeParents"))
}

func TestRealCreateFolder(t *testing.T) {
	g := newGDrive(t)
	info, err := g.service().CreateFolder(context.Background(), testUser, "Reports", "root")
	require.NoError(t, err)
	assert.Equal(t, "Reports", info.Name)
	reqs := g.find("POST", "/drive/v3/files")
	require.Len(t, reqs, 1)
	assert.JSONEq(t, `{"name":"Reports","mimeType":"application/vnd.google-apps.folder","parents":["rootREAL001"]}`, string(reqs[0].Body))
}

func TestRealDeleteOnlyWhenTrashed(t *testing.T) {
	g := newGDrive(t)
	full := drive.FileCapabilities{CanDelete: true, CanTrash: true}
	g.files["liveFILE001"] = fileWith("liveFILE001", "a", "text/plain", []string{"rootREAL001"}, full, false)
	g.files["binFILE0001"] = fileWith("binFILE0001", "b", "text/plain", []string{"rootREAL001"}, full, true)
	svc := g.service()

	assert.ErrorIs(t, svc.Delete(context.Background(), testUser, "liveFILE001"), ErrNotInTrash)
	assert.Empty(t, g.find("DELETE", "/drive/v3/files/liveFILE001"), "a live file is never deleted")
	assert.NoError(t, svc.Delete(context.Background(), testUser, "binFILE0001"))
	assert.Len(t, g.find("DELETE", "/drive/v3/files/binFILE0001"), 1)

	// nothing in the client can empty the whole trash
	for _, r := range g.reqs {
		assert.NotContains(t, r.Path, "trash")
	}
}

// Moving a folder into its own subtree is refused before Drive hears about it.
func TestRealMoveIntoDescendantIsRefused(t *testing.T) {
	g := newGDrive(t)
	full := drive.FileCapabilities{CanRename: true, CanTrash: true, CanDelete: true, CanMoveItemWithinDrive: true, CanAddChildren: true}
	g.files["folderAAAA1"] = fileWith("folderAAAA1", "A", folderMime, []string{"rootREAL001"}, full, false)
	g.files["folderBBBB2"] = fileWith("folderBBBB2", "B", folderMime, []string{"folderAAAA1"}, full, false)
	svc := g.service()
	for _, dest := range []string{"folderAAAA1", "folderBBBB2"} {
		_, err := svc.Update(context.Background(), testUser, "folderAAAA1", FilePatch{Parent: dest})
		assert.ErrorIs(t, err, ErrInvalidMove, dest)
	}
	assert.Empty(t, g.find("PATCH", "/drive/v3/files/folderAAAA1"))
	_, err := svc.Update(context.Background(), testUser, "folderBBBB2", FilePatch{Parent: "root"})
	assert.NoError(t, err)
}

// ---- unit pieces -------------------------------------------------------------

type spyReader struct {
	r      io.Reader
	maxLen int
}

func (s *spyReader) Read(p []byte) (int, error) {
	s.maxLen = max(s.maxLen, len(p))
	return s.r.Read(p)
}

func TestLimitReader(t *testing.T) {
	newL := func(src io.Reader, left int64) (*limitReader, context.Context) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return &limitReader{r: src, left: left, ctx: ctx, cancel: cancel}, ctx
	}

	l, ctx := newL(bytes.NewReader(make([]byte, 100)), 100)
	b, err := io.ReadAll(l)
	assert.NoError(t, err, "exactly the cap passes")
	assert.Len(t, b, 100)
	assert.False(t, l.over)
	assert.NoError(t, ctx.Err())

	l, ctx = newL(bytes.NewReader(make([]byte, 101)), 100)
	_, err = io.ReadAll(l)
	assert.ErrorIs(t, err, ErrTooLarge, "one byte over fails")
	assert.True(t, l.over)
	assert.ErrorIs(t, ctx.Err(), context.Canceled, "and aborts the upstream request")
	_, err = l.Read(make([]byte, 4))
	assert.ErrorIs(t, err, ErrTooLarge, "and stays failed")

	// it never asks the source for much more than it may still pass
	spy := &spyReader{r: bytes.NewReader(make([]byte, 1000))}
	l, _ = newL(spy, 10)
	_, _ = io.ReadAll(l)
	assert.LessOrEqual(t, spy.maxLen, 11)

	// a zero cap passes an empty body only
	l, _ = newL(bytes.NewReader(nil), 0)
	_, err = io.ReadAll(l)
	assert.NoError(t, err)
	l, _ = newL(bytes.NewReader([]byte{1}), 0)
	_, err = io.ReadAll(l)
	assert.ErrorIs(t, err, ErrTooLarge)

	// the request body's own cap is the same thing
	l, ctx = newL(failingReader{&http.MaxBytesError{Limit: 5}}, 100)
	_, err = io.ReadAll(l)
	assert.ErrorIs(t, err, ErrTooLarge)
	assert.ErrorIs(t, ctx.Err(), context.Canceled)

	// a source failure is remembered, not mistaken for the cap
	l, ctx = newL(failingReader{io.ErrUnexpectedEOF}, 100)
	_, err = io.ReadAll(l)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	assert.ErrorIs(t, l.srcErr, io.ErrUnexpectedEOF)
	assert.False(t, l.over)

	// a finished context stops the stream without touching the source
	l, ctx = newL(failingReader{errors.New("must not be read")}, 100)
	l.cancel()
	_, err = l.Read(make([]byte, 4))
	assert.ErrorIs(t, err, context.Canceled)
	assert.NoError(t, l.srcErr)
	_ = ctx
}

func TestReparent(t *testing.T) {
	add, rm := reparent([]string{"a"}, "b")
	assert.Equal(t, "b", add)
	assert.Equal(t, []string{"a"}, rm)
	add, rm = reparent([]string{"a"}, "a")
	assert.Empty(t, add)
	assert.Empty(t, rm)
	add, rm = reparent([]string{"a", "b", "c"}, "b")
	assert.Empty(t, add)
	assert.Equal(t, []string{"a", "c"}, rm)
	add, rm = reparent(nil, "b")
	assert.Equal(t, "b", add)
	assert.Empty(t, rm)
}

func TestDriveErrWriteMappings(t *testing.T) {
	e := func(code int, msg string, reasons ...string) error {
		ge := &googleapi.Error{Code: code, Message: msg}
		for _, r := range reasons {
			ge.Errors = append(ge.Errors, googleapi.ErrorItem{Reason: r})
		}
		return ge
	}
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"insufficientFilePermissions": {e(403, "x", "insufficientFilePermissions"), ErrDriveForbidden},
		"cannotDeleteFile":            {e(403, "x", "cannotDeleteFile"), ErrDriveForbidden},
		"appNotAuthorizedToFile":      {e(403, "x", "appNotAuthorizedToFile"), ErrDriveForbidden},
		"domainPolicy":                {e(403, "x", "domainPolicy"), ErrDriveForbidden},
		"forbidden":                   {e(403, "Forbidden", "forbidden"), ErrDriveForbidden},
		"storageQuotaExceeded":        {e(403, "x", "storageQuotaExceeded"), ErrDriveQuota},
		"quota by message":            {e(403, "The user's Drive storage quota has been exceeded."), ErrDriveQuota},
		"scope reason":                {e(403, "x", "insufficientPermissions"), ErrDriveScope},
		"scope by message":            {e(403, "Request had insufficient authentication scopes."), ErrDriveScope},
		"api disabled":                {e(403, "x", "accessNotConfigured"), ErrDriveDisabled},
		"not found":                   {e(404, "File not found"), ErrDriveNotFound},
		"wrapped like a failed chunk": {fmt.Errorf("chunk upload failed after 3 attempts, final error: %w", e(403, "x", "storageQuotaExceeded")), ErrDriveQuota},
		"cancelled":                   {fmt.Errorf("send: %w", context.Canceled), context.Canceled},
	} {
		assert.ErrorIs(t, driveErr(tc.err), tc.want, name)
	}
	// rate limits are not permission errors
	for _, r := range []string{"rateLimitExceeded", "userRateLimitExceeded", "dailyLimitExceeded", "sharingRateLimitExceeded"} {
		got := driveErr(e(403, "slow down", r))
		assert.NotErrorIs(t, got, ErrDriveForbidden, r)
		assert.NotErrorIs(t, got, ErrDriveQuota, r)
	}
	assert.NotErrorIs(t, driveErr(e(500, "boom")), ErrDriveForbidden)
}
