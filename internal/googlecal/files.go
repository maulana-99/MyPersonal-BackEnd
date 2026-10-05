package googlecal

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	drive "google.golang.org/api/drive/v3"
)

const fileFields = "id,name,mimeType,description,parents,size,modifiedTime,createdTime,starred,ownedByMe,shared,trashed," +
	"owners(displayName),thumbnailLink,webViewLink,iconLink,shortcutDetails(targetId,targetMimeType)," +
	"capabilities(canRename,canTrash,canDelete,canMoveItemWithinDrive,canAddChildren)"

// Caps is what Drive says the current user may do with a file.
type Caps struct{ CanRename, CanTrash, CanDelete, CanMove, CanAddChildren bool }

// FileInfo is the browser-side view of a Drive file. Links that embed
// credentials (webContentLink) are never requested.
type FileInfo struct {
	ID, Name, Description, MimeType string
	Parents                         []string
	Size                            *int64
	Modified, Created               time.Time
	Starred, OwnedByMe, Shared      bool
	Trashed                         bool
	Caps                            Caps
	OwnerName                       *string
	ThumbnailLink                   string // internal: proxied, never returned
	WebViewLink, IconLink           string
	ShortcutTargetID, ShortcutMime  string
}

type FilesListOpts struct {
	Query, OrderBy, PageToken string
	PageSize                  int
}

type FileList struct {
	Files         []FileInfo
	NextPageToken string
}

// Stream is an open upstream body; the caller closes it.
type Stream struct {
	Body         io.ReadCloser
	Status       int
	ContentType  string
	Length       int64 // -1 when unknown
	ContentRange string
}

type About struct {
	Name, Email, Photo                  string
	Limit, Usage, UsageInDrive, InTrash int64 // Limit 0 = unlimited
}

// DriveBrowser is the Drive surface of the file browser (fakeable). Missing or
// unreadable files are reported as ErrDriveNotFound. The write half has no
// emptyTrash on purpose: that would wipe files unrelated to this app.
type DriveBrowser interface {
	ListFiles(ctx context.Context, rt string, o FilesListOpts) (FileList, error)
	GetFile(ctx context.Context, rt, id string) (FileInfo, error)
	Download(ctx context.Context, rt, id, rangeHeader string) (*Stream, error)
	Export(ctx context.Context, rt, id, mime string) (*Stream, error)
	Thumbnail(ctx context.Context, rt, link string) (*Stream, error)
	About(ctx context.Context, rt string) (About, error)

	UploadFile(ctx context.Context, rt string, in UploadInput) (FileInfo, error)
	CreateFolder(ctx context.Context, rt, name, parent string) (FileInfo, error)
	UpdateFile(ctx context.Context, rt, id string, u FileUpdate) (FileInfo, error)
	DeleteFile(ctx context.Context, rt, id string) error // permanent; callers check it is trashed
}

func fromFile(f *drive.File) FileInfo {
	out := FileInfo{ID: f.Id, Name: f.Name, Description: f.Description, MimeType: f.MimeType, Parents: f.Parents,
		Starred: f.Starred, OwnedByMe: f.OwnedByMe, Shared: f.Shared, Trashed: f.Trashed, ThumbnailLink: f.ThumbnailLink,
		WebViewLink: f.WebViewLink, IconLink: f.IconLink}
	if c := f.Capabilities; c != nil {
		out.Caps = Caps{c.CanRename, c.CanTrash, c.CanDelete, c.CanMoveItemWithinDrive, c.CanAddChildren}
	}
	if !strings.HasPrefix(f.MimeType, "application/vnd.google-apps.") { // folders, shortcuts, Docs: no size
		sz := f.Size
		out.Size = &sz
	}
	if len(f.Owners) > 0 {
		n := f.Owners[0].DisplayName
		out.OwnerName = &n
	}
	if f.ShortcutDetails != nil {
		out.ShortcutTargetID, out.ShortcutMime = f.ShortcutDetails.TargetId, f.ShortcutDetails.TargetMimeType
	}
	out.Created, _ = time.Parse(time.RFC3339, f.CreatedTime)
	out.Modified, _ = time.Parse(time.RFC3339, f.ModifiedTime)
	return out
}

