package deals

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dos-bpas/internal/clients"
	"dos-bpas/internal/platform/audit"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/events"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/notify"
	"dos-bpas/internal/platform/numbering"
)

// Events published by this module.
const (
	EventCreated      = "deal.created"
	EventUpdated      = "deal.updated"
	EventStageChanged = "deal.stage_changed"
	EventWon          = "deal.won"
	EventLost         = "deal.lost"
)

const entityType = "deal"

// API is what other modules may use (quotations will link to a deal).
type API interface {
	Get(ctx context.Context, id uuid.UUID) (Deal, error)
	// QuoteSent and QuoteAccepted keep a deal in step with its quotations (quotes_hooks.go).
	QuoteSent(ctx context.Context, q db.Querier, dealID uuid.UUID, quoteRef, total, currency string) error
	QuoteAccepted(ctx context.Context, q db.Querier, dealID uuid.UUID, quoteRef, total, currency string) error
}

var (
	errNoChange = httpx.NewError(http.StatusUnprocessableEntity, "no_change", "Nothing was changed.")
	errClosed   = httpx.NewError(http.StatusConflict, "deal_closed",
		"This deal is closed. Reopen it before changing its details.")
	errNotYours = httpx.NewError(http.StatusForbidden, "not_your_deal",
		"Only the deal's owner or a sales manager can change this deal.")
)

type Service struct {
	log     *slog.Logger
	pool    *pgxpool.Pool
	numbers *numbering.Service
	bus     *events.Bus
	clients clients.API
	now     func() time.Time
	tz      string
}

func NewService(pool *pgxpool.Pool, numbers *numbering.Service, bus *events.Bus, cl clients.API, now func() time.Time) *Service {
	return &Service{pool: pool, numbers: numbers, bus: bus, clients: cl, now: now, tz: "Africa/Lusaka"}
}

var _ API = (*Service)(nil)

// ---- queries ----

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Deal, error) {
	d, err := getDeal(ctx, s.pool, id, false)
	if db.IsNoRows(err) {
		return Deal{}, httpx.ErrNotFound
	}
	if err != nil {
		return Deal{}, err
	}
	out := []Deal{d}
	if err := s.attachClients(ctx, out); err != nil {
		return Deal{}, err
	}
	return out[0], nil
}

func (s *Service) List(ctx context.Context, f ListFilter) (httpx.List[Deal], error) {
	var matches []uuid.UUID
	if strings.TrimSpace(f.Query) != "" {
		var err error
		if matches, err = s.clients.MatchIDs(ctx, f.Query, 500); err != nil {
			return httpx.List[Deal]{}, err
		}
	}
	items, total, err := listDeals(ctx, s.pool, f, matches)
	if err != nil {
		return httpx.List[Deal]{}, err
	}
	if err := s.attachClients(ctx, items); err != nil {
		return httpx.List[Deal]{}, err
	}
	return httpx.List[Deal]{Items: items, Total: total, Limit: f.Page.Limit, Offset: f.Page.Offset}, nil
}

func (s *Service) Notes(ctx context.Context, id uuid.UUID) ([]Note, error) {
	return listNotes(ctx, s.pool, id)
}

func (s *Service) Activity(ctx context.Context, id uuid.UUID) ([]audit.Event, error) {
	return audit.Timeline(ctx, s.pool, entityType, id, 200)
}

// attachClients fills in each deal's client name and reference.
func (s *Service) attachClients(ctx context.Context, ds []Deal) error {
	ids := make([]uuid.UUID, 0, len(ds))
	for _, d := range ds {
		ids = append(ids, d.clientID)
	}
	refs, err := s.clients.Lookup(ctx, ids)
	if err != nil {
		return err
	}
	for i := range ds {
		if r, ok := refs[ds[i].clientID]; ok {
			ds[i].Client = ClientRef{ID: r.ID, Reference: r.Reference, LegalName: r.LegalName, Sector: r.Sector, Status: string(r.Status)}
		}
	}
	return nil
}

// ---- commands ----

