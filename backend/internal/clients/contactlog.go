package clients

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dos-bpas/internal/platform/audit"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/notify"
)

// The contact log.
//
// Staff record each call, visit, meeting, email or message with a client, and
// the next step it led to (a follow-up with a date and a person). The latest
// contact is kept on the client so lists can show who hasn't been spoken to,
// and open follow-ups are chased by the reminder job (reminders.go).
//
// Entries are a record of what happened, so they are never edited or deleted.
// Only the follow-up changes: done, moved to another date, or handed to
// someone else, each recorded in the client's audit history.

const (
	FadingAfterDays = 30 // no contact for this long: the relationship is fading
	ColdAfterDays   = 60 // …and for this long: the client is going cold
)

// Kinds of contact. A note is internal and doesn't count as contact with the client.
var interactionKinds = map[string]string{
	"call": "Call", "visit": "Visit", "meeting": "Meeting", "email": "Email", "message": "Message", "note": "Note",
}

type Interaction struct {
	ID         uuid.UUID   `json:"id"`
	ClientID   uuid.UUID   `json:"client_id"`
	Kind       string      `json:"kind"`
	OccurredAt time.Time   `json:"occurred_at"`
	Summary    string      `json:"summary"`
	Outcome    *string     `json:"outcome"`
	Contact    *ContactRef `json:"contact"`
	FollowUp   *FollowUp   `json:"follow_up"`
	CheckIn    *CheckIn    `json:"check_in"`
	CreatedBy  Person      `json:"created_by"`
	CreatedAt  time.Time   `json:"created_at"`
	// Client is filled in on the team log, where entries span many clients.
	Client *ClientBrief `json:"client,omitempty"`
}

type ClientBrief struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	LegalName string    `json:"legal_name"`
}

// CheckIn is the phone's location when a visit was logged on site.
type CheckIn struct {
	Lat       float64   `json:"lat"`
	Lng       float64   `json:"lng"`
	AccuracyM int       `json:"accuracy_m"`
	At        time.Time `json:"at"`
}

type CheckInInput struct {
	Lat       float64 `json:"lat"`
	Lng       float64 `json:"lng"`
	AccuracyM float64 `json:"accuracy_m"`
}

type ContactRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type FollowUp struct {
	On       string     `json:"on"` // YYYY-MM-DD
	Note     *string    `json:"note"`
	Owner    Person     `json:"owner"`
	DoneAt   *time.Time `json:"done_at"`
	DoneBy   *string    `json:"done_by"` // name
	DoneNote *string    `json:"done_note"`
}

type InteractionInput struct {
	Kind            string     `json:"kind"`
	OccurredAt      *time.Time `json:"occurred_at"` // defaults to now
	ContactID       *uuid.UUID `json:"contact_id"`
	Summary         string     `json:"summary"`
	Outcome         *string    `json:"outcome"`
	FollowUpOn      *string    `json:"follow_up_on"` // YYYY-MM-DD
	FollowUpNote    *string    `json:"follow_up_note"`
	FollowUpOwnerID *uuid.UUID `json:"follow_up_owner_id"` // defaults to the person logging
	// Completes lists open follow-ups on this client that this contact took care of.
	Completes []uuid.UUID `json:"completes"`
	// CheckIn is the phone's location, for a visit or meeting logged on site.
	CheckIn *CheckInInput `json:"check_in"`
}

// FollowUpChange closes, moves or hands over an open follow-up.
type FollowUpChange struct {
	Action  string     `json:"action"` // done, reschedule or reassign
	On      *string    `json:"on"`
	OwnerID *uuid.UUID `json:"owner_id"`
	Note    string     `json:"note"`
}

// OpenFollowUp is a follow-up still to do, for lists across clients.
type OpenFollowUp struct {
	InteractionID uuid.UUID `json:"interaction_id"`
	ClientID      uuid.UUID `json:"client_id"`
	ClientRef     string    `json:"client_reference"`
	ClientName    string    `json:"client_name"`
	On            string    `json:"on"`
	Note          *string   `json:"note"`
	Summary       string    `json:"summary"`
	Kind          string    `json:"kind"`
	Owner         Person    `json:"owner"`
}

var (
	errFollowUpClosed = httpx.NewError(http.StatusConflict, "follow_up_closed", "This follow-up has already been done.")
	errNoFollowUp     = httpx.NewError(http.StatusNotFound, "not_found", "This entry has no follow-up.")
)

const maxSummary = 4000

// ---- queries ----