func (d *realDrive) ListFiles(ctx context.Context, rt string, o FilesListOpts) (FileList, error) {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return FileList{}, driveErr(err)
	}
	call := svc.Files.List().Q(o.Query).OrderBy(o.OrderBy).PageSize(int64(o.PageSize)).
		Fields("nextPageToken,files(" + fileFields + ")").Context(ctx)
	if o.PageToken != "" {
		call = call.PageToken(o.PageToken)
	}
	res, err := call.Do()
	if err != nil {
		return FileList{}, driveErr(err)
	}
	out := FileList{NextPageToken: res.NextPageToken}
	for _, f := range res.Files {
		out.Files = append(out.Files, fromFile(f))
	}
	return out, nil
}

func (d *realDrive) GetFile(ctx context.Context, rt, id string) (FileInfo, error) {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return FileInfo{}, driveErr(err)
	}
	f, err := svc.Files.Get(id).Fields(fileFields).Context(ctx).Do()
	if err != nil {
		return FileInfo{}, driveErr(err)
	}
	return fromFile(f), nil
}

func toStream(resp *http.Response) *Stream {
	return &Stream{Body: resp.Body, Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"),
		Length: resp.ContentLength, ContentRange: resp.Header.Get("Content-Range")}
}

func (d *realDrive) Download(ctx context.Context, rt, id, rangeHeader string) (*Stream, error) {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return nil, driveErr(err)
	}
	call := svc.Files.Get(id).Context(ctx)
	if rangeHeader != "" {
		call.Header().Set("Range", rangeHeader)
	}
	resp, err := call.Download()
	if err != nil {
		return nil, driveErr(err)
	}
	return toStream(resp), nil
}

func (d *realDrive) Export(ctx context.Context, rt, id, mime string) (*Stream, error) {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return nil, driveErr(err)
	}
	resp, err := svc.Files.Export(id, mime).Context(ctx).Download()
	if err != nil {
		return nil, driveErr(err)
	}
	return toStream(resp), nil
}

// Thumbnail fetches a thumbnailLink with the user's token. The link comes from
// Drive metadata; only https Google hosts are followed.
func (d *realDrive) Thumbnail(ctx context.Context, rt, link string) (*Stream, error) {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || !(strings.HasSuffix(u.Hostname(), ".googleusercontent.com") ||
		strings.HasSuffix(u.Hostname(), ".google.com")) {
		return nil, ErrDriveNotFound
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrDriveNotFound
	}
	resp, err := d.client(ctx, rt).Do(req)
	if err != nil {
		return nil, mapErr(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, ErrDriveNotFound
	}
	return toStream(resp), nil
}

func (d *realDrive) About(ctx context.Context, rt string) (About, error) {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return About{}, driveErr(err)
	}
	a, err := svc.About.Get().Fields("user(displayName,emailAddress,photoLink),storageQuota(limit,usage,usageInDrive,usageInDriveTrash)").
		Context(ctx).Do()
	if err != nil {
		return About{}, driveErr(err)
	}
	out := About{}
	if a.User != nil {
		out.Name, out.Email, out.Photo = a.User.DisplayName, a.User.EmailAddress, a.User.PhotoLink
	}
	if q := a.StorageQuota; q != nil {
		out.Limit, out.Usage, out.UsageInDrive, out.InTrash = q.Limit, q.Usage, q.UsageInDrive, q.UsageInDriveTrash
	}
	return out, nil
}

// FilesService is the user-scoped file browser: it resolves the user's
// refresh token and maps Drive failures onto the app's sentinels.
type FilesService struct {
	rt       func(ctx context.Context, userID uuid.UUID) (string, error)
	classify func(ctx context.Context, userID uuid.UUID, err error) error
	d        DriveBrowser
}

func NewFilesService(s *Service, d DriveBrowser) *FilesService {
	return &FilesService{rt: s.rt, classify: s.classify, d: d}
}

// NewFilesServiceWithToken serves tests and tools that bring their own token.
func NewFilesServiceWithToken(rt func(context.Context, uuid.UUID) (string, error), d DriveBrowser) *FilesService {
	return &FilesService{rt: rt, d: d,
		classify: func(context.Context, uuid.UUID, error) error { return ErrAPI }}
}

func (f *FilesService) fail(ctx context.Context, userID uuid.UUID, err error) error {
	switch {
	case errors.Is(err, ErrDriveNotFound), errors.Is(err, ErrDriveScope), errors.Is(err, ErrDriveDisabled),
		errors.Is(err, ErrDriveForbidden), errors.Is(err, ErrDriveQuota),
		errors.Is(err, ErrNotConnected), errors.Is(err, ErrNeedsReauth), errors.Is(err, ErrAPI),
		errors.Is(err, context.Canceled):
		return err
	}
	return f.classify(ctx, userID, err)
}