func (s *Service) Create(ctx context.Context, in CreateInput) (Deal, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return Deal{}, httpx.ErrUnauthorized
	}
	in.Title = strings.TrimSpace(in.Title)
	in.Description, in.Source, in.NextStep = clean(in.Description), clean(in.Source), clean(in.NextStep)
	in.Value = cleanAmount(in.Value)
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.Currency == "" {
		in.Currency = "ZMW"
	}
	if in.Stage == "" {
		in.Stage = StageLead
	}
	owner := actor.UserID
	if in.OwnerID != nil {
		owner = *in.OwnerID
	}

	fe := httpx.FieldErrors{}
	if in.ClientID == uuid.Nil {
		fe.Add("client_id", "Choose the client this deal is with.")
	}
	if !in.Stage.Valid() || in.Stage.Closed() {
		fe.Add("stage", "A new deal starts in one of the open stages.")
	}
	prob := in.Stage.DefaultProbability()
	if in.Probability != nil {
		prob = *in.Probability
	}
	validate(fe, in.Title, in.Value, in.Currency, prob, in.ExpectedClose, in.NextStepDate)
	if owner != actor.UserID && !actor.Can(PermManage) {
		fe.Add("owner_id", "Only a sales manager can create a deal for someone else.")
	}
	if err := fe.Err(); err != nil {
		return Deal{}, err
	}

	// Every deal must be with a registered, active client (brief 4.1).
	client, err := s.clients.RequireActive(ctx, in.ClientID)
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return Deal{}, httpx.FieldErrors{"client_id": "Choose a registered client."}.Err()
		}
		return Deal{}, err
	}

	var id uuid.UUID
	var ref string
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		ok, err := activeUserExists(ctx, tx, owner)
		if err != nil {
			return err
		}
		if !ok {
			return httpx.FieldErrors{"owner_id": "Choose an active staff member."}.Err()
		}
		if ref, err = s.numbers.Next(ctx, tx, numbering.Deal); err != nil {
			return err
		}
		id, err = insertDeal(ctx, tx, dealRow{
			Reference: ref, ClientID: in.ClientID, Title: in.Title, Description: in.Description, Stage: in.Stage,
			Value: in.Value, Currency: in.Currency, Probability: prob, ExpectedClose: in.ExpectedClose,
			Source: in.Source, NextStep: in.NextStep, NextStepDate: in.NextStepDate, OwnerID: owner, CreatedBy: actor.UserID,
		})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: entityType, EntityID: id, Action: "created",
			Summary: "Deal " + ref + " opened with " + client.LegalName})
	})
	if err != nil {
		return Deal{}, err
	}
	d, err := s.Get(ctx, id)
	if err != nil {
		return Deal{}, err
	}
	s.bus.Publish(ctx, events.Event{Name: EventCreated, Payload: d})
	return d, nil
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (Deal, error) {
	var updated Deal
	var previousOwner uuid.UUID
	err := s.mutate(ctx, id, in.Version, func(tx pgx.Tx, actor auth.Principal, d *Deal) (audit.Entry, error) {
		if d.Stage.Closed() {
			return audit.Entry{}, errClosed
		}
		before := snapshot(*d)
		previousOwner = d.Owner.ID
		ownerChanged := in.OwnerID != nil && *in.OwnerID != d.Owner.ID

		if in.Title != nil {
			d.Title = strings.TrimSpace(*in.Title)
		}
		if in.Description != nil {
			d.Description = clean(in.Description)
		}
		if in.Value != nil {
			d.Value = cleanAmount(in.Value)
		}
		if in.Currency != nil {
			d.Currency = strings.ToUpper(strings.TrimSpace(*in.Currency))
		}
		if in.Probability != nil {
			d.Probability = *in.Probability
		}
		if in.ExpectedClose != nil {
			d.ExpectedClose = cleanDate(in.ExpectedClose)
		}
		if in.Source != nil {
			d.Source = clean(in.Source)
		}
		if in.NextStep != nil {
			d.NextStep = clean(in.NextStep)
		}
		if in.NextStepDate != nil {
			d.NextStepDate = cleanDate(in.NextStepDate)
		}

		fe := httpx.FieldErrors{}
		validate(fe, d.Title, d.Value, d.Currency, d.Probability, d.ExpectedClose, d.NextStepDate)
		if ownerChanged {
			if !actor.Can(PermManage) {
				return audit.Entry{}, httpx.ErrForbidden
			}
			if strings.TrimSpace(in.Reason) == "" {
				fe.Add("reason", "Give a reason for handing the deal over.")
			}
			ok, err := activeUserExists(ctx, tx, *in.OwnerID)
			if err != nil {
				return audit.Entry{}, err
			}
			if !ok {
				fe.Add("owner_id", "Choose an active staff member.")
			}
			d.Owner.ID = *in.OwnerID
		}
		if err := fe.Err(); err != nil {
			return audit.Entry{}, err
		}

		changes := audit.Diff(before, snapshot(*d))
		if len(changes) == 0 {
			return audit.Entry{}, errNoChange
		}
		e := audit.Entry{EntityType: entityType, EntityID: d.ID, Action: "updated",
			Summary: "Deal details updated", Changes: changes}
		if ownerChanged {
			e.Action, e.Summary, e.Reason = "reassigned", "Deal handed over to a new owner", strings.TrimSpace(in.Reason)
		}
		return e, nil
	}, &updated)
	if err != nil {
		return Deal{}, err
	}
	s.bus.Publish(ctx, events.Event{Name: EventUpdated, Payload: updated})
	if updated.Owner.ID != previousOwner {
		s.tellOwner(ctx, updated, KindDealAssigned, "A deal was handed to you: "+updated.Title,
			updated.Client.LegalName+" · "+strings.TrimSpace(in.Reason))
	}
	return updated, nil
}

