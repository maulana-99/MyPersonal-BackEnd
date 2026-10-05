package googlecal

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	drive "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const (
	notesMime  = "text/markdown"
	folderMime = "application/vnd.google-apps.folder"
	driveScope = drive.DriveScope // full Drive (restricted): notes + file browser
	driveField = "id,name,description,appProperties,parents,trashed,createdTime,modifiedTime"
	maxBody    = 1 << 20
)

var (
	ErrDriveNotFound  = errors.New("drive_not_found")
	ErrDriveScope     = errors.New("drive_scope_missing") // grant lacks the drive scope: sign in again
	ErrDriveDisabled  = errors.New("drive_api_disabled")  // Drive API off in the Cloud project
	ErrDriveForbidden = errors.New("drive_forbidden")     // the user may not do this to this file
	ErrDriveQuota     = errors.New("drive_quota_exceeded")
)

// DriveFile is the app-side view of a Drive file or folder.
type DriveFile struct {
	ID, Name, Description, MimeType string
	Props                           map[string]string
	Parents                         []string
	Trashed                         bool
	Created, Modified               time.Time
}

type DriveListOpts struct {
	Query, PageToken string
	PageSize         int
}

type DriveList struct {
	Files         []DriveFile
	NextPageToken string
}

// DriveUpdate changes only the non-nil/non-empty parts. Props keys are merged.
type DriveUpdate struct {
	Name, Description, Body *string
	Props                   map[string]string
	Trashed                 *bool
}

// DriveClient is the Drive surface the app needs. Refresh tokens are passed
// per call, like Client. Get/Update/Delete report a missing file as ErrDriveNotFound.
type DriveClient interface {
	List(ctx context.Context, rt string, o DriveListOpts) (DriveList, error)
	Get(ctx context.Context, rt, id string) (DriveFile, error)
	Read(ctx context.Context, rt, id string) (string, error)
	Create(ctx context.Context, rt string, f DriveFile, body *string) (DriveFile, error)
	Update(ctx context.Context, rt, id string, u DriveUpdate) (DriveFile, error)
	Delete(ctx context.Context, rt, id string) error
}

// DriveAPI is everything the real Drive client offers.
type DriveAPI interface {
	DriveClient
	DriveBrowser
}

type realDrive struct {
	cfg  *oauth2.Config
	http *http.Client
	base string // Drive API endpoint override; tests point it at a local server
}

// NewDrive builds the real Drive client (refresh-token flow only). It serves
// both the notes surface (DriveClient) and the file browser (DriveBrowser).
func NewDrive(clientID, secret string) DriveAPI {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &realDrive{
		cfg: &oauth2.Config{ClientID: clientID, ClientSecret: secret, Endpoint: google.Endpoint, Scopes: []string{driveScope}},
		// No overall timeout: downloads stream; the request context bounds them.
		http: &http.Client{Transport: tr},
	}
}

// client is an HTTP client that authenticates with the user's refresh token.
func (d *realDrive) client(ctx context.Context, rt string) *http.Client {
	hc := context.WithValue(ctx, oauth2.HTTPClient, d.http)
	return oauth2.NewClient(hc, d.cfg.TokenSource(hc, &oauth2.Token{RefreshToken: rt}))
}

func (d *realDrive) service(ctx context.Context, rt string) (*drive.Service, error) {
	opts := []option.ClientOption{option.WithHTTPClient(d.client(ctx, rt))}
	if d.base != "" {
		opts = append(opts, option.WithEndpoint(d.base))
	}
	return drive.NewService(ctx, opts...)
}

// driveErr maps Drive failures onto sentinels; anything else goes through mapErr.
func driveErr(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled // the caller went away: not a Drive failure, and nothing to log
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusNotFound:
			return ErrDriveNotFound
		case http.StatusForbidden:
			msg := strings.ToLower(ge.Message)
			for _, e := range ge.Errors {
				switch e.Reason {
				case "accessNotConfigured":
					return ErrDriveDisabled
				case "insufficientPermissions", "insufficientScopes":
					return ErrDriveScope
				case "storageQuotaExceeded":
					return ErrDriveQuota
				case "insufficientFilePermissions", "appNotAuthorizedToFile", "cannotDeleteFile", "domainPolicy", "forbidden":
					return ErrDriveForbidden
				}
			}
			switch {
			case strings.Contains(msg, "has not been used") || strings.Contains(msg, "is disabled"):
				return ErrDriveDisabled
			case strings.Contains(msg, "storage quota"):
				return ErrDriveQuota
			case strings.Contains(msg, "scope"), strings.Contains(msg, "insufficient"):
				return ErrDriveScope
			}
		}
	}
	return mapErr(err)
}