func (s *Service) Interactions(ctx context.Context, clientID uuid.UUID, limit int) ([]Interaction, error) {
	if _, err := getClient(ctx, s.pool, clientID, false); db.IsNoRows(err) {
		return nil, httpx.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	return listInteractions(ctx, s.pool, `i.client_id = $1`, []any{clientID}, limit)
}

// MyFollowUps lists the open follow-ups a person is responsible for, earliest first.
func (s *Service) MyFollowUps(ctx context.Context, userID uuid.UUID, limit int) ([]OpenFollowUp, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, c.id, c.reference, c.legal_name, to_char(i.follow_up_on, 'YYYY-MM-DD'), i.follow_up_note,
		       i.summary, i.kind, u.id, u.full_name, u.position
		FROM client_interactions i
		JOIN clients c ON c.id = i.client_id
		JOIN users u ON u.id = i.follow_up_owner_id
		WHERE i.follow_up_owner_id = $1 AND i.follow_up_on IS NOT NULL AND i.follow_up_done_at IS NULL
		ORDER BY i.follow_up_on, c.legal_name
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (OpenFollowUp, error) {
		var f OpenFollowUp
		err := r.Scan(&f.InteractionID, &f.ClientID, &f.ClientRef, &f.ClientName, &f.On, &f.Note,
			&f.Summary, &f.Kind, &f.Owner.ID, &f.Owner.Name, &f.Owner.Position)
		return f, err
	})
}

// ---- commands ----