// tellOwner notifies the deal's owner of something a colleague did, unless the
// owner did it themselves. A failure is logged, not returned: the change itself
// has already been saved.
func (s *Service) tellOwner(ctx context.Context, d Deal, kind, title, body string) {
	actor, _ := auth.FromContext(ctx)
	if actor.UserID == d.Owner.ID {
		return
	}
	id := d.ID
	if _, err := notify.Create(ctx, s.pool, notify.Notification{
		UserID: d.Owner.ID, Kind: kind, Title: title, Body: body,
		Link: "/deals/" + d.ID.String(), EntityType: entityType, EntityID: &id,
	}); err != nil && s.log != nil {
		s.log.Error("deal notification failed", "deal", d.ID, "err", err)
	}
}

// Move changes the deal's stage. Losing a deal needs a reason, and so does
// reopening a closed one; winning needs the client to still be active.
func (s *Service) Move(ctx context.Context, id uuid.UUID, in MoveInput) (Deal, error) {
	if !in.Stage.Valid() {
		return Deal{}, httpx.FieldErrors{"stage": "Choose a valid stage."}.Err()
	}
	reason := strings.TrimSpace(in.Reason)
	var from Stage
	var updated Deal
	err := s.mutate(ctx, id, in.Version, func(_ pgx.Tx, _ auth.Principal, d *Deal) (audit.Entry, error) {
		from = d.Stage
		if d.Stage == in.Stage {
			return audit.Entry{}, errNoChange
		}
		fe := httpx.FieldErrors{}
		reopening := d.Stage.Closed() && !in.Stage.Closed()
		if in.Stage == StageLost && reason == "" {
			fe.Add("reason", "Say why the deal was lost, e.g. price, timing or went with a competitor.")
		}
		if reopening && reason == "" {
			fe.Add("reason", "Say why the deal is being reopened.")
		}
		prob := in.Stage.DefaultProbability()
		if in.Probability != nil && !in.Stage.Closed() {
			prob = *in.Probability
		}
		if prob < 0 || prob > 100 {
			fe.Add("probability", "Probability must be between 0 and 100.")
		}
		if err := fe.Err(); err != nil {
			return audit.Entry{}, err
		}
		if in.Stage == StageWon {
			if _, err := s.clients.RequireActive(ctx, d.clientID); err != nil {
				return audit.Entry{}, err
			}
		}

		now := s.now()
		d.Stage, d.Probability, d.StageChangedAt = in.Stage, prob, now
		d.LostReason, d.ClosedAt = nil, nil
		switch in.Stage {
		case StageWon:
			d.ClosedAt = &now
		case StageLost:
			d.ClosedAt, d.LostReason = &now, &reason
		}

		e := audit.Entry{EntityType: entityType, EntityID: d.ID, Action: "stage_changed",
			Summary: "Moved from " + stageLabel(from) + " to " + stageLabel(in.Stage),
			Changes: map[string]any{"stage": map[string]any{"from": from, "to": in.Stage}}}
		switch {
		case in.Stage == StageWon:
			e.Action, e.Summary = "won", "Deal won 🎉"
		case in.Stage == StageLost:
			e.Action, e.Summary, e.Reason = "lost", "Deal lost", reason
		case reopening:
			e.Action, e.Summary, e.Reason = "reopened", "Deal reopened at "+stageLabel(in.Stage), reason
		}
		return e, nil
	}, &updated)
	if err != nil {
		return Deal{}, err
	}
	name := EventStageChanged
	switch updated.Stage {
	case StageWon:
		name = EventWon
	case StageLost:
		name = EventLost
	}
	s.bus.Publish(ctx, events.Event{Name: name, Payload: map[string]any{"deal": updated, "from": from}})
	return updated, nil
}

