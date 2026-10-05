// Package googlecal implements Google Calendar sync. All Google I/O goes
// through the Client interface so tests never touch the network.
package googlecal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

var (
	ErrInvalidGrant     = errors.New("invalid_grant")      // refresh token revoked/expired
	ErrSyncTokenExpired = errors.New("sync token expired") // HTTP 410 on list
)

// Event is the app-side view of a Google event.
type Event struct {
	ID          string
	ETag        string
	Cancelled   bool
	RecurringID string // parent series id for instances
	Title       string
	Description string
	Start       time.Time
	End         *time.Time // all-day: exclusive end date at 00:00 UTC
	AllDay      bool
	Recurrence  string // bare RRULE body, no "RRULE:" prefix

	Location  string
	ColorID   string // "1".."11"; empty = calendar default
	TimeZone  string // IANA name; empty = UTC
	Reminders []int  // popup minutes; empty = Google default
	Attendees []Attendee
	MeetLink  string // read-only, from hangoutLink

	// Write-only hints for push/patch.
	AddMeet     bool   // create a Meet conference
	RemoveMeet  bool   // clear the conference
	SendUpdates string // all | none (Google sendUpdates)
	Instance    bool   // single instance of a series: never send a recurrence
}

// Attendee is a guest of an event.
type Attendee struct {
	Email          string
	DisplayName    string
	ResponseStatus string // needsAction | accepted | declined | tentative
	Organizer      bool
}

// Identity is the Google account behind a login, read from the ID token.
// RefreshToken is empty when Google did not issue one.
type Identity struct {
	Sub           string
	Email         string
	Name          string
	Picture       string
	EmailVerified bool
	RefreshToken  string
}

// Result is what Google returns for a write.
type Result struct {
	ID, ETag, MeetLink string
}

// ListResult is a full (all pages) events listing.
type ListResult struct {
	Events        []Event
	NextSyncToken string
}

// Client is the Google surface the app needs. Refresh tokens are passed per
// call; implementations exchange them for access tokens internally.
type Client interface {
	AuthURL(state string) string
	LoginURL(state string) string
	ExchangeLogin(ctx context.Context, code string) (Identity, error)
	Exchange(ctx context.Context, code string) (refreshToken, email string, err error)
	ListEvents(ctx context.Context, refreshToken, syncToken string) (ListResult, error)
	InsertEvent(ctx context.Context, refreshToken string, e Event) (Result, error)
	PatchEvent(ctx context.Context, refreshToken, id string, e Event) (Result, error)
	DeleteEvent(ctx context.Context, refreshToken, id, sendUpdates string) error // 404/410 are not errors
	Revoke(ctx context.Context, token string) error
}

type realClient struct {
	cfg   *oauth2.Config
	login *oauth2.Config
	http  *http.Client
}

// NewClient builds the real Google client.
func NewClient(clientID, secret, redirectURL, loginRedirectURL string) Client {
	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: secret,
		RedirectURL:  redirectURL,
		Endpoint:     google.Endpoint,
		Scopes:       []string{calendar.CalendarEventsScope, driveScope, tasksScope, "openid", "email"},
	}
	login := *cfg
	login.RedirectURL = loginRedirectURL
	login.Scopes = []string{"openid", "email", "profile", calendar.CalendarEventsScope, driveScope, tasksScope}
	return &realClient{cfg: cfg, login: &login, http: &http.Client{Timeout: 20 * time.Second}}
}

func (c *realClient) LoginURL(state string) string {
	return c.login.AuthCodeURL(state, oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent select_account"))
}

func (c *realClient) ExchangeLogin(ctx context.Context, code string) (Identity, error) {
	tok, err := c.login.Exchange(c.ctx(ctx), code)
	if err != nil {
		return Identity{}, mapErr(err)
	}
	raw, _ := tok.Extra("id_token").(string)
	id := claimsFromIDToken(raw)
	id.RefreshToken = tok.RefreshToken
	return id, nil
}

func (c *realClient) AuthURL(state string) string {
	return c.cfg.AuthCodeURL(state, oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent select_account"))
}

func (c *realClient) ctx(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauth2.HTTPClient, c.http)
}

func (c *realClient) Exchange(ctx context.Context, code string) (string, string, error) {
	tok, err := c.cfg.Exchange(c.ctx(ctx), code)
	if err != nil {
		return "", "", mapErr(err)
	}
	raw, _ := tok.Extra("id_token").(string)
	return tok.RefreshToken, emailFromIDToken(raw), nil
}

// emailFromIDToken reads the email claim. The token comes straight from
// Google's token endpoint over TLS, so the signature is not re-verified.
func emailFromIDToken(raw string) string { return claimsFromIDToken(raw).Email }

func claimsFromIDToken(raw string) Identity {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return Identity{}
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Identity{}
	}
	var cl struct {
		Sub           string `json:"sub"`
		Email         string `json:"email"`
		Name          string `json:"name"`
		Picture       string `json:"picture"`
		EmailVerified bool   `json:"email_verified"`
	}
	_ = json.Unmarshal(b, &cl)
	return Identity{Sub: cl.Sub, Email: cl.Email, Name: cl.Name, Picture: cl.Picture, EmailVerified: cl.EmailVerified}
}

