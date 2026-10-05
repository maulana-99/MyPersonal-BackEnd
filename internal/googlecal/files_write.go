package googlecal

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
)

// uploadChunk is the only upload buffer: the Drive client keeps one chunk in
// memory (a multiple of 256 KiB) and sends the file resumably, so a 200 MB
// upload is never held whole in memory or written to disk.
const uploadChunk = 8 << 20

// ErrInvalidMove (tasks.go) also reports a folder moved into itself or one of
// its descendants.
var (
	ErrNotFolder  = errors.New("not_a_folder")
	ErrNotInTrash = errors.New("not_in_trash") // permanent delete is for trashed files only
	ErrTooLarge   = errors.New("too_large")
	ErrBadUpload  = errors.New("invalid_upload") // the request body ended or broke before the file did
)

// UploadInput is a streamed Drive upload; Body is read once, chunk by chunk.
type UploadInput struct {
	Name, Parent, MimeType string
	Body                   io.Reader
}

// FileUpdate changes only the parts that are set. AddParent and RemoveParents
// move the file inside the same request.
type FileUpdate struct {
	Name             *string
	Starred, Trashed *bool
	AddParent        string
	RemoveParents    []string
}

func (u FileUpdate) empty() bool {
	return u.Name == nil && u.Starred == nil && u.Trashed == nil && u.AddParent == "" && len(u.RemoveParents) == 0
}

func (d *realDrive) UploadFile(ctx context.Context, rt string, in UploadInput) (FileInfo, error) {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return FileInfo{}, driveErr(err)
	}
	f, err := svc.Files.Create(&drive.File{Name: in.Name, Parents: []string{in.Parent}}).
		Media(in.Body, googleapi.ContentType(in.MimeType), googleapi.ChunkSize(uploadChunk)).
		Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return FileInfo{}, driveErr(err)
	}
	return fromFile(f), nil
}

func (d *realDrive) CreateFolder(ctx context.Context, rt, name, parent string) (FileInfo, error) {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return FileInfo{}, driveErr(err)
	}
	f, err := svc.Files.Create(&drive.File{Name: name, MimeType: folderMime, Parents: []string{parent}}).
		Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return FileInfo{}, driveErr(err)
	}
	return fromFile(f), nil
}

func (d *realDrive) UpdateFile(ctx context.Context, rt, id string, u FileUpdate) (FileInfo, error) {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return FileInfo{}, driveErr(err)
	}
	meta := &drive.File{}
	if u.Name != nil {
		meta.Name = *u.Name
	}
	// false is the zero value and would be dropped from the body: force it.
	if u.Starred != nil {
		meta.Starred = *u.Starred
		meta.ForceSendFields = append(meta.ForceSendFields, "Starred")
	}
	if u.Trashed != nil {
		meta.Trashed = *u.Trashed
		meta.ForceSendFields = append(meta.ForceSendFields, "Trashed")
	}
	call := svc.Files.Update(id, meta).Fields(fileFields).Context(ctx)
	if u.AddParent != "" {
		call = call.AddParents(u.AddParent)
	}
	if len(u.RemoveParents) > 0 {
		call = call.RemoveParents(strings.Join(u.RemoveParents, ","))
	}
	f, err := call.Do()
	if err != nil {
		return FileInfo{}, driveErr(err)
	}
	return fromFile(f), nil
}

func (d *realDrive) DeleteFile(ctx context.Context, rt, id string) error {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return driveErr(err)
	}
	return driveErr(svc.Files.Delete(id).Context(ctx).Do())
}

// limitReader caps an upload body. Going over the cap, or a done context, ends
// the stream and cancels the upstream request, so Drive never receives a
// finished file; a failing source is remembered so the caller can tell it from
// a Drive failure.
type limitReader struct {
	r      io.Reader
	left   int64
	ctx    context.Context
	cancel context.CancelFunc
	over   bool  // the cap was exceeded
	srcErr error // the source itself failed
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.over {
		return 0, ErrTooLarge
	}
	if err := l.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > l.left+1 { // one byte past the cap is enough to notice it
		p = p[:l.left+1]
	}
	n, err := l.r.Read(p)
	var big *http.MaxBytesError // the request body's own cap counts as too large too
	if l.left -= int64(n); l.left < 0 || errors.As(err, &big) {
		l.over = true
		l.cancel()
		return 0, ErrTooLarge
	}
	if err != nil && err != io.EOF {
		l.srcErr = err
	}
	return n, err
}

// UploadReq is a streamed upload: Body is cut off after Max bytes.
type UploadReq struct {
	Parent, Name, MimeType string
	Body                   io.Reader
	Max                    int64
}

// info is GetFile with the service's error mapping.
func (f *FilesService) info(ctx context.Context, userID uuid.UUID, rt, id string) (FileInfo, error) {
	fi, err := f.d.GetFile(ctx, rt, id)
	return fi, f.failIf(ctx, userID, err)
}

// destination resolves the folder that receives a file: the "root" alias, or a
// live folder the user may add children to.
func (f *FilesService) destination(ctx context.Context, userID uuid.UUID, rt, parent string) (FileInfo, error) {
	d, err := f.info(ctx, userID, rt, parent)
	switch {
	case err != nil:
		return FileInfo{}, err
	case d.MimeType != folderMime:
		return FileInfo{}, ErrNotFolder
	case parent != "root" && (d.Trashed || !d.Caps.CanAddChildren):
		return FileInfo{}, ErrDriveForbidden
	}
	return d, nil
}