// LogInteraction records a contact with the client, updates when the client
// was last contacted, closes any follow-ups it took care of, and tells the
// follow-up's owner if someone else set it for them.
func (s *Service) LogInteraction(ctx context.Context, clientID uuid.UUID, in InteractionInput) (Interaction, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return Interaction{}, httpx.ErrUnauthorized
	}
	now := s.now()
	today := s.today()

	in.Kind = strings.TrimSpace(in.Kind)
	in.Summary = strings.TrimSpace(in.Summary)
	in.Outcome, in.FollowUpNote = clean(in.Outcome), clean(in.FollowUpNote)
	at := now
	if in.OccurredAt != nil {
		at = *in.OccurredAt
	}

	fe := httpx.FieldErrors{}
	if _, ok := interactionKinds[in.Kind]; !ok {
		fe.Add("kind", "Choose call, visit, meeting, email, message or note.")
	}
	if in.Summary == "" {
		fe.Add("summary", "Say briefly what was discussed.")
	} else if utf8.RuneCountInString(in.Summary) > maxSummary {
		fe.Add("summary", "Keep it under 4,000 characters.")
	}
	if at.After(now.Add(10 * time.Minute)) {
		fe.Add("occurred_at", "This can't be in the future. Use a follow-up for things still to happen.")
	} else if at.Before(now.AddDate(-1, 0, 0)) {
		fe.Add("occurred_at", "Entries can go back up to a year.")
	}
	var followOn *time.Time
	if in.FollowUpOn != nil && strings.TrimSpace(*in.FollowUpOn) != "" {
		d, err := parseDay(*in.FollowUpOn)
		switch {
		case err != nil:
			fe.Add("follow_up_on", "Enter a date.")
		case d.Before(today):
			fe.Add("follow_up_on", "Choose today or a later date.")
		case d.After(today.AddDate(2, 0, 0)):
			fe.Add("follow_up_on", "Choose a date within two years.")
		default:
			followOn = &d
		}
	}
	owner := actor.UserID
	if in.FollowUpOwnerID != nil {
		owner = *in.FollowUpOwnerID
	}
	if c := in.CheckIn; c != nil {
		switch {
		case in.Kind != "visit" && in.Kind != "meeting":
			fe.Add("check_in", "Only visits and meetings can be checked in.")
		case at.Before(now.Add(-2 * time.Hour)):
			fe.Add("check_in", "Check in while you're at the client. For an earlier visit, log it without a location.")
		case c.Lat < -90 || c.Lat > 90 || c.Lng < -180 || c.Lng > 180 || (c.Lat == 0 && c.Lng == 0):
			fe.Add("check_in", "The location from your phone wasn't valid. Try again.")
		case c.AccuracyM < 0 || c.AccuracyM > 20000:
			fe.Add("check_in", "Your phone's location is too rough to record. Move outside or turn on GPS and try again.")
		}
	}
	if err := fe.Err(); err != nil {
		return Interaction{}, err
	}
	var lat, lng *float64
	var acc *int
	var checkedAt *time.Time
	if c := in.CheckIn; c != nil {
		la, lo, a := c.Lat, c.Lng, int(c.AccuracyM+0.5)
		lat, lng, acc, checkedAt = &la, &lo, &a, &now
	}

	var id uuid.UUID
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := getClient(ctx, tx, clientID, true)
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if c.Status != StatusActive {
			return ErrInactive
		}
		if in.ContactID != nil {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM client_contacts WHERE id = $1 AND client_id = $2)`,
				*in.ContactID, clientID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return httpx.FieldErrors{"contact_id": "Choose one of this client's contacts."}.Err()
			}
		}
		var ownerPtr *uuid.UUID
		if followOn != nil {
			ok, err := activeUserExists(ctx, tx, owner)
			if err != nil {
				return err
			}
			if !ok {
				return httpx.FieldErrors{"follow_up_owner_id": "Choose an active staff member."}.Err()
			}
			ownerPtr = &owner
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO client_interactions (client_id, contact_id, kind, occurred_at, summary, outcome,
			                                 follow_up_on, follow_up_note, follow_up_owner_id, created_by,
			                                 checkin_lat, checkin_lng, checkin_accuracy_m, checked_in_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
			clientID, in.ContactID, in.Kind, at, in.Summary, in.Outcome,
			followOn, nilIfNoDate(followOn, in.FollowUpNote), ownerPtr, actor.UserID,
			lat, lng, acc, checkedAt).Scan(&id); err != nil {
			return err
		}
		if in.Kind != "note" {
			if _, err := tx.Exec(ctx, `UPDATE clients SET last_contact_at = greatest(coalesce(last_contact_at, $2), $2)
				WHERE id = $1`, clientID, at); err != nil {
				return err
			}
		}

		// Follow-ups this contact took care of.
		label := interactionKinds[in.Kind]
		for _, fid := range uniqueIDs(in.Completes) {
			f, err := lockFollowUp(ctx, tx, clientID, fid)
			if err != nil {
				return err
			}
			if f.doneAt != nil {
				continue // someone else closed it meanwhile
			}
			if !mayChangeFollowUp(actor, c, f) {
				return httpx.ErrForbidden
			}
			note := label + " on " + at.In(s.loc()).Format("2 Jan") + ": " + shortText(in.Summary, 120)
			if err := closeFollowUp(ctx, tx, fid, actor.UserID, note); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Entry{EntityType: entityType, EntityID: clientID, Action: "follow_up_done",
				Summary: "Follow-up done: " + followUpLabel(f)}); err != nil {
				return err
			}
		}

		// Tell the person if someone else gave them the follow-up.
		if followOn != nil && owner != actor.UserID {
			_, err := notify.Create(ctx, tx, notify.Notification{
				UserID: owner, Kind: KindFollowUpAssigned, Link: "/clients/" + clientID.String(),
				EntityType: entityType, EntityID: &clientID,
				Title:     actor.Name + " asked you to follow up with " + c.LegalName,
				Body:      "By " + followOn.Format("Mon 2 Jan") + " · " + coalesce(in.FollowUpNote, shortText(in.Summary, 140)),
				DedupeKey: fmt.Sprintf("%s:%s", KindFollowUpAssigned, id),
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Interaction{}, err
	}
	items, err := listInteractions(ctx, s.pool, `i.id = $1`, []any{id}, 1)
	if err != nil || len(items) == 0 {
		return Interaction{}, err
	}
	return items[0], nil
}

// ChangeFollowUp marks a follow-up done, moves it to another date, or hands it
// to someone else. The follow-up's owner, the client's owner, whoever set it,
// and managers may do this.
func (s *Service) ChangeFollowUp(ctx context.Context, clientID, interactionID uuid.UUID, in FollowUpChange) (Interaction, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return Interaction{}, httpx.ErrUnauthorized
	}
	in.Note = strings.TrimSpace(in.Note)
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := getClient(ctx, tx, clientID, true)
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		f, err := lockFollowUp(ctx, tx, clientID, interactionID)
		if err != nil {
			return err
		}
		if f.doneAt != nil {
			return errFollowUpClosed
		}
		if !mayChangeFollowUp(actor, c, f) {
			return httpx.ErrForbidden
		}

		entry := audit.Entry{EntityType: entityType, EntityID: clientID, Reason: in.Note}
		switch in.Action {
		case "done":
			if err := closeFollowUp(ctx, tx, interactionID, actor.UserID, in.Note); err != nil {
				return err
			}
			entry.Action, entry.Summary, entry.Reason = "follow_up_done", "Follow-up done: "+followUpLabel(f), ""
			if in.Note != "" {
				entry.Changes = map[string]any{"note": in.Note}
			}

		case "reschedule":
			if in.On == nil {
				return httpx.FieldErrors{"on": "Choose the new date."}.Err()
			}
			d, err := parseDay(*in.On)
			if err != nil {
				return httpx.FieldErrors{"on": "Enter a date."}.Err()
			}
			if d.Before(s.today()) {
				return httpx.FieldErrors{"on": "Choose today or a later date."}.Err()
			}
			if d.Equal(f.on) {
				return errNoChange
			}
			if _, err := tx.Exec(ctx, `UPDATE client_interactions SET follow_up_on = $2 WHERE id = $1`, interactionID, d); err != nil {
				return err
			}
			entry.Action, entry.Summary = "follow_up_moved", "Follow-up moved to "+d.Format("2 Jan 2006")+": "+followUpLabel(f)
			entry.Changes = map[string]any{"follow_up_on": map[string]any{"from": f.on.Format("2006-01-02"), "to": d.Format("2006-01-02")}}

		case "reassign":
			if in.OwnerID == nil {
				return httpx.FieldErrors{"owner_id": "Choose who should follow up."}.Err()
			}
			if *in.OwnerID == f.ownerID {
				return errNoChange
			}
			ok, err := activeUserExists(ctx, tx, *in.OwnerID)
			if err != nil {
				return err
			}
			if !ok {
				return httpx.FieldErrors{"owner_id": "Choose an active staff member."}.Err()
			}
			if _, err := tx.Exec(ctx, `UPDATE client_interactions SET follow_up_owner_id = $2 WHERE id = $1`, interactionID, *in.OwnerID); err != nil {
				return err
			}
			entry.Action, entry.Summary = "follow_up_reassigned", "Follow-up handed over: "+followUpLabel(f)
			entry.Changes = map[string]any{"follow_up_owner_id": map[string]any{"from": f.ownerID.String(), "to": in.OwnerID.String()}}
			if *in.OwnerID != actor.UserID {
				body := "By " + f.on.Format("Mon 2 Jan") + " · " + followUpLabel(f)
				if in.Note != "" {
					body += " — " + in.Note
				}
				if _, err := notify.Create(ctx, tx, notify.Notification{
					UserID: *in.OwnerID, Kind: KindFollowUpAssigned, Link: "/clients/" + clientID.String(),
					EntityType: entityType, EntityID: &clientID,
					Title:     actor.Name + " handed you a follow-up with " + c.LegalName,
					Body:      body,
					DedupeKey: fmt.Sprintf("%s:%s:%s:%d", KindFollowUpAssigned, interactionID, *in.OwnerID, time.Now().UnixNano()),
				}); err != nil {
					return err
				}
			}

		default:
			return httpx.FieldErrors{"action": "Use done, reschedule or reassign."}.Err()
		}
		return audit.Record(ctx, tx, entry)
	})
	if err != nil {
		return Interaction{}, err
	}
	items, err := listInteractions(ctx, s.pool, `i.id = $1`, []any{interactionID}, 1)
	if err != nil || len(items) == 0 {
		return Interaction{}, err
	}
	return items[0], nil
}

// ---- internals ----

type lockedFollowUp struct {
	on        time.Time
	note      *string
	summary   string
	ownerID   uuid.UUID
	createdBy uuid.UUID
	doneAt    *time.Time
}

func lockFollowUp(ctx context.Context, tx pgx.Tx, clientID, id uuid.UUID) (lockedFollowUp, error) {
	var f lockedFollowUp
	var on *time.Time
	var owner *uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT follow_up_on, follow_up_note, summary, follow_up_owner_id, created_by, follow_up_done_at
		FROM client_interactions WHERE id = $1 AND client_id = $2 FOR UPDATE`, id, clientID).
		Scan(&on, &f.note, &f.summary, &owner, &f.createdBy, &f.doneAt)
	if db.IsNoRows(err) {
		return f, httpx.ErrNotFound
	}
	if err != nil {
		return f, err
	}
	if on == nil || owner == nil {
		return f, errNoFollowUp
	}
	f.on = time.Date(on.Year(), on.Month(), on.Day(), 0, 0, 0, 0, time.UTC)
	f.ownerID = *owner
	return f, nil
}

