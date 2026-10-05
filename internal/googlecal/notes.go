package googlecal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	NotesFolderName = "MyPersonal Notes"
	DefaultTitle    = "Tanpa judul"
	maxLabels       = 8
	maxLabelBytes   = 100
	maxFileName     = 100
	previewLen      = 160
)

var (
	ErrInvalidColor = errors.New("invalid_color")
	ErrInvalidLabel = errors.New("invalid_label")
	ErrNoteNotFound = errors.New("note_not_found")

	labelRe  = regexp.MustCompile(`^[a-z0-9_-]{1,24}$`)
	colors   = map[string]bool{"yellow": true, "green": true, "blue": true, "pink": true, "gray": true}
	badChars = strings.NewReplacer("/", "", "\\", "", ":", "", "*", "", "?", "", "\"", "", "<", "", ">", "", "|", "")
)

// Note is a note stored as a Drive file. Body is only filled by Get/Create/Update.
type Note struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Preview   string    `json:"preview"`
	Pinned    bool      `json:"pinned"`
	Archived  bool      `json:"archived"`
	Color     *string   `json:"color"`
	Labels    []string  `json:"labels"`
	Trashed   bool      `json:"trashed"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type NoteInput struct {
	Title, Body      string
	Pinned, Archived bool
	Color            *string
	Labels           []string
}

// NotePatch changes only what is set.
type NotePatch struct {
	Pinned, Archived, Trashed *bool
	SetColor                  bool
	Color                     *string
	Labels                    *[]string
}

type NoteListOpts struct {
	Q, Label, PageToken string
	Archived, Trashed   bool
	Limit               int
}

// LocalNote is a row of the local notes table offered for import.
type LocalNote struct{ ID, Title, Body string }

// NotesService stores notes in the user's Drive, inside the app's notes folder.
type NotesService struct {
	s *Service
	d DriveClient
}

func NewNotesService(s *Service, d DriveClient) *NotesService { return &NotesService{s: s, d: d} }

// Require reports ErrNotConnected / ErrNeedsReauth unless Drive can be used.
func (n *NotesService) Require(ctx context.Context, userID uuid.UUID) error {
	return n.s.Require(ctx, userID)
}

// Preview is the single-line list excerpt of a body.
func Preview(body string) string {
	r := []rune(strings.Join(strings.Fields(body), " "))
	if len(r) > previewLen {
		r = r[:previewLen]
	}
	return string(r)
}

func fileName(title string) string {
	t := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, badChars.Replace(title))
	t = strings.TrimSpace(t)
	if r := []rune(t); len(r) > maxFileName {
		t = strings.TrimSpace(string(r[:maxFileName]))
	}
	if t == "" {
		t = DefaultTitle
	}
	return t + ".md"
}

func checkColor(c *string) error {
	if c != nil && !colors[*c] {
		return ErrInvalidColor
	}
	return nil
}

func normLabels(in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, l := range in {
		l = strings.ToLower(strings.TrimSpace(l))
		if !labelRe.MatchString(l) {
			return nil, ErrInvalidLabel
		}
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	if len(out) > maxLabels || len(strings.Join(out, ",")) > maxLabelBytes {
		return nil, ErrInvalidLabel
	}
	return out, nil
}

func flag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// Drive cannot delete an appProperties key through this client, so cleared keys
// are written as "0" (flags), "none" (color) or "," (no labels); readers treat
// anything but "1" / a valid color / a valid label as unset.
func colorProp(c *string) string {
	if c == nil {
		return "none"
	}
	return *c
}

func labelsProp(l []string) string {
	if len(l) == 0 {
		return ","
	}
	return strings.Join(l, ",")
}

func toNote(f DriveFile, body string) Note {
	n := Note{ID: f.ID, Title: strings.TrimSuffix(f.Name, ".md"), Body: body, Preview: f.Description,
		Pinned: f.Props["pinned"] == "1", Archived: f.Props["archived"] == "1", Labels: []string{},
		Trashed: f.Trashed, CreatedAt: f.Created, UpdatedAt: f.Modified}
	if c := f.Props["color"]; colors[c] {
		n.Color = &c
	}
	for _, l := range strings.Split(f.Props["labels"], ",") {
		if labelRe.MatchString(l) {
			n.Labels = append(n.Labels, l)
		}
	}
	return n
}

// fail maps a Drive error; unknown ones go through classify (flags needs_reauth).
func (n *NotesService) fail(ctx context.Context, userID uuid.UUID, err error) error {
	switch {
	case errors.Is(err, ErrDriveNotFound):
		return ErrNoteNotFound
	case errors.Is(err, ErrDriveScope), errors.Is(err, ErrDriveDisabled):
		return err
	}
	return n.s.classify(ctx, userID, err)
}

// open returns the refresh token and the notes folder id, creating the folder
// when it is unknown, missing or trashed.
func (n *NotesService) open(ctx context.Context, userID uuid.UUID) (rt, folder string, err error) {
	if rt, err = n.s.rt(ctx, userID); err != nil {
		return "", "", err
	}
	var stored *string
	err = n.s.db.QueryRow(ctx, `SELECT notes_folder_id FROM integrations_google WHERE user_id = $1`, userID).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotConnected
	}
	if err != nil {
		log.Printf("notes folder lookup user=%s: %v", userID, err)
		return "", "", ErrAPI
	}
	if stored != nil && *stored != "" {
		f, gerr := n.d.Get(ctx, rt, *stored)
		if gerr == nil && !f.Trashed {
			return rt, *stored, nil
		}
		if gerr != nil && !errors.Is(gerr, ErrDriveNotFound) {
			return "", "", n.fail(ctx, userID, gerr)
		}
	}
	l, err := n.d.List(ctx, rt, DriveListOpts{PageSize: 1, Query: "mimeType = '" + folderMime + "' and name = '" +
		NotesFolderName + "' and trashed = false and appProperties has { key='mypersonal' and value='notes' }"})
	if err != nil {
		return "", "", n.fail(ctx, userID, err)
	}
	var id string
	if len(l.Files) > 0 {
		id = l.Files[0].ID
	} else {
		f, err := n.d.Create(ctx, rt, DriveFile{Name: NotesFolderName, MimeType: folderMime,
			Props: map[string]string{"mypersonal": "notes"}}, nil)
		if err != nil {
			return "", "", n.fail(ctx, userID, err)
		}
		id = f.ID
	}
	if _, err := n.s.db.Exec(ctx, `UPDATE integrations_google SET notes_folder_id = $2 WHERE user_id = $1`, userID, id); err != nil {
		log.Printf("notes folder save user=%s: %v", userID, err)
		return "", "", ErrAPI
	}
	return rt, id, nil
}

// inFolder fetches file id and verifies it lives in the notes folder.
func (n *NotesService) inFolder(ctx context.Context, userID uuid.UUID, rt, folder, id string) (DriveFile, error) {
	f, err := n.d.Get(ctx, rt, id)
	if err != nil {
		return DriveFile{}, n.fail(ctx, userID, err)
	}
	for _, p := range f.Parents {
		if p == folder {
			return f, nil
		}
	}
	return DriveFile{}, ErrNoteNotFound
}

func escapeQ(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

const archivedProp = "appProperties has { key='archived' and value='1' }"

func (n *NotesService) List(ctx context.Context, userID uuid.UUID, o NoteListOpts) ([]Note, string, error) {
	rt, folder, err := n.open(ctx, userID)
	if err != nil {
		return nil, "", err
	}
	q := fmt.Sprintf("'%s' in parents and trashed = %t", folder, o.Trashed)
	if !o.Trashed {
		if o.Archived {
			q += " and " + archivedProp
		} else {
			q += " and not " + archivedProp
		}
	}
	if o.Q != "" {
		e := escapeQ(o.Q)
		q += " and (fullText contains '" + e + "' or name contains '" + e + "')"
	}
	if o.Limit < 1 {
		o.Limit = 50
	}
	if o.Limit > 100 {
		o.Limit = 100
	}
	res, err := n.d.List(ctx, rt, DriveListOpts{Query: q, PageSize: o.Limit, PageToken: o.PageToken})
	if err != nil {
		return nil, "", n.fail(ctx, userID, err)
	}
	notes := make([]Note, 0, len(res.Files))
	for _, f := range res.Files {
		nt := toNote(f, "")
		if o.Label != "" && !contains(nt.Labels, o.Label) {
			continue
		}
		notes = append(notes, nt)
	}
	sort.SliceStable(notes, func(i, j int) bool { return notes[i].Pinned && !notes[j].Pinned })
	return notes, res.NextPageToken, nil
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func (n *NotesService) Get(ctx context.Context, userID uuid.UUID, id string) (Note, error) {
	rt, folder, err := n.open(ctx, userID)
	if err != nil {
		return Note{}, err
	}
	f, err := n.inFolder(ctx, userID, rt, folder, id)
	if err != nil {
		return Note{}, err
	}
	body, err := n.d.Read(ctx, rt, id)
	if err != nil {
		return Note{}, n.fail(ctx, userID, err)
	}
	return toNote(f, body), nil
}

func (in *NoteInput) check() ([]string, error) {
	if err := checkColor(in.Color); err != nil {
		return nil, err
	}
	return normLabels(in.Labels)
}

func (n *NotesService) Create(ctx context.Context, userID uuid.UUID, in NoteInput) (Note, error) {
	labels, err := in.check()
	if err != nil {
		return Note{}, err
	}
	rt, folder, err := n.open(ctx, userID)
	if err != nil {
		return Note{}, err
	}
	props := map[string]string{}
	if in.Pinned {
		props["pinned"] = "1"
	}
	if in.Archived {
		props["archived"] = "1"
	}
	if in.Color != nil {
		props["color"] = *in.Color
	}
	if len(labels) > 0 {
		props["labels"] = strings.Join(labels, ",")
	}
	f, err := n.d.Create(ctx, rt, DriveFile{Name: fileName(in.Title), Description: Preview(in.Body), MimeType: notesMime,
		Props: props, Parents: []string{folder}}, &in.Body)
	if err != nil {
		return Note{}, n.fail(ctx, userID, err)
	}
	return toNote(f, in.Body), nil
}

// Update replaces title, body and all metadata (omitted = off/none/empty).
func (n *NotesService) Update(ctx context.Context, userID uuid.UUID, id string, in NoteInput) (Note, error) {
	labels, err := in.check()
	if err != nil {
		return Note{}, err
	}
	rt, folder, err := n.open(ctx, userID)
	if err != nil {
		return Note{}, err
	}
	if _, err := n.inFolder(ctx, userID, rt, folder, id); err != nil {
		return Note{}, err
	}
	name, prev := fileName(in.Title), Preview(in.Body)
	f, err := n.d.Update(ctx, rt, id, DriveUpdate{Name: &name, Description: &prev, Body: &in.Body, Props: map[string]string{
		"pinned": flag(in.Pinned), "archived": flag(in.Archived), "color": colorProp(in.Color), "labels": labelsProp(labels)}})
	if err != nil {
		return Note{}, n.fail(ctx, userID, err)
	}
	return toNote(f, in.Body), nil
}

// Patch changes metadata only; the returned note has no body.
func (n *NotesService) Patch(ctx context.Context, userID uuid.UUID, id string, p NotePatch) (Note, error) {
	props := map[string]string{}
	if p.Pinned != nil {
		props["pinned"] = flag(*p.Pinned)
	}
	if p.Archived != nil {
		props["archived"] = flag(*p.Archived)
	}
	if p.SetColor {
		if err := checkColor(p.Color); err != nil {
			return Note{}, err
		}
		props["color"] = colorProp(p.Color)
	}
	if p.Labels != nil {
		l, err := normLabels(*p.Labels)
		if err != nil {
			return Note{}, err
		}
		props["labels"] = labelsProp(l)
	}
	rt, folder, err := n.open(ctx, userID)
	if err != nil {
		return Note{}, err
	}
	f, err := n.inFolder(ctx, userID, rt, folder, id)
	if err != nil {
		return Note{}, err
	}
	if len(props) == 0 && p.Trashed == nil {
		return toNote(f, ""), nil
	}
	f, err = n.d.Update(ctx, rt, id, DriveUpdate{Props: props, Trashed: p.Trashed})
	if err != nil {
		return Note{}, n.fail(ctx, userID, err)
	}
	return toNote(f, ""), nil
}

// Delete trashes the note, or removes it for good when permanent.
func (n *NotesService) Delete(ctx context.Context, userID uuid.UUID, id string, permanent bool) error {
	rt, folder, err := n.open(ctx, userID)
	if err != nil {
		return err
	}
	if _, err := n.inFolder(ctx, userID, rt, folder, id); err != nil {
		return err
	}
	if permanent {
		err = n.d.Delete(ctx, rt, id)
	} else {
		yes := true
		_, err = n.d.Update(ctx, rt, id, DriveUpdate{Trashed: &yes})
	}
	if err != nil {
		return n.fail(ctx, userID, err)
	}
	return nil
}

type ImportStats struct {
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
}

// ImportLocal copies local notes to Drive once; a localId marker makes it idempotent.
func (n *NotesService) ImportLocal(ctx context.Context, userID uuid.UUID, local []LocalNote) (ImportStats, error) {
	var st ImportStats
	rt, folder, err := n.open(ctx, userID)
	if err != nil {
		return st, err
	}
	seen := map[string]bool{}
	token := ""
	for {
		res, err := n.d.List(ctx, rt, DriveListOpts{Query: "'" + folder + "' in parents", PageSize: 100, PageToken: token})
		if err != nil {
			return st, n.fail(ctx, userID, err)
		}
		for _, f := range res.Files {
			if id := f.Props["localId"]; id != "" {
				seen[id] = true
			}
		}
		if token = res.NextPageToken; token == "" {
			break
		}
	}
	for _, ln := range local {
		if seen[ln.ID] {
			st.Skipped++
			continue
		}
		body := ln.Body
		if _, err := n.d.Create(ctx, rt, DriveFile{Name: fileName(ln.Title), Description: Preview(body), MimeType: notesMime,
			Props: map[string]string{"localId": ln.ID}, Parents: []string{folder}}, &body); err != nil {
			return st, n.fail(ctx, userID, err)
		}
		seen[ln.ID] = true
		st.Imported++
	}
	return st, nil
}
