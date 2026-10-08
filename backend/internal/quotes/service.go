package quotes

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dos-bpas/internal/clients"
	"dos-bpas/internal/deals"
	"dos-bpas/internal/platform/audit"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/config"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/httpx"
	mailer "dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/notify"
	"dos-bpas/internal/platform/numbering"
)

const entityType = "quote"

const (
	KindApprovalRequested = "quote_approval_requested"
	KindApproved          = "quote_approved"
	KindReturned          = "quote_returned"
	KindDecided           = "quote_decided"
	KindExpiring          = "quote_expiring"
	KindExpired           = "quote_expired"
)

var (
	errNoChange = httpx.NewError(http.StatusUnprocessableEntity, "no_change", "Nothing was changed.")
	errNotYours = httpx.NewError(http.StatusForbidden, "not_your_quote",
		"Only the person who prepared this quotation or a sales manager can change it.")
	errNoLines = httpx.NewError(http.StatusUnprocessableEntity, "no_lines", "Add at least one line before continuing.")
)

func wrongStatus(s Status, doing string) error {
	return httpx.NewError(http.StatusConflict, "wrong_status",
		fmt.Sprintf("A quotation that is %s can't be %s.", statusLabel(s), doing)).With(map[string]any{"status": s})
}

type Service struct {
	pool          *pgxpool.Pool
	numbers       *numbering.Service
	clients       clients.API
	deals         deals.API
	mail          *mailer.Service
	log           *slog.Logger
	now           func() time.Time
	loc           *time.Location
	company       config.Company
	approvalLimit hundredths // discount percentage ×100 above which a manager must approve
	validDays     int
	appURL        string
}

type Options struct {
	Company          config.Company
	ApprovalDiscount float64
	ValidDays        int
	AppURL           string
	Location         *time.Location
	Log              *slog.Logger
}