func closeFollowUp(ctx context.Context, tx pgx.Tx, id, by uuid.UUID, note string) error {
	_, err := tx.Exec(ctx, `UPDATE client_interactions
		SET follow_up_done_at = now(), follow_up_done_by = $2, follow_up_done_note = NULLIF($3, '')
		WHERE id = $1 AND follow_up_done_at IS NULL`, id, by, note)
	return err
}

func mayChangeFollowUp(actor auth.Principal, c Client, f lockedFollowUp) bool {
	return actor.UserID == f.ownerID || actor.UserID == f.createdBy || actor.UserID == c.Owner.ID || actor.Can(PermManage)
}

func followUpLabel(f lockedFollowUp) string {
	if f.note != nil {
		return shortText(*f.note, 100)
	}
	return shortText(f.summary, 100)
}

func listInteractions(ctx context.Context, q db.Querier, where string, args []any, limit int) ([]Interaction, error) {
	return queryInteractions(ctx, q, where, args, limit, 0, false)
}

func queryInteractions(ctx context.Context, q db.Querier, where string, args []any, limit, offset int, withClient bool) ([]Interaction, error) {
	args = append(args, limit, offset)
	rows, err := q.Query(ctx, `
		SELECT i.id, i.client_id, i.kind, i.occurred_at, i.summary, i.outcome,
		       k.id, k.name,
		       to_char(i.follow_up_on, 'YYYY-MM-DD'), i.follow_up_note,
		       fo.id, fo.full_name, fo.position,
		       i.follow_up_done_at, fd.full_name, i.follow_up_done_note,
		       cb.id, cb.full_name, cb.position, i.created_at,
		       i.checkin_lat::float8, i.checkin_lng::float8, i.checkin_accuracy_m, i.checked_in_at,
		       cl.reference, cl.legal_name
		FROM client_interactions i
		JOIN users cb ON cb.id = i.created_by
		JOIN clients cl ON cl.id = i.client_id
		LEFT JOIN client_contacts k ON k.id = i.contact_id
		LEFT JOIN users fo ON fo.id = i.follow_up_owner_id
		LEFT JOIN users fd ON fd.id = i.follow_up_done_by
		WHERE `+where+`
		ORDER BY i.occurred_at DESC, i.created_at DESC
		LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Interaction, error) {
		var it Interaction
		var kID *uuid.UUID
		var kName *string
		var on *string
		var fNote, foName, foPos, fdName, fdNote *string
		var foID *uuid.UUID
		var doneAt *time.Time
		var lat, lng *float64
		var acc *int
		var checked *time.Time
		var cref, cname string
		err := r.Scan(&it.ID, &it.ClientID, &it.Kind, &it.OccurredAt, &it.Summary, &it.Outcome,
			&kID, &kName, &on, &fNote, &foID, &foName, &foPos, &doneAt, &fdName, &fdNote,
			&it.CreatedBy.ID, &it.CreatedBy.Name, &it.CreatedBy.Position, &it.CreatedAt,
			&lat, &lng, &acc, &checked, &cref, &cname)
		if err != nil {
			return it, err
		}
		if lat != nil && lng != nil && checked != nil {
			it.CheckIn = &CheckIn{Lat: *lat, Lng: *lng, At: *checked}
			if acc != nil {
				it.CheckIn.AccuracyM = *acc
			}
		}
		if withClient {
			it.Client = &ClientBrief{ID: it.ClientID, Reference: cref, LegalName: cname}
		}
		if kID != nil {
			it.Contact = &ContactRef{ID: *kID, Name: *kName}
		}
		if on != nil && foID != nil {
			it.FollowUp = &FollowUp{On: *on, Note: fNote, Owner: Person{ID: *foID, Name: deref(foName), Position: deref(foPos)},
				DoneAt: doneAt, DoneBy: fdName, DoneNote: fdNote}
		}
		return it, nil
	})
}

// today is the current date in the business timezone, as a UTC midnight.
func (s *Service) today() time.Time {
	t := s.now().In(s.loc())
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func (s *Service) loc() *time.Location {
	if l, err := time.LoadLocation(s.tz); err == nil {
		return l
	}
	return time.UTC
}

func parseDay(v string) (time.Time, error) {
	return time.Parse("2006-01-02", strings.TrimSpace(v))
}

func nilIfNoDate(d *time.Time, s *string) *string {
	if d == nil {
		return nil
	}
	return s
}

func uniqueIDs(ids []uuid.UUID) []uuid.UUID {
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func shortText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n])) + "…"
}

func coalesce(s *string, fallback string) string {
	if s != nil && *s != "" {
		return *s
	}
	return fallback
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
