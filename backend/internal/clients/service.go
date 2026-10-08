package clients

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dos-bpas/internal/platform/audit"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/events"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/numbering"
)

// Events published by this module.
const (
	EventRegistered  = "client.registered"
	EventUpdated     = "client.updated"
	EventDeactivated = "client.deactivated"
)

const entityType = "client"

// API is what other modules may use. Quotations, orders and invoices call
// RequireActive before issuing a document (brief 4.1: every client must be
// registered before a document is issued).
type API interface {
	Get(ctx context.Context, id uuid.UUID) (Client, error)
	RequireActive(ctx context.Context, id uuid.UUID) (Client, error)
	// Lookup returns the name and reference of each client, for lists in other modules.
	Lookup(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Ref, error)
	// MatchIDs returns clients whose name, reference or TPIN matches the search text.
	MatchIDs(ctx context.Context, query string, limit int) ([]uuid.UUID, error)
}

// Ref is the short form of a client that other modules show beside their records.
type Ref struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	LegalName string    `json:"legal_name"`
	Sector    *string   `json:"sector"`
	Status    Status    `json:"status"`
}

var ErrInactive = httpx.NewError(http.StatusConflict, "client_inactive",
	"This client is inactive. Reactivate it before issuing documents.")

type Service struct {
	pool    *pgxpool.Pool
	numbers *numbering.Service
	bus     *events.Bus
	now     func() time.Time
	tz      string // business timezone name, for month-based figures
}

func NewService(pool *pgxpool.Pool, numbers *numbering.Service, bus *events.Bus, now func() time.Time) *Service {
	return &Service{pool: pool, numbers: numbers, bus: bus, now: now, tz: "Africa/Lusaka"}
}

var _ API = (*Service)(nil)

// ---- queries ----

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Client, error) {
	c, err := getClient(ctx, s.pool, id, false)
	if db.IsNoRows(err) {
		return Client{}, httpx.ErrNotFound
	}
	if err != nil {
		return Client{}, err
	}
	if c.Contacts, err = listContacts(ctx, s.pool, id); err != nil {
		return Client{}, err
	}
	if c.Sites, err = listSites(ctx, s.pool, id); err != nil {
		return Client{}, err
	}
	return c, nil
}

func (s *Service) RequireActive(ctx context.Context, id uuid.UUID) (Client, error) {
	c, err := s.Get(ctx, id)
	if err != nil {
		return Client{}, err
	}
	if c.Status != StatusActive {
		return Client{}, ErrInactive
	}
	return c, nil
}

func (s *Service) Lookup(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]Ref, error) {
	return lookupRefs(ctx, s.pool, ids)
}

func (s *Service) MatchIDs(ctx context.Context, query string, limit int) ([]uuid.UUID, error) {
	return matchIDs(ctx, s.pool, query, limit)
}

func (s *Service) List(ctx context.Context, f ListFilter) (httpx.List[Client], error) {
	f.TZ = s.tz
	items, total, err := listClients(ctx, s.pool, f)
	if err != nil {
		return httpx.List[Client]{}, err
	}
	return httpx.List[Client]{Items: items, Total: total, Limit: f.Page.Limit, Offset: f.Page.Offset}, nil
}

func (s *Service) CheckDuplicates(ctx context.Context, legalName string, tpin, regNo *string) ([]PossibleDuplicate, error) {
	return findDuplicates(ctx, s.pool, NameKey(legalName), normID(clean(tpin)), normID(clean(regNo)), nil)
}

func (s *Service) Activity(ctx context.Context, id uuid.UUID) ([]audit.Event, error) {
	return audit.Timeline(ctx, s.pool, entityType, id, 200)
}

// ---- commands ----