func fromDrive(f *drive.File) DriveFile {
	out := DriveFile{ID: f.Id, Name: f.Name, Description: f.Description, MimeType: f.MimeType,
		Props: f.AppProperties, Parents: f.Parents, Trashed: f.Trashed}
	out.Created, _ = time.Parse(time.RFC3339, f.CreatedTime)
	out.Modified, _ = time.Parse(time.RFC3339, f.ModifiedTime)
	return out
}

func (d *realDrive) List(ctx context.Context, rt string, o DriveListOpts) (DriveList, error) {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return DriveList{}, driveErr(err)
	}
	call := svc.Files.List().Q(o.Query).OrderBy("modifiedTime desc").PageSize(int64(o.PageSize)).
		Fields("nextPageToken,files(" + driveField + ")").Context(ctx)
	if o.PageToken != "" {
		call = call.PageToken(o.PageToken)
	}
	res, err := call.Do()
	if err != nil {
		return DriveList{}, driveErr(err)
	}
	out := DriveList{NextPageToken: res.NextPageToken}
	for _, f := range res.Files {
		out.Files = append(out.Files, fromDrive(f))
	}
	return out, nil
}

func (d *realDrive) Get(ctx context.Context, rt, id string) (DriveFile, error) {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return DriveFile{}, driveErr(err)
	}
	f, err := svc.Files.Get(id).Fields(driveField).Context(ctx).Do()
	if err != nil {
		return DriveFile{}, driveErr(err)
	}
	return fromDrive(f), nil
}

func (d *realDrive) Read(ctx context.Context, rt, id string) (string, error) {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return "", driveErr(err)
	}
	resp, err := svc.Files.Get(id).Context(ctx).Download()
	if err != nil {
		return "", driveErr(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", errors.New("google request failed")
	}
	return string(b), nil
}

func (d *realDrive) Create(ctx context.Context, rt string, f DriveFile, body *string) (DriveFile, error) {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return DriveFile{}, driveErr(err)
	}
	call := svc.Files.Create(&drive.File{Name: f.Name, Description: f.Description, MimeType: f.MimeType,
		AppProperties: f.Props, Parents: f.Parents}).Fields(driveField).Context(ctx)
	if body != nil {
		call = call.Media(strings.NewReader(*body), googleapi.ContentType(notesMime))
	}
	res, err := call.Do()
	if err != nil {
		return DriveFile{}, driveErr(err)
	}
	return fromDrive(res), nil
}

func (d *realDrive) Update(ctx context.Context, rt, id string, u DriveUpdate) (DriveFile, error) {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return DriveFile{}, driveErr(err)
	}
	meta := &drive.File{AppProperties: u.Props}
	if u.Name != nil {
		meta.Name = *u.Name
	}
	if u.Description != nil {
		meta.Description = *u.Description
		meta.ForceSendFields = append(meta.ForceSendFields, "Description")
	}
	if u.Trashed != nil {
		meta.Trashed = *u.Trashed
		meta.ForceSendFields = append(meta.ForceSendFields, "Trashed")
	}
	call := svc.Files.Update(id, meta).Fields(driveField).Context(ctx)
	if u.Body != nil {
		call = call.Media(strings.NewReader(*u.Body), googleapi.ContentType(notesMime))
	}
	res, err := call.Do()
	if err != nil {
		return DriveFile{}, driveErr(err)
	}
	return fromDrive(res), nil
}

func (d *realDrive) Delete(ctx context.Context, rt, id string) error {
	svc, err := d.service(ctx, rt)
	if err != nil {
		return driveErr(err)
	}
	return driveErr(svc.Files.Delete(id).Context(ctx).Do())
}