func NewService(pool *pgxpool.Pool, numbers *numbering.Service, cl clients.API, dl deals.API, m *mailer.Service, now func() time.Time, o Options) *Service {
	if o.Location == nil {
		o.Location, _ = time.LoadLocation("Africa/Lusaka")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.ValidDays == 0 {
		o.ValidDays = 30
	}
	if o.Company.Name == "" {
		o.Company.Name = "Digital Office Solutions Limited"
	}
	return &Service{pool: pool, numbers: numbers, clients: cl, deals: dl, mail: m, log: o.Log, now: now, loc: o.Location,
		company: o.Company, approvalLimit: hundredths(o.ApprovalDiscount*100 + 0.5), validDays: o.ValidDays,
		appURL: strings.TrimRight(o.AppURL, "/")}
}

// ApprovalLimit is the discount percentage above which a manager must approve.
func (s *Service) ApprovalLimit() string { return s.approvalLimit.Trimmed() }

func (s *Service) today() time.Time {
	t := s.now().In(s.loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// ---- queries ----

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Quote, error) {
	q, err := getQuote(ctx, s.pool, id, false)
	if db.IsNoRows(err) {
		return Quote{}, httpx.ErrNotFound
	}
	if err != nil {
		return Quote{}, err
	}
	if q.Lines, err = listLines(ctx, s.pool, id); err != nil {
		return Quote{}, err
	}
	if q.Revisions, err = revisions(ctx, s.pool, q.Reference); err != nil {
		return Quote{}, err
	}
	if err := s.attach(ctx, []*Quote{&q}, true); err != nil {
		return Quote{}, err
	}
	return q, nil
}

func (s *Service) List(ctx context.Context, f ListFilter) (httpx.List[Quote], error) {
	var matches []uuid.UUID
	if strings.TrimSpace(f.Query) != "" {
		var err error
		if matches, err = s.clients.MatchIDs(ctx, f.Query, 500); err != nil {
			return httpx.List[Quote]{}, err
		}
	}
	items, total, err := listQuotes(ctx, s.pool, f, matches)
	if err != nil {
		return httpx.List[Quote]{}, err
	}
	ptrs := make([]*Quote, len(items))
	for i := range items {
		ptrs[i] = &items[i]
	}
	if err := s.attach(ctx, ptrs, false); err != nil {
		return httpx.List[Quote]{}, err
	}
	return httpx.List[Quote]{Items: items, Total: total, Limit: f.Limit, Offset: f.Offset}, nil
}

// attach fills in the client (and, on the detail, the contact and deal) from their modules.
func (s *Service) attach(ctx context.Context, qs []*Quote, detail bool) error {
	ids := make([]uuid.UUID, 0, len(qs))
	for _, q := range qs {
		ids = append(ids, q.clientID)
		q.NeedsApproval = s.needsApproval(q)
	}
	refs, err := s.clients.Lookup(ctx, ids)
	if err != nil {
		return err
	}
	for _, q := range qs {
		r := refs[q.clientID]
		q.Client = ClientRef{ID: q.clientID, Reference: r.Reference, LegalName: r.LegalName, Status: string(r.Status)}
		if !detail {
			continue
		}
		if q.contactID != nil {
			c, err := s.clients.Get(ctx, q.clientID)
			if err != nil {
				return err
			}
			for _, k := range c.Contacts {
				if k.ID == *q.contactID {
					q.Contact = &ContactRef{ID: k.ID, Name: k.Name, Position: k.Position, Email: k.Email, Phone: k.Phone}
				}
			}
		}
		if q.dealID != nil {
			if d, err := s.deals.Get(ctx, *q.dealID); err == nil {
				q.Deal = &DealRef{ID: d.ID, Reference: d.Reference, Title: d.Title, Stage: string(d.Stage)}
			}
		}
	}
	return nil
}

func (s *Service) needsApproval(q *Quote) bool {
	d, _ := parseHundredths(q.DiscountPct)
	m, _ := parseHundredths(q.MaxLineDiscount)
	return d > s.approvalLimit || m > s.approvalLimit
}

// ---- totals ----

type lineRow struct {
	LineInput
	qty, price, disc, vat      hundredths
	gross, net, lineVAT, total hundredths
}

type computed struct {
	lines                               []lineRow
	subtotal, discount, net, vat, total hundredths
	discountPct, maxLineDisc            hundredths
}

const maxLines = 200

// compute checks and prices every line, adding problems to fe.
func compute(in []LineInput, fe httpx.FieldErrors) computed {
	var c computed
	if len(in) > maxLines {
		fe.Add("lines", "A quotation can have up to 200 lines.")
		return c
	}
	for i, l := range in {
		p := "lines." + strconv.Itoa(i) + "."
		l.Description = strings.TrimSpace(l.Description)
		l.SKU = clean(l.SKU)
		l.Unit = strings.TrimSpace(l.Unit)
		if l.Unit == "" {
			l.Unit = "each"
		}
		r := lineRow{LineInput: l}
		var err error
		if l.Description == "" {
			fe.Add(p+"description", "Describe the item.")
		} else if utf8.RuneCountInString(l.Description) > 2000 {
			fe.Add(p+"description", "Keep the description under 2,000 characters.")
		}
		if r.qty, err = parseHundredths(l.Quantity); err != nil || r.qty == 0 {
			fe.Add(p+"quantity", "Enter a quantity above 0.")
		}
		if r.price, err = parseHundredths(l.UnitPrice); err != nil {
			fe.Add(p+"unit_price", "Enter a price like 12500 or 12500.00.")
		}
		if strings.TrimSpace(l.DiscountPct) == "" {
			l.DiscountPct = "0"
		}
		if r.disc, err = parseHundredths(l.DiscountPct); err != nil || r.disc > 10000 {
			fe.Add(p+"discount_pct", "Enter a discount between 0 and 100%.")
		}
		if strings.TrimSpace(l.VATRate) == "" {
			l.VATRate = "16"
		}
		if r.vat, err = parseHundredths(l.VATRate); err != nil || r.vat > 10000 {
			fe.Add(p+"vat_rate", "Enter a VAT rate between 0 and 100%.")
		}
		r.LineInput = l
		r.gross, r.net, r.lineVAT, r.total = lineAmounts(r.qty, r.price, r.disc, r.vat)
		c.lines = append(c.lines, r)
		c.subtotal += r.gross
		c.net += r.net
		c.vat += r.lineVAT
		c.total += r.total
		if r.disc > c.maxLineDisc {
			c.maxLineDisc = r.disc
		}
	}
	c.discount = c.subtotal - c.net
	c.discountPct = percentOf(c.discount, c.subtotal)
	return c
}

// ---- commands ----

func (s *Service) Create(ctx context.Context, in QuoteInput) (Quote, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return Quote{}, httpx.ErrUnauthorized
	}
	cl, err := s.clients.RequireActive(ctx, in.ClientID)
	if err != nil {
		if err == httpx.ErrNotFound {
			return Quote{}, httpx.FieldErrors{"client_id": "Choose a registered client."}.Err()
		}
		return Quote{}, err
	}
	if strings.TrimSpace(in.Currency) == "" {
		in.Currency = cl.DefaultCurrency
	}
	if strings.TrimSpace(in.ValidUntil) == "" {
		in.ValidUntil = s.today().AddDate(0, 0, s.validDays).Format("2006-01-02")
	}
	def := s.DefaultTerms(cl.CreditTermsDays)
	for _, f := range []struct {
		dst **string
		v   string
	}{
		{&in.DeliveryTerms, def.Delivery}, {&in.PaymentTerms, def.Payment}, {&in.WarrantyTerms, def.Warranty},
	} {
		if *f.dst == nil {
			v := f.v
			*f.dst = &v
		}
	}
	h, err := s.checkHeader(ctx, in, cl)
	if err != nil {
		return Quote{}, err
	}
	var id uuid.UUID
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.checkProducts(ctx, tx, h.totals.lines); err != nil {
			return err
		}
		ref, err := s.numbers.Next(ctx, tx, numbering.Quotation)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO quotes (reference, client_id, title, currency, quote_date, valid_until, owner_id, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$7) RETURNING id`,
			ref, h.ClientID, h.Title, h.Currency, s.today(), h.ValidUntil, actor.UserID).Scan(&id); err != nil {
			return err
		}
		if err := saveTotals(ctx, tx, id, h); err != nil {
			return err
		}
		if err := replaceLines(ctx, tx, id, h.totals.lines); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: entityType, EntityID: id, Action: "created",
			Summary: "Quotation " + ref + " started for " + cl.LegalName})
	})
	if err != nil {
		return Quote{}, err
	}
	return s.Get(ctx, id)
}

// Update replaces a draft's details and lines. Changing a quotation that is
// waiting for approval takes it back to draft.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in QuoteInput) (Quote, error) {
	err := s.mutate(ctx, id, in.Version, func(tx pgx.Tx, actor auth.Principal, q *Quote) (audit.Entry, error) {
		if !q.Status.Editable() {
			return audit.Entry{}, wrongStatus(q.Status, "edited — make a revision instead")
		}
		if in.ClientID != q.clientID {
			return audit.Entry{}, httpx.FieldErrors{"client_id": "A quotation's client can't change. Start a new quotation instead."}.Err()
		}
		cl, err := s.clients.RequireActive(ctx, q.clientID)
		if err != nil {
			return audit.Entry{}, err
		}
		if strings.TrimSpace(in.Currency) == "" {
			in.Currency = q.Currency
		}
		if strings.TrimSpace(in.ValidUntil) == "" {
			in.ValidUntil = q.ValidUntil
		}
		h, err := s.checkHeader(ctx, in, cl)
		if err != nil {
			return audit.Entry{}, err
		}
		if err := s.checkProducts(ctx, tx, h.totals.lines); err != nil {
			return audit.Entry{}, err
		}
		before := q.Total
		if err := saveTotals(ctx, tx, id, h); err != nil {
			return audit.Entry{}, err
		}
		if err := replaceLines(ctx, tx, id, h.totals.lines); err != nil {
			return audit.Entry{}, err
		}
		e := audit.Entry{EntityType: entityType, EntityID: id, Action: "updated", Summary: "Quotation updated",
			Changes: map[string]any{"total": map[string]any{"from": before, "to": h.totals.total.String()}, "lines": len(h.totals.lines)}}
		if q.Status == StatusPendingApproval {
			if _, err := tx.Exec(ctx, `UPDATE quotes SET status='draft', submitted_at=NULL WHERE id=$1`, id); err != nil {
				return audit.Entry{}, err
			}
			e.Summary = "Quotation changed while waiting for approval — back to draft"
		}
		return e, nil
	})
	if err != nil {
		return Quote{}, err
	}
	return s.Get(ctx, id)
}

func (s *Service) checkHeader(ctx context.Context, in QuoteInput, cl clients.Client) (header, error) {
	fe := httpx.FieldErrors{}
	h := header{ClientID: cl.ID, DealID: in.DealID, ContactID: in.ContactID, Title: strings.TrimSpace(in.Title),
		Currency: strings.ToUpper(strings.TrimSpace(in.Currency)), Notes: clean(in.Notes), Terms: clean(in.Terms),
		InternalNote: clean(in.InternalNote), Delivery: clean(in.DeliveryTerms), Payment: clean(in.PaymentTerms),
		Warranty: clean(in.WarrantyTerms)}
	for _, t := range []*string{h.Delivery, h.Payment, h.Warranty} {
		if t != nil && utf8.RuneCountInString(*t) > 200 {
			fe.Add("terms", "Keep delivery, payment and warranty terms short — under 200 characters each.")
		}
	}
	if h.Title == "" {
		fe.Add("title", "Give the quotation a short title, e.g. Supply of 3 colour copiers.")
	} else if utf8.RuneCountInString(h.Title) > 200 {
		fe.Add("title", "Keep the title under 200 characters.")
	}
	if !currencyOK(h.Currency) {
		fe.Add("currency", "Use a 3-letter currency code, e.g. ZMW or USD.")
	}
	vu, err := time.Parse("2006-01-02", strings.TrimSpace(in.ValidUntil))
	switch {
	case err != nil:
		fe.Add("valid_until", "Enter the date the quotation is valid until.")
	case vu.Before(s.today()):
		fe.Add("valid_until", "The validity date can't be in the past.")
	case vu.After(s.today().AddDate(1, 0, 0)):
		fe.Add("valid_until", "Keep the validity within a year.")
	}
	h.ValidUntil = vu
	for _, t := range []*string{h.Notes, h.Terms, h.InternalNote} {
		if t != nil && utf8.RuneCountInString(*t) > 4000 {
			fe.Add("notes", "Notes and terms must each be under 4,000 characters.")
		}
	}
	if h.ContactID != nil {
		found := false
		for _, k := range cl.Contacts {
			found = found || (k.ID == *h.ContactID && k.Active)
		}
		if !found {
			fe.Add("contact_id", "Choose one of this client's contacts.")
		}
	}
	if h.DealID != nil {
		d, err := s.deals.Get(ctx, *h.DealID)
		switch {
		case err == httpx.ErrNotFound:
			fe.Add("deal_id", "That deal doesn't exist.")
		case err != nil:
			return h, err
		case d.Client.ID != cl.ID:
			fe.Add("deal_id", "Choose one of this client's deals.")
		case d.Stage == deals.StageLost:
			fe.Add("deal_id", "That deal is lost. Reopen it first, or leave the deal empty.")
		}
	}
	h.totals = compute(in.Lines, fe)
	return h, fe.Err()
}

func (s *Service) checkProducts(ctx context.Context, tx pgx.Tx, lines []lineRow) error {
	var ids []uuid.UUID
	for _, l := range lines {
		if l.ProductID != nil {
			ids = append(ids, *l.ProductID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT id) FROM products WHERE id = ANY($1)`, ids).Scan(&n); err != nil {
		return err
	}
	distinct := map[uuid.UUID]bool{}
	for _, id := range ids {
		distinct[id] = true
	}
	if n != len(distinct) {
		return httpx.FieldErrors{"lines": "A line refers to an item that isn't on the price list."}.Err()
	}
	return nil
}