func (s *Service) Create(ctx context.Context, in CreateInput) (Client, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return Client{}, httpx.ErrUnauthorized
	}

	in.LegalName = strings.TrimSpace(in.LegalName)
	in.TradingName, in.Sector, in.BillingAddress, in.Notes =
		clean(in.TradingName), clean(in.Sector), clean(in.BillingAddress), clean(in.Notes)
	in.TPIN, in.RegistrationNo = normID(clean(in.TPIN)), normID(clean(in.RegistrationNo))
	in.CreditLimit = clean(in.CreditLimit)
	in.DefaultCurrency = strings.ToUpper(strings.TrimSpace(in.DefaultCurrency))
	if in.DefaultCurrency == "" {
		in.DefaultCurrency = "ZMW"
	}
	terms := 30 // brief: established clients default to 30-day terms
	if in.CreditTermsDays != nil {
		terms = *in.CreditTermsDays
	}
	owner := actor.UserID
	if in.OwnerID != nil {
		owner = *in.OwnerID
	}

	fe := httpx.FieldErrors{}
	if in.LegalName == "" {
		fe.Add("legal_name", "Legal name is required.")
	}
	validateCommon(fe, nil, &in.DefaultCurrency, &terms, in.CreditLimit, in.TPIN)
	primaries := 0
	for i, c := range in.Contacts {
		validateContact(fe, "contacts."+strconv.Itoa(i)+".", c)
		if c.IsPrimary {
			primaries++
		}
	}
	if primaries > 1 {
		fe.Add("contacts", "Only one contact can be the primary contact.")
	}
	for i, st := range in.Sites {
		validateSite(fe, "sites."+strconv.Itoa(i)+".", st)
	}
	if in.AllowDuplicate {
		if !actor.Can(PermOverrideDuplicate) {
			return Client{}, httpx.ErrForbidden
		}
		if strings.TrimSpace(in.DuplicateReason) == "" {
			fe.Add("duplicate_reason", "Give a reason for registering a possible duplicate.")
		}
	}
	if err := fe.Err(); err != nil {
		return Client{}, err
	}

	var id uuid.UUID
	var ref string
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		ok, err := activeUserExists(ctx, tx, owner)
		if err != nil {
			return err
		}
		if !ok {
			return httpx.FieldErrors{"owner_id": "Choose an active staff member."}.Err()
		}

		dups, err := findDuplicates(ctx, tx, NameKey(in.LegalName), in.TPIN, in.RegistrationNo, nil)
		if err != nil {
			return err
		}
		if len(dups) > 0 && !in.AllowDuplicate {
			return httpx.NewError(http.StatusConflict, "possible_duplicate",
				"This client may already be registered.").With(map[string]any{"matches": dups})
		}

		if ref, err = s.numbers.Next(ctx, tx, numbering.Client); err != nil {
			return err
		}
		var dupReason *string
		if in.AllowDuplicate && len(dups) > 0 {
			r := strings.TrimSpace(in.DuplicateReason)
			dupReason = &r
		}
		id, err = insertClient(ctx, tx, clientRow{
			Reference: ref, LegalName: in.LegalName, NameKey: NameKey(in.LegalName), Currency: in.DefaultCurrency,
			TradingName: in.TradingName, RegNo: in.RegistrationNo, TPIN: in.TPIN, Sector: in.Sector,
			BillingAddress: in.BillingAddress, CreditLimit: in.CreditLimit, Notes: in.Notes,
			DuplicateReason: dupReason, CreditTermsDays: terms, OwnerID: owner, CreatedBy: actor.UserID,
		})
		if err != nil {
			return err
		}
		for _, c := range in.Contacts {
			if _, err := insertContact(ctx, tx, id, c); err != nil {
				return err
			}
		}
		for _, st := range in.Sites {
			if _, err := insertSite(ctx, tx, id, st); err != nil {
				return err
			}
		}

		entry := audit.Entry{EntityType: entityType, EntityID: id, Action: "created",
			Summary: "Client " + ref + " – " + in.LegalName + " registered"}
		if dupReason != nil {
			entry.Action = "created_duplicate_override"
			entry.Reason = *dupReason
			entry.Changes = map[string]any{"possible_duplicates": dups}
		}
		return audit.Record(ctx, tx, entry)
	})
	if err != nil {
		return Client{}, err
	}

	c, err := s.Get(ctx, id)
	if err != nil {
		return Client{}, err
	}
	s.bus.Publish(ctx, events.Event{Name: EventRegistered, Payload: c})
	return c, nil
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (Client, error) {
	var updated Client
	err := s.mutate(ctx, id, in.Version, func(tx pgx.Tx, c *Client) (audit.Entry, error) {
		before := snapshot(*c)
		ownerChanged := in.OwnerID != nil && *in.OwnerID != c.Owner.ID

		if in.LegalName != nil {
			c.LegalName = strings.TrimSpace(*in.LegalName)
		}
		if in.TradingName != nil {
			c.TradingName = clean(in.TradingName)
		}
		if in.RegistrationNo != nil {
			c.RegistrationNo = normID(clean(in.RegistrationNo))
		}
		if in.TPIN != nil {
			c.TPIN = normID(clean(in.TPIN))
		}
		if in.Sector != nil {
			c.Sector = clean(in.Sector)
		}
		if in.BillingAddress != nil {
			c.BillingAddress = clean(in.BillingAddress)
		}
		if in.DefaultCurrency != nil {
			c.DefaultCurrency = strings.ToUpper(strings.TrimSpace(*in.DefaultCurrency))
		}
		if in.CreditTermsDays != nil {
			c.CreditTermsDays = *in.CreditTermsDays
		}
		if in.CreditLimit != nil {
			c.CreditLimit = clean(in.CreditLimit)
		}
		if in.Notes != nil {
			c.Notes = clean(in.Notes)
		}

		fe := httpx.FieldErrors{}
		validateCommon(fe, &c.LegalName, &c.DefaultCurrency, &c.CreditTermsDays, c.CreditLimit, c.TPIN)
		if ownerChanged {
			if strings.TrimSpace(in.Reason) == "" {
				fe.Add("reason", "Give a reason for the handover.")
			}
			ok, err := activeUserExists(ctx, tx, *in.OwnerID)
			if err != nil {
				return audit.Entry{}, err
			}
			if !ok {
				fe.Add("owner_id", "Choose an active staff member.")
			}
			c.Owner.ID = *in.OwnerID
		}
		if err := fe.Err(); err != nil {
			return audit.Entry{}, err
		}

		// Tax and registration numbers must stay unique; a similar name is allowed on edit.
		dups, err := findDuplicates(ctx, tx, "", c.TPIN, c.RegistrationNo, &c.ID)
		if err != nil {
			return audit.Entry{}, err
		}
		for _, d := range dups {
			for _, m := range d.MatchedOn {
				fe.Add(m, "Already used by "+d.Reference+" – "+d.LegalName+".")
			}
		}
		if err := fe.Err(); err != nil {
			return audit.Entry{}, err
		}

		changes := audit.Diff(before, snapshot(*c))
		if len(changes) == 0 {
			return audit.Entry{}, errNoChange
		}
		e := audit.Entry{EntityType: entityType, EntityID: c.ID, Action: "updated",
			Summary: "Client details updated", Changes: changes}
		if ownerChanged {
			e.Action = "reassigned"
			e.Summary = "Client handed over to a new owner"
			e.Reason = strings.TrimSpace(in.Reason)
		}
		return e, nil
	}, &updated)
	if err != nil {
		return Client{}, err
	}
	s.bus.Publish(ctx, events.Event{Name: EventUpdated, Payload: updated})
	return updated, nil
}