func (c *realClient) service(ctx context.Context, refreshToken string) (*calendar.Service, error) {
	ts := c.cfg.TokenSource(c.ctx(ctx), &oauth2.Token{RefreshToken: refreshToken})
	return calendar.NewService(ctx, option.WithHTTPClient(oauth2.NewClient(c.ctx(ctx), ts)))
}

func (c *realClient) ListEvents(ctx context.Context, rt, syncToken string) (ListResult, error) {
	svc, err := c.service(ctx, rt)
	if err != nil {
		return ListResult{}, mapErr(err)
	}
	var out ListResult
	page := ""
	for {
		call := svc.Events.List("primary").SingleEvents(true).MaxResults(250).Context(ctx)
		if syncToken != "" {
			call = call.SyncToken(syncToken)
		}
		if page != "" {
			call = call.PageToken(page)
		}
		res, err := call.Do()
		if err != nil {
			return ListResult{}, mapErr(err)
		}
		for _, ev := range res.Items {
			if e, ok := fromAPI(ev); ok {
				out.Events = append(out.Events, e)
			}
		}
		if res.NextPageToken == "" {
			out.NextSyncToken = res.NextSyncToken
			return out, nil
		}
		page = res.NextPageToken
	}
}

func (c *realClient) InsertEvent(ctx context.Context, rt string, e Event) (Result, error) {
	svc, err := c.service(ctx, rt)
	if err != nil {
		return Result{}, mapErr(err)
	}
	call := svc.Events.Insert("primary", toAPI(e)).ConferenceDataVersion(confVersion(e)).Context(ctx)
	if e.SendUpdates != "" {
		call = call.SendUpdates(e.SendUpdates)
	}
	res, err := call.Do()
	if err != nil {
		return Result{}, mapErr(err)
	}
	return Result{ID: res.Id, ETag: res.Etag, MeetLink: res.HangoutLink}, nil
}

func (c *realClient) PatchEvent(ctx context.Context, rt, id string, e Event) (Result, error) {
	svc, err := c.service(ctx, rt)
	if err != nil {
		return Result{}, mapErr(err)
	}
	call := svc.Events.Patch("primary", id, toAPI(e)).ConferenceDataVersion(confVersion(e)).Context(ctx)
	if e.SendUpdates != "" {
		call = call.SendUpdates(e.SendUpdates)
	}
	res, err := call.Do()
	if err != nil {
		return Result{}, mapErr(err)
	}
	return Result{ID: res.Id, ETag: res.Etag, MeetLink: res.HangoutLink}, nil
}

func (c *realClient) DeleteEvent(ctx context.Context, rt, id, sendUpdates string) error {
	svc, err := c.service(ctx, rt)
	if err != nil {
		return mapErr(err)
	}
	call := svc.Events.Delete("primary", id).Context(ctx)
	if sendUpdates != "" {
		call = call.SendUpdates(sendUpdates)
	}
	err = call.Do()
	var ge *googleapi.Error
	if errors.As(err, &ge) && (ge.Code == http.StatusNotFound || ge.Code == http.StatusGone) {
		return nil
	}
	return mapErr(err)
}

func (c *realClient) Revoke(ctx context.Context, token string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/revoke",
		strings.NewReader(url.Values{"token": {token}}.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return errors.New("revoke request failed")
	}
	resp.Body.Close()
	return nil
}

// mapErr turns library errors into sentinels and strips anything that could
// carry a token or secret.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.ErrorCode == "invalid_grant" {
		return ErrInvalidGrant
	}
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		if ge.Code == http.StatusGone {
			return ErrSyncTokenExpired
		}
		// Message/Reason come from Google's error body (e.g. accessNotConfigured); they never contain tokens.
		reason := ""
		if len(ge.Errors) > 0 {
			reason = ge.Errors[0].Reason
		}
		msg := ge.Message
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("google api status %s (%d) reason=%s: %s", http.StatusText(ge.Code), ge.Code, reason, msg)
	}
	if strings.Contains(err.Error(), "invalid_grant") {
		return ErrInvalidGrant
	}
	return errors.New("google request failed")
}