// Submit finishes a draft. Within the discount limit, or prepared by someone
// who may approve, it is ready to send; otherwise it waits for a manager.
func (s *Service) Submit(ctx context.Context, id uuid.UUID, version int) (Quote, error) {
	var notifyApprovers *Quote
	err := s.mutate(ctx, id, version, func(tx pgx.Tx, actor auth.Principal, q *Quote) (audit.Entry, error) {
		if q.Status != StatusDraft {
			return audit.Entry{}, wrongStatus(q.Status, "submitted")
		}
		if err := s.checkSendable(ctx, tx, q); err != nil {
			return audit.Entry{}, err
		}
		if q.NeedsApproval && !actor.Can(PermApprove) {
			if _, err := tx.Exec(ctx, `UPDATE quotes SET status='pending_approval', submitted_at=now() WHERE id=$1`, id); err != nil {
				return audit.Entry{}, err
			}
			notifyApprovers = q
			return audit.Entry{EntityType: entityType, EntityID: id, Action: "submitted",
				Summary: fmt.Sprintf("Sent for approval — discount %s%% is above the %s%% limit", maxPct(q), s.approvalLimit.Trimmed())}, nil
		}
		if q.NeedsApproval {
			if _, err := tx.Exec(ctx, `UPDATE quotes SET status='ready', submitted_at=now(), approved_by=$2, approved_at=now(),
				approval_note='Prepared by someone who can approve discounts' WHERE id=$1`, id, actor.UserID); err != nil {
				return audit.Entry{}, err
			}
			return audit.Entry{EntityType: entityType, EntityID: id, Action: "approved",
				Summary: fmt.Sprintf("Ready to send — discount %s%% approved by the preparer", maxPct(q))}, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE quotes SET status='ready', submitted_at=now() WHERE id=$1`, id); err != nil {
			return audit.Entry{}, err
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: "ready", Summary: "Ready to send"}, nil
	})
	if err != nil {
		return Quote{}, err
	}
	out, err := s.Get(ctx, id)
	if err == nil && notifyApprovers != nil {
		s.askApprovers(ctx, out)
	}
	return out, err
}

// Approve or return a quotation waiting for approval.
func (s *Service) Decide(ctx context.Context, id uuid.UUID, version int, approve bool, note string) (Quote, error) {
	actor, _ := auth.FromContext(ctx)
	if !actor.Can(PermApprove) {
		return Quote{}, httpx.ErrForbidden
	}
	note = strings.TrimSpace(note)
	if !approve && note == "" {
		return Quote{}, httpx.FieldErrors{"note": "Say what needs to change, so they can fix it."}.Err()
	}
	err := s.mutateAs(ctx, id, version, true, func(tx pgx.Tx, actor auth.Principal, q *Quote) (audit.Entry, error) {
		if q.Status != StatusPendingApproval {
			return audit.Entry{}, wrongStatus(q.Status, "approved")
		}
		if approve {
			if _, err := tx.Exec(ctx, `UPDATE quotes SET status='ready', approved_by=$2, approved_at=now(), approval_note=NULLIF($3,'')
				WHERE id=$1`, id, actor.UserID, note); err != nil {
				return audit.Entry{}, err
			}
			return audit.Entry{EntityType: entityType, EntityID: id, Action: "approved",
				Summary: fmt.Sprintf("Discount of %s%% approved", maxPct(q)), Reason: note}, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE quotes SET status='draft', submitted_at=NULL, approval_note=$2 WHERE id=$1`, id, note); err != nil {
			return audit.Entry{}, err
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: "returned", Summary: "Returned for changes", Reason: note}, nil
	})
	if err != nil {
		return Quote{}, err
	}
	out, err := s.Get(ctx, id)
	if err != nil {
		return out, err
	}
	if approve {
		s.tell(ctx, out, KindApproved, "Approved: "+out.Number+" can be sent", out.Client.LegalName+" · "+out.Currency+" "+grouped(out.Total))
	} else {
		s.tell(ctx, out, KindReturned, "Changes needed on "+out.Number, actor.Name+": "+note)
	}
	return out, nil
}