func (s *Service) Deactivate(ctx context.Context, id uuid.UUID, version int, reason string) (Client, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Client{}, httpx.FieldErrors{"reason": "Give a reason for deactivating this client."}.Err()
	}
	var updated Client
	err := s.mutate(ctx, id, version, func(_ pgx.Tx, c *Client) (audit.Entry, error) {
		if c.Status == StatusInactive {
			return audit.Entry{}, errNoChange
		}
		now := s.now()
		c.Status, c.DeactivatedAt, c.DeactivationReason = StatusInactive, &now, &reason
		return audit.Entry{EntityType: entityType, EntityID: c.ID, Action: "deactivated",
			Summary: "Client deactivated", Reason: reason}, nil
	}, &updated)
	if err != nil {
		return Client{}, err
	}
	s.bus.Publish(ctx, events.Event{Name: EventDeactivated, Payload: updated})
	return updated, nil
}

func (s *Service) Reactivate(ctx context.Context, id uuid.UUID, version int) (Client, error) {
	var updated Client
	err := s.mutate(ctx, id, version, func(_ pgx.Tx, c *Client) (audit.Entry, error) {
		if c.Status == StatusActive {
			return audit.Entry{}, errNoChange
		}
		c.Status, c.DeactivatedAt, c.DeactivationReason = StatusActive, nil, nil
		return audit.Entry{EntityType: entityType, EntityID: c.ID, Action: "reactivated",
			Summary: "Client reactivated"}, nil
	}, &updated)
	return updated, err
}