// Query escapes a value for a Drive `q` string literal.
func Query(v string) string { return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'" }

func (f *FilesService) List(ctx context.Context, userID uuid.UUID, o FilesListOpts) (FileList, error) {
	rt, err := f.rt(ctx, userID)
	if err != nil {
		return FileList{}, err
	}
	res, err := f.d.ListFiles(ctx, rt, o)
	return res, f.failIf(ctx, userID, err)
}

func (f *FilesService) failIf(ctx context.Context, userID uuid.UUID, err error) error {
	if err == nil {
		return nil
	}
	return f.fail(ctx, userID, err)
}

func (f *FilesService) Get(ctx context.Context, userID uuid.UUID, id string) (FileInfo, error) {
	rt, err := f.rt(ctx, userID)
	if err != nil {
		return FileInfo{}, err
	}
	res, err := f.d.GetFile(ctx, rt, id)
	return res, f.failIf(ctx, userID, err)
}

// Crumb is one breadcrumb segment.
type Crumb struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

const (
	maxPathDepth = 20
	RootName     = "Drive Saya"
)

// Path returns the chain from "root" down to the item itself (walking
// parents[0], at most 20 levels, cycle-safe). A chain cut short by missing
// access (shared items) is returned partial, without the root segment.
func (f *FilesService) Path(ctx context.Context, userID uuid.UUID, id string) ([]Crumb, error) {
	rt, err := f.rt(ctx, userID)
	if err != nil {
		return nil, err
	}
	root, err := f.d.GetFile(ctx, rt, "root")
	if err != nil {
		return nil, f.fail(ctx, userID, err)
	}
	if id == "root" || id == root.ID {
		return []Crumb{{"root", RootName}}, nil
	}
	cur, err := f.d.GetFile(ctx, rt, id)
	if err != nil {
		return nil, f.fail(ctx, userID, err)
	}
	var rev []Crumb
	seen := map[string]bool{}
	for depth := 0; ; depth++ {
		if cur.ID == root.ID {
			rev = append(rev, Crumb{"root", RootName})
			break
		}
		if seen[cur.ID] {
			break
		}
		seen[cur.ID] = true
		rev = append(rev, Crumb{cur.ID, cur.Name})
		if len(cur.Parents) == 0 || depth >= maxPathDepth-1 {
			break
		}
		next, err := f.d.GetFile(ctx, rt, cur.Parents[0])
		if err != nil {
			if errors.Is(err, ErrDriveNotFound) {
				break // no access to the ancestor: partial chain
			}
			return nil, f.fail(ctx, userID, err)
		}
		cur = next
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

func (f *FilesService) Thumbnail(ctx context.Context, userID uuid.UUID, id string) (*Stream, error) {
	rt, err := f.rt(ctx, userID)
	if err != nil {
		return nil, err
	}
	info, err := f.d.GetFile(ctx, rt, id)
	if err != nil {
		return nil, f.fail(ctx, userID, err)
	}
	if info.ThumbnailLink == "" {
		return nil, ErrDriveNotFound
	}
	s, err := f.d.Thumbnail(ctx, rt, info.ThumbnailLink)
	if err != nil {
		return nil, f.fail(ctx, userID, err)
	}
	return s, nil
}

// Content opens the bytes of a file (export != "" exports a Google-native doc).
// info comes from Get; the caller has already decided what may be served.
func (f *FilesService) Content(ctx context.Context, userID uuid.UUID, id, export, rangeHeader string) (*Stream, error) {
	rt, err := f.rt(ctx, userID)
	if err != nil {
		return nil, err
	}
	var s *Stream
	if export != "" {
		s, err = f.d.Export(ctx, rt, id, export)
	} else {
		s, err = f.d.Download(ctx, rt, id, rangeHeader)
	}
	if err != nil {
		return nil, f.fail(ctx, userID, err)
	}
	return s, nil
}

func (f *FilesService) About(ctx context.Context, userID uuid.UUID) (About, error) {
	rt, err := f.rt(ctx, userID)
	if err != nil {
		return About{}, err
	}
	a, err := f.d.About(ctx, rt)
	return a, f.failIf(ctx, userID, err)
}

// Require reports ErrNotConnected / ErrNeedsReauth unless Drive can be used.
func (f *FilesService) Require(ctx context.Context, userID uuid.UUID) error {
	_, err := f.rt(ctx, userID)
	return err
}