// SendInput says how the quotation goes to the client.
type SendInput struct {
	Version int    `json:"version"`
	Email   bool   `json:"email"`   // email the PDF; false records it as delivered another way
	To      string `json:"to"`      // defaults to the quotation's contact
	CopyMe  bool   `json:"copy_me"` // copy the sender
	Message string `json:"message"` // a personal note above the standard text
	Via     string `json:"via"`     // when not emailed: how it was delivered, e.g. "Hand delivered"
}

type SendResult struct {
	Quote   Quote  `json:"quote"`
	Emailed bool   `json:"emailed"`
	Note    string `json:"note,omitempty"`
}

// Send emails the quotation PDF to the client (or records that it was given
// to them another way), and moves a linked deal to Proposal.
func (s *Service) Send(ctx context.Context, id uuid.UUID, in SendInput) (SendResult, error) {
	var res SendResult
	err := s.mutate(ctx, id, in.Version, func(tx pgx.Tx, actor auth.Principal, q *Quote) (audit.Entry, error) {
		if q.Status != StatusReady && q.Status != StatusSent {
			if q.Status == StatusPendingApproval {
				return audit.Entry{}, httpx.NewError(http.StatusConflict, "needs_approval",
					"This quotation's discount needs a sales manager's approval before it can be sent.")
			}
			return audit.Entry{}, wrongStatus(q.Status, "sent")
		}
		today := s.today()
		vu, _ := time.Parse("2006-01-02", q.ValidUntil)
		if vu.Before(today) {
			return audit.Entry{}, httpx.NewError(http.StatusConflict, "validity_passed",
				"This quotation's validity date has passed. Make a revision with a new date.")
		}
		full, err := s.loadForDocument(ctx, tx, q)
		if err != nil {
			return audit.Entry{}, err
		}
		if q.Status == StatusReady {
			full.QuoteDate = today.Format("2006-01-02") // dated the day it goes out
		}
		sentTo := strings.TrimSpace(in.Via)
		if in.Email {
			to := strings.TrimSpace(in.To)
			if to == "" && full.Contact != nil && full.Contact.Email != nil {
				to = *full.Contact.Email
			}
			if _, err := mail.ParseAddress(to); err != nil || to == "" {
				return audit.Entry{}, httpx.FieldErrors{"to": "Enter the email address to send it to."}.Err()
			}
			if utf8.RuneCountInString(in.Message) > 2000 {
				return audit.Entry{}, httpx.FieldErrors{"message": "Keep the message under 2,000 characters."}.Err()
			}
			pdfBytes, err := s.renderPDF(ctx, full)
			if err != nil {
				return audit.Entry{}, err
			}
			msg := s.quoteEmail(full, actor, to, strings.TrimSpace(in.Message))
			msg.Attachments = []mailer.Attachment{{Name: full.Number + ".pdf", ContentType: "application/pdf", Data: pdfBytes}}
			sender := full.Owner
			if actor.UserID != full.Owner.ID {
				sender = Person{Name: actor.Name, Email: ""}
				var email string
				_ = tx.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, actor.UserID).Scan(&email)
				sender.Email = email
			}
			if sender.Email != "" {
				msg.ReplyTo = sender.Email
				if in.CopyMe {
					msg.Cc = []string{sender.Email}
				}
			}
			if err := s.mail.Send(ctx, msg); err != nil {
				return audit.Entry{}, httpx.NewError(http.StatusBadGateway, "email_failed",
					"The email couldn't be sent, so the quotation is still marked as not sent. Check the address, or try again in a moment.")
			}
			res.Emailed = s.mail.Enabled()
			if !res.Emailed {
				res.Note = "Email isn't set up on this server, so the message was written to the server log instead of being sent."
			}
			sentTo = to
		} else if sentTo == "" {
			return audit.Entry{}, httpx.FieldErrors{"via": "Say how it reached the client, e.g. hand delivered or shared on WhatsApp."}.Err()
		}

		if _, err := tx.Exec(ctx, `UPDATE quotes SET status='sent', sent_at=now(), sent_to=$2, quote_date=$3::date WHERE id=$1`,
			id, sentTo, full.QuoteDate); err != nil {
			return audit.Entry{}, err
		}
		if q.dealID != nil {
			if err := s.deals.QuoteSent(ctx, tx, *q.dealID, q.Number, q.Total, q.Currency); err != nil {
				return audit.Entry{}, err
			}
		}
		action, summary := "sent", "Sent to "+sentTo
		if q.Status == StatusSent {
			action, summary = "resent", "Sent again to "+sentTo
		}
		if !in.Email {
			summary = "Marked as sent — " + sentTo
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: action, Summary: summary}, nil
	})
	if err != nil {
		return res, err
	}
	res.Quote, err = s.Get(ctx, id)
	return res, err
}

