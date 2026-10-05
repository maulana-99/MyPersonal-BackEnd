package googlecal

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotConnected = errors.New("not_connected")
	ErrNeedsReauth  = errors.New("needs_reauth")
	ErrAPI          = errors.New("google_api_error")
)

// Callback failure reasons (fixed codes, safe to put in a redirect URL).
const (
	ReasonDenied   = "denied"
	ReasonState    = "state"
	ReasonExchange = "exchange"
	ReasonInternal = "internal"
)

// Service owns the integration row, importer and pusher.
type Service struct {
	db          *pgxpool.Pool
	client      Client
	key         []byte
	stateSecret string
	// Go runs background work; tests replace it with a synchronous runner.
	Go func(func())
}

func NewService(db *pgxpool.Pool, client Client, tokenKey []byte, stateSecret string) *Service {
	return &Service{db: db, client: client, key: tokenKey, stateSecret: stateSecret,
		Go: func(f func()) { go f() }}
}

type Stats struct {
	Imported int `json:"imported"`
	Updated  int `json:"updated"`
	Deleted  int `json:"deleted"`
}

type Status struct {
	Configured   bool       `json:"configured"`
	Connected    bool       `json:"connected"`
	Email        *string    `json:"email"`
	Status       *string    `json:"status"`
	LastSyncedAt *time.Time `json:"last_synced_at"`
	LastError    *string    `json:"last_error"`
	// LocalOnlyCount counts local schedules never sent to Google (see UploadLocal).
	LocalOnlyCount int `json:"local_only_count"`
}

// UploadStats is the outcome of UploadLocal.
type UploadStats struct {
	Uploaded int `json:"uploaded"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
}

// ---- connect / callback / status / disconnect ------------------------------

func (s *Service) ConnectURL(userID uuid.UUID) (string, error) {
	state, err := SignState(s.stateSecret, userID, StateTTL)
	if err != nil {
		return "", err
	}
	return s.client.AuthURL(state), nil
}

// HandleCallback verifies state first, then exchanges the code. It returns ""
// on success or one of the Reason* codes.
func (s *Service) HandleCallback(ctx context.Context, code, state string) string {
	userID, err := VerifyState(s.stateSecret, state)
	if err != nil {
		return ReasonState
	}
	if code == "" {
		return ReasonDenied
	}
	rt, email, err := s.client.Exchange(ctx, code)
	if err != nil || rt == "" {
		return ReasonExchange
	}
	enc, err := Encrypt(s.key, []byte(rt))
	if err != nil {
		return ReasonInternal
	}
	if _, err := s.db.Exec(ctx, `
		INSERT INTO integrations_google (user_id, google_email, refresh_token_enc)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET
			google_email = EXCLUDED.google_email, refresh_token_enc = EXCLUDED.refresh_token_enc,
			sync_token = NULL, status = 'connected', last_error = NULL, updated_at = NOW()
	`, userID, email, enc); err != nil {
		return ReasonInternal
	}
	s.SyncBackground(userID)
	return ""
}

// SyncBackground runs an incremental sync detached from the request.
func (s *Service) SyncBackground(userID uuid.UUID) {
	s.Go(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := s.Sync(c, userID); err != nil {
			log.Printf("google background sync user=%s: %v", userID, err)
		}
	})
}

// ---- login ------------------------------------------------------------------

func nonceHash(nonce string) string {
	h := sha256.Sum256([]byte(nonce))
	return hex.EncodeToString(h[:])
}

// LoginStart returns a fresh nonce (for the g_nonce cookie) and the Google
// consent URL whose signed state embeds the nonce hash.
func (s *Service) LoginStart() (nonce, authURL string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	nonce = base64.RawURLEncoding.EncodeToString(b)
	state, err := signState(s.stateSecret, loginStatePurpose, nonceHash(nonce), StateTTL)
	if err != nil {
		return "", "", err
	}
	return nonce, s.client.LoginURL(state), nil
}

// LoginExchange verifies state AND the nonce cookie value (login CSRF), then
// exchanges the code. reason is "" on success, else a Reason* code.
func (s *Service) LoginExchange(ctx context.Context, code, state, nonce string) (id Identity, reason string) {
	want, err := verifyState(s.stateSecret, loginStatePurpose, state)
	if err != nil || nonce == "" || subtle.ConstantTimeCompare([]byte(want), []byte(nonceHash(nonce))) != 1 {
		return id, ReasonState
	}
	if code == "" {
		return id, ReasonDenied
	}
	id, err = s.client.ExchangeLogin(ctx, code)
	if err != nil || id.Sub == "" || id.Email == "" || !id.EmailVerified {
		return Identity{}, ReasonExchange
	}
	return id, ""
}

// SaveGrant stores the (encrypted) refresh token from a login and marks the
// integration connected. An empty rt keeps the stored token and fails with
// ErrNotConnected when none exists. The sync token survives
// unless the Google account changed.
func (s *Service) SaveGrant(ctx context.Context, userID uuid.UUID, email, rt string) error {
	if rt == "" {
		tag, err := s.db.Exec(ctx, `UPDATE integrations_google SET google_email = $2,
			status = 'connected', last_error = NULL, updated_at = NOW() WHERE user_id = $1`, userID, email)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotConnected
		}
		return nil
	}
	enc, err := Encrypt(s.key, []byte(rt))
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO integrations_google (user_id, google_email, refresh_token_enc)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id) DO UPDATE SET
			sync_token = CASE WHEN integrations_google.google_email = EXCLUDED.google_email
			                  THEN integrations_google.sync_token END,
			google_email = EXCLUDED.google_email, refresh_token_enc = EXCLUDED.refresh_token_enc,
			status = 'connected', last_error = NULL, updated_at = NOW()
	`, userID, email, enc)
	return err
}