// AddNote logs a call, meeting, visit, email or note. Anyone who can work on
// deals may add one, so colleagues can record what they did for a client.
func (s *Service) AddNote(ctx context.Context, dealID uuid.UUID, in NoteInput) (Note, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return Note{}, httpx.ErrUnauthorized
	}
	in.Body = strings.TrimSpace(in.Body)
	if in.Kind == "" {
		in.Kind = "note"
	}
	fe := httpx.FieldErrors{}
	if !slices.Contains(NoteKinds, in.Kind) {
		fe.Add("kind", "Choose note, call, meeting, visit or email.")
	}
	if in.Body == "" {
		fe.Add("body", "Write what happened.")
	} else if len(in.Body) > 4000 {
		fe.Add("body", "Keep it under 4,000 characters.")
	}
	if err := fe.Err(); err != nil {
		return Note{}, err
	}
	var noteID uuid.UUID
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := getDeal(ctx, tx, dealID, true); err != nil {
			if db.IsNoRows(err) {
				return httpx.ErrNotFound
			}
			return err
		}
		var err error
		if noteID, err = insertNote(ctx, tx, dealID, actor.UserID, in); err != nil {
			return err
		}
		// Recent activity keeps the deal from looking neglected.
		_, err = tx.Exec(ctx, `UPDATE deals SET updated_at = now() WHERE id = $1`, dealID)
		return err
	})
	if err != nil {
		return Note{}, err
	}
	notes, err := listNotes(ctx, s.pool, dealID)
	if err != nil {
		return Note{}, err
	}
	if d, err := s.Get(ctx, dealID); err == nil {
		label := map[string]string{"note": "a note", "call": "a call", "meeting": "a meeting", "visit": "a site visit", "email": "an email"}[in.Kind]
		s.tellOwner(ctx, d, KindDealNote, actor.Name+" logged "+label+" on "+d.Title, truncate(in.Body, 160))
	}
	for _, n := range notes {
		if n.ID == noteID {
			return n, nil
		}
	}
	return Note{}, httpx.ErrNotFound
}

// ---- internals ----

// mutate locks the deal, checks the version and the caller's right to change
// it, applies fn, saves and records the audit entry in one transaction.
func (s *Service) mutate(ctx context.Context, id uuid.UUID, version int,
	fn func(tx pgx.Tx, actor auth.Principal, d *Deal) (audit.Entry, error), out *Deal) error {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return httpx.ErrUnauthorized
	}
	if version < 1 {
		return httpx.FieldErrors{"version": "Version is required."}.Err()
	}
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		d, err := getDeal(ctx, tx, id, true)
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if d.Owner.ID != actor.UserID && !actor.Can(PermManage) {
			return errNotYours
		}
		if d.Version != version {
			return httpx.ErrStaleVersion.With(map[string]any{"current_version": d.Version})
		}
		entry, err := fn(tx, actor, &d)
		if err != nil {
			return err
		}
		if err := updateDeal(ctx, tx, d); err != nil {
			return err
		}
		return audit.Record(ctx, tx, entry)
	})
	if err != nil {
		return err
	}
	*out, err = s.Get(ctx, id)
	return err
}

func stageLabel(s Stage) string {
	switch s {
	case StageLead:
		return "Lead"
	case StageQualified:
		return "Qualified"
	case StageProposal:
		return "Proposal"
	case StageNegotiation:
		return "Negotiation"
	case StageWon:
		return "Won"
	case StageLost:
		return "Lost"
	}
	return string(s)
}

func snapshot(d Deal) map[string]any {
	str := func(s *string) any {
		if s == nil {
			return nil
		}
		return *s
	}
	date := func(x *Date) any {
		if x == nil {
			return nil
		}
		return string(*x)
	}
	return map[string]any{
		"title": d.Title, "description": str(d.Description), "value": str(d.Value), "currency": d.Currency,
		"probability": d.Probability, "expected_close": date(d.ExpectedClose), "source": str(d.Source),
		"next_step": str(d.NextStep), "next_step_date": date(d.NextStepDate), "owner_id": d.Owner.ID.String(),
	}
}

func truncate(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}