// Accept records the client's acceptance and wins the linked deal.
func (s *Service) Accept(ctx context.Context, id uuid.UUID, version int, clientPO, note string) (Quote, error) {
	clientPO, note = strings.TrimSpace(clientPO), strings.TrimSpace(note)
	err := s.mutate(ctx, id, version, func(tx pgx.Tx, actor auth.Principal, q *Quote) (audit.Entry, error) {
		if q.Status != StatusSent && q.Status != StatusExpired {
			return audit.Entry{}, wrongStatus(q.Status, "accepted — send it first")
		}
		if _, err := s.clients.RequireActive(ctx, q.clientID); err != nil {
			return audit.Entry{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE quotes SET status='accepted', decided_at=now(), client_po=NULLIF($2,''), decision_note=NULLIF($3,'')
			WHERE id=$1`, id, clientPO, note); err != nil {
			return audit.Entry{}, err
		}
		if q.dealID != nil {
			if err := s.deals.QuoteAccepted(ctx, tx, *q.dealID, q.Number, q.Total, q.Currency); err != nil {
				return audit.Entry{}, err
			}
		}
		summary := "Accepted by the client 🎉"
		if clientPO != "" {
			summary += " — their order no. " + clientPO
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: "accepted", Summary: summary, Reason: note}, nil
	})
	if err != nil {
		return Quote{}, err
	}
	out, err := s.Get(ctx, id)
	if err == nil {
		actor, _ := auth.FromContext(ctx)
		if actor.UserID != out.Owner.ID {
			s.tell(ctx, out, KindDecided, "Accepted: "+out.Number+" 🎉", out.Client.LegalName+" · "+out.Currency+" "+grouped(out.Total))
		}
	}
	return out, err
}

func (s *Service) Decline(ctx context.Context, id uuid.UUID, version int, reason string) (Quote, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Quote{}, httpx.FieldErrors{"reason": "Say why, e.g. price, went with a competitor, or project postponed."}.Err()
	}
	err := s.mutate(ctx, id, version, func(tx pgx.Tx, _ auth.Principal, q *Quote) (audit.Entry, error) {
		if q.Status != StatusSent && q.Status != StatusExpired {
			return audit.Entry{}, wrongStatus(q.Status, "declined")
		}
		if _, err := tx.Exec(ctx, `UPDATE quotes SET status='declined', decided_at=now(), decision_note=$2 WHERE id=$1`, id, reason); err != nil {
			return audit.Entry{}, err
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: "declined", Summary: "Declined by the client", Reason: reason}, nil
	})
	if err != nil {
		return Quote{}, err
	}
	return s.Get(ctx, id)
}

func (s *Service) Cancel(ctx context.Context, id uuid.UUID, version int, reason string) (Quote, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Quote{}, httpx.FieldErrors{"reason": "Say why it's being cancelled."}.Err()
	}
	err := s.mutate(ctx, id, version, func(tx pgx.Tx, _ auth.Principal, q *Quote) (audit.Entry, error) {
		if q.Status != StatusDraft && q.Status != StatusPendingApproval && q.Status != StatusReady {
			return audit.Entry{}, wrongStatus(q.Status, "cancelled — record the client's decision instead")
		}
		if _, err := tx.Exec(ctx, `UPDATE quotes SET status='cancelled', closed_reason=$2 WHERE id=$1`, id, reason); err != nil {
			return audit.Entry{}, err
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: "cancelled", Summary: "Cancelled", Reason: reason}, nil
	})
	if err != nil {
		return Quote{}, err
	}
	return s.Get(ctx, id)
}

// Revise starts a new revision as a draft copy. The quotation it replaces is
// marked superseded (unless the client already declined it or it expired).
func (s *Service) Revise(ctx context.Context, id uuid.UUID, version int) (Quote, error) {
	var newID uuid.UUID
	err := s.mutate(ctx, id, version, func(tx pgx.Tx, actor auth.Principal, q *Quote) (audit.Entry, error) {
		switch q.Status {
		case StatusReady, StatusSent, StatusDeclined, StatusExpired:
		default:
			return audit.Entry{}, wrongStatus(q.Status, "revised")
		}
		var latest int
		if err := tx.QueryRow(ctx, `SELECT max(revision) FROM quotes WHERE reference = $1`, q.Reference).Scan(&latest); err != nil {
			return audit.Entry{}, err
		}
		if latest != q.Revision {
			return audit.Entry{}, httpx.NewError(http.StatusConflict, "not_latest", "A newer revision of this quotation already exists.")
		}
		if _, err := s.clients.RequireActive(ctx, q.clientID); err != nil {
			return audit.Entry{}, err
		}
		owner := q.ownerID
		var ownerActive bool
		if err := tx.QueryRow(ctx, `SELECT active FROM users WHERE id=$1`, owner).Scan(&ownerActive); err != nil {
			return audit.Entry{}, err
		}
		if !ownerActive {
			owner = actor.UserID
		}
		valid := s.today().AddDate(0, 0, s.validDays)
		if err := tx.QueryRow(ctx, `
			INSERT INTO quotes (reference, revision, client_id, deal_id, contact_id, title, currency, quote_date, valid_until,
			                    notes, terms, delivery_terms, payment_terms, warranty_terms, internal_note, subtotal, discount_total, net_total, vat_total, total, discount_pct,
			                    max_line_discount, owner_id, created_by)
			SELECT reference, revision + 1, client_id, deal_id, contact_id, title, currency, $2, $3, notes, terms,
			       delivery_terms, payment_terms, warranty_terms, internal_note,
			       subtotal, discount_total, net_total, vat_total, total, discount_pct, max_line_discount, $4, $5
			FROM quotes WHERE id = $1 RETURNING id`, id, s.today(), valid, owner, actor.UserID).Scan(&newID); err != nil {
			return audit.Entry{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO quote_lines (quote_id, position, product_id, sku, description, quantity, unit, unit_price, discount_pct,
			                         vat_rate, line_net, line_vat, line_total)
			SELECT $2, position, product_id, sku, description, quantity, unit, unit_price, discount_pct, vat_rate, line_net, line_vat, line_total
			FROM quote_lines WHERE quote_id = $1`, id, newID); err != nil {
			return audit.Entry{}, err
		}
		next := number(q.Reference, q.Revision+1)
		if err := audit.Record(ctx, tx, audit.Entry{EntityType: entityType, EntityID: newID, Action: "created",
			Summary: "Revision " + strconv.Itoa(q.Revision+1) + " started from " + q.Number}); err != nil {
			return audit.Entry{}, err
		}
		if q.Status == StatusReady || q.Status == StatusSent {
			if _, err := tx.Exec(ctx, `UPDATE quotes SET status='superseded' WHERE id=$1`, id); err != nil {
				return audit.Entry{}, err
			}
			return audit.Entry{EntityType: entityType, EntityID: id, Action: "superseded", Summary: "Replaced by " + next}, nil
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: "revised", Summary: "Revised as " + next}, nil
	})
	if err != nil {
		return Quote{}, err
	}
	return s.Get(ctx, newID)
}

// PDF renders the quotation document.
func (s *Service) PDF(ctx context.Context, id uuid.UUID) ([]byte, string, error) {
	q, err := getQuote(ctx, s.pool, id, false)
	if db.IsNoRows(err) {
		return nil, "", httpx.ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	full, err := s.loadForDocument(ctx, s.pool, &q)
	if err != nil {
		return nil, "", err
	}
	b, err := s.renderPDF(ctx, full)
	return b, full.Number + ".pdf", err
}

func (s *Service) Activity(ctx context.Context, id uuid.UUID) ([]audit.Event, error) {
	q, err := getQuote(ctx, s.pool, id, false)
	if db.IsNoRows(err) {
		return nil, httpx.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// The history of every revision, so it reads as one story.
	ids := []uuid.UUID{}
	revs, err := revisions(ctx, s.pool, q.Reference)
	if err != nil {
		return nil, err
	}
	for _, r := range revs {
		ids = append(ids, r.ID)
	}
	var out []audit.Event
	for _, rid := range ids {
		evs, err := audit.Timeline(ctx, s.pool, entityType, rid, 100)
		if err != nil {
			return nil, err
		}
		out = append(out, evs...)
	}
	sortEvents(out)
	return out, nil
}

// ---- internals ----

// mutate locks the quotation, checks the version and that the person may
// change it, and runs fn and its audit entry in one transaction.
func (s *Service) mutate(ctx context.Context, id uuid.UUID, version int, fn func(tx pgx.Tx, actor auth.Principal, q *Quote) (audit.Entry, error)) error {
	return s.mutateAs(ctx, id, version, false, fn)
}

func (s *Service) mutateAs(ctx context.Context, id uuid.UUID, version int, approver bool,
	fn func(tx pgx.Tx, actor auth.Principal, q *Quote) (audit.Entry, error)) error {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return httpx.ErrUnauthorized
	}
	if version < 1 {
		return httpx.FieldErrors{"version": "Version is required."}.Err()
	}
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q, err := getQuote(ctx, tx, id, true)
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if !approver && q.ownerID != actor.UserID && !actor.Can(PermManage) {
			return errNotYours
		}
		if q.Version != version {
			return httpx.ErrStaleVersion.With(map[string]any{"current_version": q.Version})
		}
		q.NeedsApproval = s.needsApproval(&q)
		entry, err := fn(tx, actor, &q)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE quotes SET updated_at=now(), version=version+1 WHERE id=$1 AND version=$2`, id, version); err != nil {
			return err
		}
		return audit.Record(ctx, tx, entry)
	})
}

func (s *Service) checkSendable(ctx context.Context, tx pgx.Tx, q *Quote) error {
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM quote_lines WHERE quote_id=$1`, q.ID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errNoLines
	}
	if _, err := s.clients.RequireActive(ctx, q.clientID); err != nil {
		return err
	}
	return nil
}

// tell notifies the quotation's preparer about something someone else did.
func (s *Service) tell(ctx context.Context, q Quote, kind, title, body string) {
	actor, _ := auth.FromContext(ctx)
	if actor.UserID == q.Owner.ID {
		return
	}
	id := q.ID
	if _, err := notify.Create(ctx, s.pool, notify.Notification{UserID: q.Owner.ID, Kind: kind, Title: title, Body: body,
		Link: "/quotations/" + q.ID.String(), EntityType: entityType, EntityID: &id}); err != nil {
		s.log.Error("quotation notification failed", "quote", q.ID, "err", err)
	}
}

// askApprovers tells the sales managers (or the administrators, if there are
// none) that a quotation is waiting for them.
func (s *Service) askApprovers(ctx context.Context, q Quote) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM users WHERE active AND (role = 'sales_manager'
		      OR (role = 'admin' AND NOT EXISTS (SELECT 1 FROM users WHERE active AND role = 'sales_manager')))`)
	if err != nil {
		s.log.Error("approvers lookup failed", "err", err)
		return
	}
	ids, _ := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	id := q.ID
	for _, uid := range ids {
		if uid == q.Owner.ID {
			continue
		}
		_, err := notify.Create(ctx, s.pool, notify.Notification{UserID: uid, Kind: KindApprovalRequested,
			Title: "Approval needed: " + q.Number + " (" + maxPct(&q) + "% discount)",
			Body:  q.Owner.Name + " · " + q.Client.LegalName + " · " + q.Currency + " " + grouped(q.Total),
			Link:  "/quotations/" + q.ID.String(), EntityType: entityType, EntityID: &id,
			DedupeKey: fmt.Sprintf("%s:%s:%d:%s", KindApprovalRequested, q.ID, q.Version, uid)})
		if err != nil {
			s.log.Error("approval notification failed", "err", err)
		}
	}
}

// TermDefaults are the standard entries for the template's terms fields; staff can change them.
type TermDefaults struct {
	Delivery string `json:"delivery_terms"`
	Payment  string `json:"payment_terms"`
	Warranty string `json:"warranty_terms"`
}

// DefaultTerms are the standard terms for a new quotation for a client with these credit terms.
func (s *Service) DefaultTerms(creditDays int) TermDefaults {
	pay := "100% with order"
	if creditDays > 0 {
		pay = fmt.Sprintf("%d days from date of invoice", creditDays)
	}
	return TermDefaults{
		Delivery: "7–14 working days from receipt of order, subject to stock",
		Payment:  pay,
		Warranty: "As per manufacturer's warranty",
	}
}

func maxPct(q *Quote) string {
	d, _ := parseHundredths(q.DiscountPct)
	m, _ := parseHundredths(q.MaxLineDiscount)
	if m > d {
		return m.Trimmed()
	}
	return d.Trimmed()
}

func grouped(amount string) string {
	h, err := parseHundredths(amount)
	if err != nil {
		return amount
	}
	return h.Grouped()
}

func statusLabel(s Status) string {
	switch s {
	case StatusPendingApproval:
		return "waiting for approval"
	case StatusReady:
		return "ready to send"
	default:
		return string(s)
	}
}

func clean(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func currencyOK(c string) bool {
	if len(c) != 3 {
		return false
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}