// Require reports ErrNotConnected / ErrNeedsReauth unless the user has a usable
// Google connection. Schedule writes call it before touching anything.
func (s *Service) Require(ctx context.Context, userID uuid.UUID) error {
	_, err := s.rt(ctx, userID)
	return err
}

func (s *Service) Status(ctx context.Context, userID uuid.UUID) (Status, error) {
	st := Status{Configured: true}
	err := s.db.QueryRow(ctx, `
		SELECT google_email, status, last_synced_at, last_error
		FROM integrations_google WHERE user_id = $1
	`, userID).Scan(&st.Email, &st.Status, &st.LastSyncedAt, &st.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	} else {
		st.Connected = err == nil
	}
	if err != nil {
		return st, err
	}
	err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM schedules
		WHERE user_id = $1 AND source = 'local' AND google_event_id IS NULL`, userID).Scan(&st.LocalOnlyCount)
	return st, err
}

// Disconnect revokes the token (best effort), removes imported schedules and
// unlinks pushed ones.
func (s *Service) Disconnect(ctx context.Context, userID uuid.UUID) error {
	if rt, err := s.refreshToken(ctx, userID); err == nil {
		_ = s.client.Revoke(ctx, rt)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, q := range []string{
		`DELETE FROM schedules WHERE user_id = $1 AND source = 'google'`,
		`UPDATE schedules SET google_event_id = NULL, google_etag = NULL,
			google_sync_state = NULL, google_sync_error = NULL
			WHERE user_id = $1 AND source = 'local'`,
		`DELETE FROM integrations_google WHERE user_id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, userID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// refreshToken loads and decrypts the user's token. ErrNotConnected when no
// row; ErrNeedsReauth when the integration is flagged.
func (s *Service) refreshToken(ctx context.Context, userID uuid.UUID) (string, error) {
	var enc []byte
	var status string
	err := s.db.QueryRow(ctx, `SELECT refresh_token_enc, status FROM integrations_google WHERE user_id = $1`,
		userID).Scan(&enc, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotConnected
	}
	if err != nil {
		return "", err
	}
	if status != "connected" {
		return "", ErrNeedsReauth
	}
	pt, err := Decrypt(s.key, enc)
	if err != nil {
		return "", errors.New("token decrypt failed")
	}
	return string(pt), nil
}

// classify maps a Google client error to an app sentinel, flagging the
// integration when the grant is gone.
func (s *Service) classify(ctx context.Context, userID uuid.UUID, err error) error {
	if errors.Is(err, ErrInvalidGrant) {
		_, _ = s.db.Exec(ctx, `UPDATE integrations_google
			SET status = 'needs_reauth', last_error = 'needs_reauth', updated_at = NOW()
			WHERE user_id = $1`, userID)
		return ErrNeedsReauth
	}
	log.Printf("google api error user=%s: %v", userID, err)
	return ErrAPI
}

// ---- import ----------------------------------------------------------------

// Sync imports changes for one user (incremental, full on 410).
func (s *Service) Sync(ctx context.Context, userID uuid.UUID) (Stats, error) {
	var st Stats
	rt, err := s.refreshToken(ctx, userID)
	if err != nil {
		return st, err
	}
	var token *string
	if err := s.db.QueryRow(ctx, `SELECT sync_token FROM integrations_google WHERE user_id = $1`, userID).Scan(&token); err != nil {
		return st, err
	}
	tok := ""
	if token != nil {
		tok = *token
	}

	res, err := s.client.ListEvents(ctx, rt, tok)
	if errors.Is(err, ErrSyncTokenExpired) && tok != "" {
		res, err = s.client.ListEvents(ctx, rt, "")
	}
	if err != nil {
		err = s.classify(ctx, userID, err)
		if err == ErrAPI {
			_, _ = s.db.Exec(ctx, `UPDATE integrations_google SET last_error = 'google_api_error', updated_at = NOW() WHERE user_id = $1`, userID)
		}
		return st, err
	}

	for _, e := range res.Events {
		if err := s.apply(ctx, userID, e, &st); err != nil {
			return st, err
		}
	}
	_, err = s.db.Exec(ctx, `
		UPDATE integrations_google
		SET sync_token = NULLIF($2, ''), last_synced_at = NOW(), last_error = NULL, updated_at = NOW()
		WHERE user_id = $1`, userID, res.NextSyncToken)
	return st, err
}

// apply upserts or deletes one event. For rows pushed from the app
// (source='local') only the etag is refreshed, which breaks the echo loop.
func (s *Service) apply(ctx context.Context, userID uuid.UUID, e Event, st *Stats) error {
	if e.Cancelled {
		tag, err := s.db.Exec(ctx, `DELETE FROM schedules WHERE user_id = $1 AND google_event_id = $2`, userID, e.ID)
		if err == nil && tag.RowsAffected() > 0 {
			st.Deleted++
		}
		return err
	}
	if e.RecurringID != "" {
		// Instance of a recurring event pushed from the app: the series row
		// already represents it, importing would duplicate it.
		var n int
		if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM schedules
			WHERE user_id = $1 AND google_event_id = $2 AND source = 'local'`, userID, e.RecurringID).Scan(&n); err != nil || n > 0 {
			return err
		}
	}
	title := e.Title
	if title == "" {
		title = "(no title)"
	}
	tz := e.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	var id uuid.UUID
	var inserted bool
	var source string
	err := s.db.QueryRow(ctx, `
		INSERT INTO schedules (user_id, title, description, start_time, end_time, all_day,
		                       source, google_event_id, google_etag, google_sync_state,
		                       location, color_id, meet_link, tz)
		VALUES ($1, $2, $3, $4, $5, $6, 'google', $7, $8, 'synced',
		        NULLIF($9, ''), NULLIF($10, ''), NULLIF($11, ''), $12)
		ON CONFLICT (user_id, google_event_id) WHERE google_event_id IS NOT NULL DO UPDATE SET
			google_etag = EXCLUDED.google_etag,
			title       = CASE WHEN schedules.source = 'google' THEN EXCLUDED.title       ELSE schedules.title END,
			description = CASE WHEN schedules.source = 'google' THEN EXCLUDED.description ELSE schedules.description END,
			start_time  = CASE WHEN schedules.source = 'google' THEN EXCLUDED.start_time  ELSE schedules.start_time END,
			end_time    = CASE WHEN schedules.source = 'google' THEN EXCLUDED.end_time    ELSE schedules.end_time END,
			all_day     = CASE WHEN schedules.source = 'google' THEN EXCLUDED.all_day     ELSE schedules.all_day END,
			location    = CASE WHEN schedules.source = 'google' THEN EXCLUDED.location    ELSE schedules.location END,
			color_id    = CASE WHEN schedules.source = 'google' THEN EXCLUDED.color_id    ELSE schedules.color_id END,
			tz          = CASE WHEN schedules.source = 'google' THEN EXCLUDED.tz          ELSE schedules.tz END,
			-- Meet links can appear asynchronously after a push, so local rows take them too.
			meet_link   = CASE WHEN schedules.source = 'google' THEN EXCLUDED.meet_link
			                   ELSE COALESCE(EXCLUDED.meet_link, schedules.meet_link) END,
			updated_at  = NOW()
		WHERE schedules.google_etag IS DISTINCT FROM EXCLUDED.google_etag
		RETURNING id, (xmax = 0), source
	`, userID, title, e.Description, e.Start, e.End, e.AllDay, e.ID, e.ETag,
		e.Location, e.ColorID, e.MeetLink, tz).Scan(&id, &inserted, &source)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // etag unchanged
	}
	if err != nil {
		return err
	}
	if err := s.applyGuests(ctx, id, source == "google", e); err != nil {
		return err
	}
	if inserted {
		st.Imported++
	} else {
		st.Updated++
	}
	return nil
}

// applyGuests refreshes attendees (and, for imported rows, reminders) from
// Google. Pushed rows only get guest responses updated: the app owns their set.
func (s *Service) applyGuests(ctx context.Context, id uuid.UUID, imported bool, e Event) error {
	if !imported {
		for _, a := range e.Attendees {
			if _, err := s.db.Exec(ctx, `UPDATE schedule_attendees
				SET response_status = $3, display_name = COALESCE(NULLIF($4, ''), display_name)
				WHERE schedule_id = $1 AND email = $2`,
				id, a.Email, statusOr(a.ResponseStatus), a.DisplayName); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM schedule_attendees WHERE schedule_id = $1`, id); err != nil {
		return err
	}
	for _, a := range e.Attendees {
		if _, err := s.db.Exec(ctx, `INSERT INTO schedule_attendees
			(schedule_id, email, display_name, response_status, is_organizer)
			VALUES ($1, $2, NULLIF($3, ''), $4, $5) ON CONFLICT DO NOTHING`,
			id, a.Email, a.DisplayName, statusOr(a.ResponseStatus), a.Organizer); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM reminders WHERE schedule_id = $1`, id); err != nil {
		return err
	}
	for _, m := range e.Reminders {
		fireAt := e.Start.Add(-time.Duration(m) * time.Minute)
		// Reminders already in the past are stored as sent so they never fire late.
		if _, err := s.db.Exec(ctx, `INSERT INTO reminders (schedule_id, offset_mins, fire_at, sent)
			VALUES ($1, $2, $3, $3 < NOW()) ON CONFLICT DO NOTHING`, id, m, fireAt); err != nil {
			return err
		}
	}
	return nil
}

func statusOr(s string) string {
	if s == "" {
		return "needsAction"
	}
	return s
}

// SyncAll syncs every connected user, one after another.
func (s *Service) SyncAll(ctx context.Context) {
	rows, err := s.db.Query(ctx, `SELECT user_id FROM integrations_google WHERE status = 'connected'`)
	if err != nil {
		log.Printf("google sync list: %v", err)
		return
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		log.Printf("google sync list: %v", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		c, cancel := context.WithTimeout(ctx, 2*time.Minute)
		if _, err := s.Sync(c, id); err != nil {
			log.Printf("google sync user=%s: %v", id, err)
		}
		cancel()
	}
}

// Run syncs all users every interval until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.SyncAll(ctx)
		}
	}
}

// ---- push ------------------------------------------------------------------

// PushOpts carries per-request hints that are not stored on the schedule.
type PushOpts struct {
	AddMeet     *bool  // nil keep, true create a Meet link, false remove it
	SendUpdates string // all | none; empty = all when the event has guests
}

// LoadEvent reads a schedule as a Google event, with its linked event id ("" when
// not pushed yet). Imported rows are marked Instance: they never carry a rule.
func (s *Service) LoadEvent(ctx context.Context, userID, scheduleID uuid.UUID) (Event, string, error) {
	var e Event
	var eventID *string
	var source string
	err := s.db.QueryRow(ctx, `
		SELECT s.title, COALESCE(s.description, ''), s.start_time, s.end_time, s.all_day,
		       COALESCE(r.rule, ''), s.google_event_id, s.source,
		       COALESCE(s.location, ''), COALESCE(s.color_id, ''), COALESCE(s.meet_link, ''), s.tz
		FROM schedules s LEFT JOIN schedule_recurrences r ON r.schedule_id = s.id
		WHERE s.id = $1 AND s.user_id = $2`, scheduleID, userID,
	).Scan(&e.Title, &e.Description, &e.Start, &e.End, &e.AllDay, &e.Recurrence, &eventID, &source,
		&e.Location, &e.ColorID, &e.MeetLink, &e.TimeZone)
	if err != nil {
		return e, "", err
	}
	e.Instance = source == "google"

	rows, err := s.db.Query(ctx, `SELECT DISTINCT offset_mins FROM reminders
		WHERE schedule_id = $1 AND dismissed = FALSE ORDER BY offset_mins`, scheduleID)
	if err != nil {
		return e, "", err
	}
	if e.Reminders, err = pgx.CollectRows(rows, pgx.RowTo[int]); err != nil {
		return e, "", err
	}

	rows, err = s.db.Query(ctx, `SELECT email, COALESCE(display_name, ''), response_status, is_organizer
		FROM schedule_attendees WHERE schedule_id = $1 ORDER BY email`, scheduleID)
	if err != nil {
		return e, "", err
	}
	if e.Attendees, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (a Attendee, err error) {
		err = r.Scan(&a.Email, &a.DisplayName, &a.ResponseStatus, &a.Organizer)
		return
	}); err != nil {
		return e, "", err
	}
	if eventID == nil {
		return e, "", nil
	}
	return e, *eventID, nil
}

// prepare applies the per-request hints to an event about to be written.
func prepare(e *Event, opts PushOpts) {
	if opts.AddMeet != nil {
		e.AddMeet = *opts.AddMeet && e.MeetLink == ""
		e.RemoveMeet = !*opts.AddMeet && e.MeetLink != ""
	}
	e.SendUpdates = opts.SendUpdates
	if e.SendUpdates == "" {
		e.SendUpdates = "none"
		if len(e.Attendees) > 0 {
			e.SendUpdates = "all"
		}
	}
}

// rt loads the refresh token, mapping unexpected failures to ErrAPI.
func (s *Service) rt(ctx context.Context, userID uuid.UUID) (string, error) {
	rt, err := s.refreshToken(ctx, userID)
	if err != nil && !errors.Is(err, ErrNotConnected) && !errors.Is(err, ErrNeedsReauth) {
		log.Printf("google token user=%s: %v", userID, err)
		return "", ErrAPI
	}
	return rt, err
}

// PatchRemote patches a linked Google event before any local change, so a
// failure leaves the local row untouched. Errors are mapped to the app
// sentinels (ErrNotConnected, ErrNeedsReauth, ErrAPI).
func (s *Service) PatchRemote(ctx context.Context, userID uuid.UUID, eventID string, e *Event, opts PushOpts) (Result, error) {
	rt, err := s.rt(ctx, userID)
	if err != nil {
		return Result{}, err
	}
	prepare(e, opts)
	res, err := s.client.PatchEvent(ctx, rt, eventID, *e)
	if err != nil {
		return Result{}, s.classify(ctx, userID, err)
	}
	return res, nil
}

// InsertRemote creates a Google event; same error mapping as PatchRemote.
func (s *Service) InsertRemote(ctx context.Context, userID uuid.UUID, e *Event, opts PushOpts) (Result, error) {
	rt, err := s.rt(ctx, userID)
	if err != nil {
		return Result{}, err
	}
	prepare(e, opts)
	res, err := s.client.InsertEvent(ctx, rt, *e)
	if err != nil {
		return Result{}, s.classify(ctx, userID, err)
	}
	return res, nil
}

// UploadLocal sends every local-only schedule to Google. Recurring series whose
// rule fails validRule are skipped. Guests are not notified.
func (s *Service) UploadLocal(ctx context.Context, userID uuid.UUID, validRule func(string) bool) (UploadStats, error) {
	var st UploadStats
	if err := s.Require(ctx, userID); err != nil {
		return st, err
	}
	rows, err := s.db.Query(ctx, `SELECT id FROM schedules
		WHERE user_id = $1 AND source = 'local' AND google_event_id IS NULL ORDER BY start_time`, userID)
	if err != nil {
		return st, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return st, err
	}
	for _, id := range ids {
		e, _, err := s.LoadEvent(ctx, userID, id)
		if err != nil {
			st.Failed++
			continue
		}
		if e.Recurrence != "" && !validRule(e.Recurrence) {
			st.Skipped++
			continue
		}
		res, err := s.InsertRemote(ctx, userID, &e, PushOpts{SendUpdates: "none"})
		if errors.Is(err, ErrNeedsReauth) {
			return st, err
		}
		if err == nil {
			err = s.Record(ctx, userID, id, res, e)
		}
		if err != nil {
			st.Failed++
			continue
		}
		st.Uploaded++
	}
	if st.Uploaded > 0 {
		s.SyncBackground(userID)
	}
	return st, nil
}

// Record stores a successful write: event id, etag, Meet link change, synced state.
func (s *Service) Record(ctx context.Context, userID, scheduleID uuid.UUID, res Result, e Event) error {
	_, err := s.db.Exec(ctx, `UPDATE schedules SET google_event_id = $3, google_etag = $4,
		meet_link = CASE WHEN $5 THEN NULLIF($6, '') WHEN $7 THEN NULL ELSE meet_link END
		WHERE id = $1 AND user_id = $2`, scheduleID, userID, res.ID, res.ETag, e.AddMeet, res.MeetLink, e.RemoveMeet)
	return err
}

// DeleteRemote removes a linked event (404/410 count as gone). Errors are
// mapped like PatchRemote. sendUpdates is all|none, empty means none.
func (s *Service) DeleteRemote(ctx context.Context, userID uuid.UUID, eventID, sendUpdates string) error {
	rt, err := s.rt(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.client.DeleteEvent(ctx, rt, eventID, sendUpdates); err != nil {
		return s.classify(ctx, userID, err)
	}
	return nil
}