func (s *Service) AddContact(ctx context.Context, clientID uuid.UUID, in ContactInput) (Contact, error) {
	fe := httpx.FieldErrors{}
	validateContact(fe, "", in)
	if err := fe.Err(); err != nil {
		return Contact{}, err
	}
	var out Contact
	err := s.childChange(ctx, clientID, func(tx pgx.Tx) (audit.Entry, error) {
		var err error
		if out, err = insertContact(ctx, tx, clientID, in); err != nil {
			return audit.Entry{}, err
		}
		return audit.Entry{EntityType: entityType, EntityID: clientID, Action: "contact_added",
			Summary: "Contact " + out.Name + " added"}, nil
	})
	return out, err
}

func (s *Service) UpdateContact(ctx context.Context, clientID, contactID uuid.UUID, in ContactInput, active bool) (Contact, error) {
	fe := httpx.FieldErrors{}
	validateContact(fe, "", in)
	if err := fe.Err(); err != nil {
		return Contact{}, err
	}
	var out Contact
	err := s.childChange(ctx, clientID, func(tx pgx.Tx) (audit.Entry, error) {
		var err error
		out, err = updateContact(ctx, tx, clientID, contactID, in, active)
		if db.IsNoRows(err) {
			return audit.Entry{}, httpx.ErrNotFound
		}
		if err != nil {
			return audit.Entry{}, err
		}
		return audit.Entry{EntityType: entityType, EntityID: clientID, Action: "contact_updated",
			Summary: "Contact " + out.Name + " updated"}, nil
	})
	return out, err
}

func (s *Service) AddSite(ctx context.Context, clientID uuid.UUID, in SiteInput) (Site, error) {
	fe := httpx.FieldErrors{}
	validateSite(fe, "", in)
	if err := fe.Err(); err != nil {
		return Site{}, err
	}
	var out Site
	err := s.childChange(ctx, clientID, func(tx pgx.Tx) (audit.Entry, error) {
		var err error
		if out, err = insertSite(ctx, tx, clientID, in); err != nil {
			return audit.Entry{}, err
		}
		return audit.Entry{EntityType: entityType, EntityID: clientID, Action: "site_added",
			Summary: "Delivery site " + out.Name + " added"}, nil
	})
	return out, err
}

// ---- internals ----

var errNoChange = httpx.NewError(http.StatusUnprocessableEntity, "no_change", "Nothing was changed.")

// mutate locks the client row, checks the optimistic-lock version, applies fn,
// saves, and records the audit entry in one transaction.
func (s *Service) mutate(ctx context.Context, id uuid.UUID, version int,
	fn func(tx pgx.Tx, c *Client) (audit.Entry, error), out *Client) error {
	if version < 1 {
		return httpx.FieldErrors{"version": "Version is required."}.Err()
	}
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := getClient(ctx, tx, id, true)
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if c.Version != version {
			return httpx.ErrStaleVersion.With(map[string]any{"current_version": c.Version})
		}
		entry, err := fn(tx, &c)
		if err != nil {
			return err
		}
		if err := updateClient(ctx, tx, c); err != nil {
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

// childChange locks the parent client for contact/site edits and bumps its
// updated_at so the change shows as recent activity.
func (s *Service) childChange(ctx context.Context, clientID uuid.UUID, fn func(tx pgx.Tx) (audit.Entry, error)) error {
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
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
		entry, err := fn(tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE clients SET updated_at = now() WHERE id = $1`, clientID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, entry)
	})
}

func snapshot(c Client) map[string]any {
	deref := func(s *string) any {
		if s == nil {
			return nil
		}
		return *s
	}
	return map[string]any{
		"legal_name": c.LegalName, "trading_name": deref(c.TradingName),
		"registration_no": deref(c.RegistrationNo), "tpin": deref(c.TPIN), "sector": deref(c.Sector),
		"billing_address": deref(c.BillingAddress), "default_currency": c.DefaultCurrency,
		"credit_terms_days": c.CreditTermsDays, "credit_limit": deref(c.CreditLimit),
		"notes": deref(c.Notes), "owner_id": c.Owner.ID.String(), "status": string(c.Status),
	}
}