// Upload streams r.Body into a new file under r.Parent. The destination is
// checked before a byte is read; the body is never buffered whole.
func (f *FilesService) Upload(ctx context.Context, userID uuid.UUID, r UploadReq) (FileInfo, error) {
	rt, err := f.rt(ctx, userID)
	if err != nil {
		return FileInfo{}, err
	}
	dest, err := f.destination(ctx, userID, rt, r.Parent)
	if err != nil {
		return FileInfo{}, err
	}
	up, cancel := context.WithCancel(ctx) // cancelled on every exit: no upstream request outlives the call
	defer cancel()
	body := &limitReader{r: r.Body, left: max(r.Max, 0), ctx: up, cancel: cancel}
	info, err := f.d.UploadFile(up, rt, UploadInput{Name: r.Name, Parent: dest.ID, MimeType: r.MimeType, Body: body})
	switch {
	case ctx.Err() != nil:
		return FileInfo{}, driveErr(ctx.Err())
	case body.over:
		return FileInfo{}, ErrTooLarge
	case body.srcErr != nil:
		return FileInfo{}, ErrBadUpload
	}
	return info, f.failIf(ctx, userID, err)
}

func (f *FilesService) CreateFolder(ctx context.Context, userID uuid.UUID, name, parent string) (FileInfo, error) {
	rt, err := f.rt(ctx, userID)
	if err != nil {
		return FileInfo{}, err
	}
	dest, err := f.destination(ctx, userID, rt, parent)
	if err != nil {
		return FileInfo{}, err
	}
	out, err := f.d.CreateFolder(ctx, rt, name, dest.ID)
	return out, f.failIf(ctx, userID, err)
}

// FilePatch is a partial update; unset parts stay as they are. Parent is a
// folder id or "root" ("" = no move).
type FilePatch struct {
	Name    *string
	Starred *bool
	Parent  string
}

// Update renames, stars and/or moves a file in one Drive request. Every part
// must be allowed by the capabilities Drive reports; trashed files are
// read-only until restored.
func (f *FilesService) Update(ctx context.Context, userID uuid.UUID, id string, p FilePatch) (FileInfo, error) {
	rt, err := f.rt(ctx, userID)
	if err != nil {
		return FileInfo{}, err
	}
	cur, err := f.info(ctx, userID, rt, id)
	if err != nil {
		return FileInfo{}, err
	}
	if cur.Trashed || p.Name != nil && !cur.Caps.CanRename || p.Parent != "" && !cur.Caps.CanMove {
		return FileInfo{}, ErrDriveForbidden
	}
	u := FileUpdate{Name: p.Name, Starred: p.Starred}
	if p.Parent != "" {
		dest, err := f.destination(ctx, userID, rt, p.Parent)
		if err != nil {
			return FileInfo{}, err
		}
		if cur.MimeType == folderMime {
			if err := f.noCycle(ctx, userID, cur.ID, dest.ID); err != nil {
				return FileInfo{}, err
			}
		}
		u.AddParent, u.RemoveParents = reparent(cur.Parents, dest.ID)
	}
	if u.empty() {
		return cur, nil
	}
	out, err := f.d.UpdateFile(ctx, rt, id, u)
	return out, f.failIf(ctx, userID, err)
}

// noCycle refuses to move folder id into itself or one of its descendants: the
// target's ancestor chain (the path walker) must not contain it. A chain cut at
// the depth cap proves nothing, so it is refused as well.
func (f *FilesService) noCycle(ctx context.Context, userID uuid.UUID, id, dest string) error {
	chain, err := f.Path(ctx, userID, dest)
	if err != nil {
		return err
	}
	if len(chain) >= maxPathDepth && chain[0].ID != "root" {
		return ErrInvalidMove
	}
	for _, c := range chain {
		if c.ID == id {
			return ErrInvalidMove
		}
	}
	return nil
}

// reparent turns "move under dst" into addParents/removeParents: add dst unless
// the file is already there, and drop every other current parent.
func reparent(parents []string, dst string) (add string, remove []string) {
	has := false
	for _, p := range parents {
		if p == dst {
			has = true
		} else {
			remove = append(remove, p)
		}
	}
	if !has {
		add = dst
	}
	return add, remove
}

// SetTrashed moves a file to the trash or restores it. Repeating it is a no-op.
// Drive itself decides whether the user may restore.
func (f *FilesService) SetTrashed(ctx context.Context, userID uuid.UUID, id string, trashed bool) (FileInfo, error) {
	rt, err := f.rt(ctx, userID)
	if err != nil {
		return FileInfo{}, err
	}
	cur, err := f.info(ctx, userID, rt, id)
	switch {
	case err != nil:
		return FileInfo{}, err
	case cur.Trashed == trashed:
		return cur, nil
	case trashed && !cur.Caps.CanTrash:
		return FileInfo{}, ErrDriveForbidden
	}
	out, err := f.d.UpdateFile(ctx, rt, id, FileUpdate{Trashed: &trashed})
	return out, f.failIf(ctx, userID, err)
}

// Delete removes a file for good, and only when it is already in the trash.
func (f *FilesService) Delete(ctx context.Context, userID uuid.UUID, id string) error {
	rt, err := f.rt(ctx, userID)
	if err != nil {
		return err
	}
	cur, err := f.info(ctx, userID, rt, id)
	switch {
	case err != nil:
		return err
	case !cur.Trashed:
		return ErrNotInTrash
	case !cur.Caps.CanDelete:
		return ErrDriveForbidden
	}
	return f.failIf(ctx, userID, f.d.DeleteFile(ctx, rt, id))
}
