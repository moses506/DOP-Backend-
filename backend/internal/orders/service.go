package orders

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/notify"
	"dos-bpas/internal/platform/numbering"
	"dos-bpas/internal/quotes"
)

const entityType = "customer_order"

const (
	KindNewOrder   = "order_new"       // operations: something to source and deliver
	KindDelivered  = "order_delivered" // finance: ready to invoice
	KindChanged    = "order_changed"   // the owner: someone else changed their order
	KindDueSoon    = "order_due_soon"
	KindOverdue    = "order_overdue"
	KindEscalated  = "order_overdue_escalated"
	KindCancelled  = "order_cancelled"
	maxTextLen     = 2000
	maxShortTxtLen = 120
)

var (
	errNotYours = httpx.NewError(http.StatusForbidden, "not_your_order",
		"Only the order's owner or a sales manager can change this.")
	errCannotFulfil = httpx.NewError(http.StatusForbidden, "not_your_order",
		"Only the order's owner, operations or a sales manager can update progress and deliveries.")
	errClosed = httpx.NewError(http.StatusConflict, "order_closed",
		"This order is closed and can't be changed.")
	errOnHold = httpx.NewError(http.StatusConflict, "order_on_hold",
		"This order is on hold. Resume it first.")
	errNoChange = httpx.NewError(http.StatusUnprocessableEntity, "no_change", "Nothing was changed.")
)

func orderExists(id uuid.UUID, ref string) error {
	return httpx.NewError(http.StatusConflict, "order_exists",
		"This quotation already has an order: "+ref+".").With(map[string]any{"order_id": id, "reference": ref})
}

type Service struct {
	pool    *pgxpool.Pool
	numbers *numbering.Service
	clients clients.API
	quotes  quotes.API
	deals   deals.API
	log     *slog.Logger
	now     func() time.Time
	loc     *time.Location
	appURL  string
}

type Options struct {
	AppURL   string
	Location *time.Location
	Log      *slog.Logger
}

func NewService(pool *pgxpool.Pool, numbers *numbering.Service, cl clients.API, qt quotes.API, dl deals.API, now func() time.Time, o Options) *Service {
	if o.Location == nil {
		o.Location, _ = time.LoadLocation("Africa/Lusaka")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Service{pool: pool, numbers: numbers, clients: cl, quotes: qt, deals: dl, log: o.Log, now: now, loc: o.Location,
		appURL: strings.TrimRight(o.AppURL, "/")}
}

// today is the business date as YYYY-MM-DD.
func (s *Service) today() string { return s.now().In(s.loc).Format("2006-01-02") }

// ---- queries ----

func (s *Service) Get(ctx context.Context, id uuid.UUID) (Order, error) {
	o, err := getOrder(ctx, s.pool, id, s.today(), false)
	if db.IsNoRows(err) {
		return Order{}, httpx.ErrNotFound
	}
	if err != nil {
		return Order{}, err
	}
	if o.Lines, err = listLines(ctx, s.pool, id); err != nil {
		return Order{}, err
	}
	if o.Deliveries, err = listDeliveries(ctx, s.pool, id); err != nil {
		return Order{}, err
	}
	s.attach(ctx, &o)
	return o, nil
}

// attach fills in the contact, delivery site and deal from their modules.
func (s *Service) attach(ctx context.Context, o *Order) {
	if o.contactID != nil || o.siteID != nil {
		if c, err := s.clients.Get(ctx, o.clientID); err == nil {
			for _, k := range c.Contacts {
				if o.contactID != nil && k.ID == *o.contactID {
					o.Contact = &ContactRef{ID: k.ID, Name: k.Name, Phone: k.Phone, Email: k.Email}
				}
			}
			for _, st := range c.Sites {
				if o.siteID != nil && st.ID == *o.siteID {
					o.Site = &SiteRef{ID: st.ID, Name: st.Name, Address: st.Address, ContactName: st.ContactName, ContactPhone: st.ContactPhone}
				}
			}
		}
	}
	if o.dealID != nil {
		if d, err := s.deals.Get(ctx, *o.dealID); err == nil {
			o.Deal = &DealRef{ID: d.ID, Reference: d.Reference, Title: d.Title}
		}
	}
}

func (s *Service) List(ctx context.Context, f ListFilter) (httpx.List[Order], error) {
	var matches []uuid.UUID
	if strings.TrimSpace(f.Query) != "" {
		var err error
		if matches, err = s.clients.MatchIDs(ctx, f.Query, 500); err != nil {
			return httpx.List[Order]{}, err
		}
	}
	items, total, err := listOrders(ctx, s.pool, f, s.today(), matches)
	if err != nil {
		return httpx.List[Order]{}, err
	}
	return httpx.List[Order]{Items: items, Total: total, Limit: f.Limit, Offset: f.Offset}, nil
}

func (s *Service) Summary(ctx context.Context) (Summary, error) {
	var out Summary
	today := s.today()
	week := s.now().In(s.loc).AddDate(0, 0, 7).Format("2006-01-02")
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status IN ('open','in_progress')),
		       count(*) FILTER (WHERE status = 'ready'),
		       count(*) FILTER (WHERE status = 'part_delivered'),
		       count(*) FILTER (WHERE status IN ('open','in_progress','ready','part_delivered','on_hold') AND delivery_due < $1::date),
		       count(*) FILTER (WHERE status IN ('open','in_progress','ready','part_delivered') AND delivery_due BETWEEN $1::date AND $2::date),
		       count(*) FILTER (WHERE status = 'delivered'),
		       count(*) FILTER (WHERE status = 'on_hold'),
		       coalesce(sum(total) FILTER (WHERE status IN ('open','in_progress','ready','part_delivered','on_hold') AND currency = 'ZMW'), 0)::text,
		       coalesce(sum(total) FILTER (WHERE status IN ('open','in_progress','ready','part_delivered','on_hold') AND currency = 'USD'), 0)::text,
		       coalesce(sum(total) FILTER (WHERE status = 'delivered' AND currency = 'ZMW'), 0)::text
		FROM customer_orders`, today, week).Scan(&out.Open, &out.Ready, &out.PartDelivered, &out.Late, &out.DueThisWeek,
		&out.ToInvoice, &out.OnHold, &out.LiveValue, &out.LiveValueUSD, &out.ToInvoiceValue)
	return out, err
}

func (s *Service) Activity(ctx context.Context, id uuid.UUID) ([]audit.Event, error) {
	return audit.Timeline(ctx, s.pool, entityType, id, 200)
}

// ---- create ----

// Create turns an accepted quotation into an order. The lines, prices and
// totals are copied exactly; the quotation stays as it was.
func (s *Service) Create(ctx context.Context, in CreateInput) (Order, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return Order{}, httpx.ErrUnauthorized
	}
	fe := httpx.FieldErrors{}
	today := s.today()
	po := cleanShort(in.ClientPO)
	poDate := s.checkDate(fe, "client_po_date", in.ClientPODate, "", today)
	due := s.checkDate(fe, "delivery_due", in.DeliveryDue, today, "")
	instr, note := clean(in.DeliveryInstructions), clean(in.InternalNote)
	checkLen(fe, "delivery_instructions", instr)
	checkLen(fe, "internal_note", note)
	if in.QuoteID == uuid.Nil {
		fe.Add("quote_id", "Choose the accepted quotation this order is for.")
	}
	if err := fe.Err(); err != nil {
		return Order{}, err
	}

	var id uuid.UUID
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q, err := s.quotes.ForOrder(ctx, tx, in.QuoteID)
		if err != nil {
			return err
		}
		if q.OwnerID != actor.UserID && !actor.Can(PermManage) {
			return httpx.NewError(http.StatusForbidden, "not_your_quote",
				"Only the person who prepared the quotation or a sales manager can turn it into an order.")
		}
		cl, err := s.clients.RequireActive(ctx, q.ClientID)
		if err != nil {
			return err
		}
		var existing uuid.UUID
		var existingRef string
		err = tx.QueryRow(ctx, `SELECT id, reference FROM customer_orders WHERE quote_id = $1 AND status <> 'cancelled' FOR UPDATE`, q.ID).
			Scan(&existing, &existingRef)
		if err == nil {
			return orderExists(existing, existingRef)
		}
		if !db.IsNoRows(err) {
			return err
		}
		if in.SiteID != nil && !siteOK(cl, *in.SiteID) {
			fe.Add("site_id", "Choose one of this client's delivery sites.")
		}
		sources := map[int]LineSource{}
		for _, ls := range in.Sources {
			if !ls.Source.Valid() {
				fe.Add("lines", "Each line comes from stock, a supplier, or is a service.")
				break
			}
			sources[ls.Position] = ls
		}
		if err := fe.Err(); err != nil {
			return err
		}
		if po == nil {
			po = q.ClientPO
		}

		ref, err := s.numbers.Next(ctx, tx, numbering.CustomerOrder)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO customer_orders (reference, client_id, quote_id, quote_number, deal_id, contact_id, title, currency,
			    client_po, client_po_date, order_date, delivery_due, site_id, delivery_instructions, payment_terms, internal_note,
			    subtotal, discount_total, net_total, vat_total, total, owner_id, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
			RETURNING id`,
			ref, q.ClientID, q.ID, q.Number, q.DealID, q.ContactID, q.Title, q.Currency,
			po, poDate, today, due, in.SiteID, instr, q.PaymentTerms, note,
			q.Subtotal, q.DiscountTotal, q.NetTotal, q.VATTotal, q.Total, q.OwnerID, actor.UserID).Scan(&id)
		if db.IsUniqueViolation(err) {
			return httpx.NewError(http.StatusConflict, "order_exists", "This quotation already has an order.")
		}
		if err != nil {
			return err
		}
		for _, l := range q.Lines {
			src := guessSource(l)
			var sNote *string
			if ls, ok := sources[l.Position]; ok {
				src, sNote = ls.Source, cleanShort(ls.SupplierNote)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO customer_order_lines (order_id, position, product_id, sku, description, quantity, unit, unit_price,
				    discount_pct, vat_rate, line_net, line_vat, line_total, source, supplier_note)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
				id, l.Position, l.ProductID, l.SKU, l.Description, l.Quantity, l.Unit, l.UnitPrice,
				l.DiscountPct, l.VATRate, l.LineNet, l.LineVAT, l.LineTotal, src, sNote); err != nil {
				return err
			}
		}
		summary := "Order created from quotation " + q.Number
		if po != nil {
			summary += " — client's order " + *po
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: entityType, EntityID: id, Action: "created", Summary: summary,
			Changes: map[string]any{"quote": q.Number, "total": q.Currency + " " + q.Total, "delivery_due": due}})
	})
	if err != nil {
		return Order{}, err
	}
	o, err := s.Get(ctx, id)
	if err == nil {
		body := o.Client.LegalName + " · " + plural(len(o.Lines), "line")
		if o.DeliveryDue != nil {
			body += " · promised " + dayLabel(*o.DeliveryDue)
		}
		s.notifyRole(ctx, "operations", o, KindNewOrder, "New order to fulfil: "+o.Reference, body, "")
	}
	return o, err
}

// guessSource suggests where a line comes from: services are work DOS does
// itself; everything else is assumed to be bought in until someone says
// it's in stock.
func guessSource(l quotes.Line) Source {
	u := strings.ToLower(l.Unit)
	d := strings.ToLower(l.Description)
	if u == "hour" || u == "hours" || u == "month" || u == "months" || u == "visit" || u == "day" || u == "days" ||
		strings.Contains(d, "installation") || strings.Contains(d, "training") || strings.Contains(d, "service") ||
		strings.Contains(d, "support") || strings.Contains(d, "labour") || strings.Contains(d, "configuration") {
		return SourceService
	}
	return SourceSupplier
}

func siteOK(c clients.Client, id uuid.UUID) bool {
	for _, st := range c.Sites {
		if st.ID == id && st.Active {
			return true
		}
	}
	return false
}

// ---- changes ----

// Update changes the order's details. Moving the promised delivery date needs
// a reason, because the client was told the first one.
func (s *Service) Update(ctx context.Context, id uuid.UUID, in UpdateInput) (Order, error) {
	reason := strings.TrimSpace(in.Reason)
	err := s.mutate(ctx, id, in.Version, ownerOnly, func(tx pgx.Tx, actor auth.Principal, o *Order) (audit.Entry, error) {
		if o.Status.Closed() {
			return audit.Entry{}, errClosed
		}
		fe := httpx.FieldErrors{}
		changes := map[string]any{}
		set := []string{}
		args := []any{id}
		add := func(col string, v any) {
			args = append(args, v)
			set = append(set, fmt.Sprintf("%s = $%d", col, len(args)))
		}
		if in.ClientPO != nil {
			v := cleanShort(in.ClientPO)
			if deref(v) != deref(o.ClientPO) {
				changes["client_po"] = map[string]any{"from": o.ClientPO, "to": v}
				add("client_po", v)
			}
		}
		if in.ClientPODate != nil {
			v := s.checkDate(fe, "client_po_date", in.ClientPODate, "", s.today())
			if deref(v) != deref(o.ClientPODate) {
				changes["client_po_date"] = map[string]any{"from": o.ClientPODate, "to": v}
				add("client_po_date", v)
			}
		}
		dueMoved := false
		if in.DeliveryDue != nil {
			v := s.checkDate(fe, "delivery_due", in.DeliveryDue, o.OrderDate, "")
			if deref(v) != deref(o.DeliveryDue) {
				changes["delivery_due"] = map[string]any{"from": o.DeliveryDue, "to": v}
				add("delivery_due", v)
				if o.DeliveryDue != nil {
					dueMoved = true
					if reason == "" {
						fe.Add("reason", "Say why the delivery date is moving — the client was promised "+dayLabel(*o.DeliveryDue)+".")
					}
				}
			}
		}
		if in.SiteID != nil {
			var v *uuid.UUID
			if *in.SiteID != "" {
				sid, err := uuid.Parse(*in.SiteID)
				if err != nil {
					fe.Add("site_id", "Choose one of this client's delivery sites.")
				} else {
					v = &sid
					c, err := s.clients.Get(ctx, o.clientID)
					if err != nil {
						return audit.Entry{}, err
					}
					if !siteOK(c, sid) {
						fe.Add("site_id", "Choose one of this client's delivery sites.")
					}
				}
			}
			if idStr(v) != idStr(o.siteID) {
				changes["site"] = map[string]any{"from": idStr(o.siteID), "to": idStr(v)}
				add("site_id", v)
			}
		}
		if in.DeliveryInstructions != nil {
			v := clean(in.DeliveryInstructions)
			checkLen(fe, "delivery_instructions", v)
			if deref(v) != deref(o.DeliveryInstructions) {
				changes["delivery_instructions"] = map[string]any{"from": o.DeliveryInstructions, "to": v}
				add("delivery_instructions", v)
			}
		}
		if in.InternalNote != nil {
			v := clean(in.InternalNote)
			checkLen(fe, "internal_note", v)
			if deref(v) != deref(o.InternalNote) {
				changes["internal_note"] = "changed"
				add("internal_note", v)
			}
		}
		if err := fe.Err(); err != nil {
			return audit.Entry{}, err
		}
		if len(set) == 0 {
			return audit.Entry{}, errNoChange
		}
		if _, err := tx.Exec(ctx, `UPDATE customer_orders SET `+strings.Join(set, ", ")+` WHERE id = $1`, args...); err != nil {
			return audit.Entry{}, err
		}
		summary := "Details changed"
		action := "updated"
		if dueMoved {
			action = "delivery_date_moved"
			summary = "Delivery date moved from " + dayLabel(*o.DeliveryDue) + " to " + dayLabel(*in.DeliveryDue)
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: action, Summary: summary, Changes: changes, Reason: reason}, nil
	})
	if err != nil {
		return Order{}, err
	}
	return s.after(ctx, id, "Order details changed")
}

// Progress records where each line has got to: where it comes from, notes on
// the supplier order, and how much is ready to deliver.
func (s *Service) Progress(ctx context.Context, id uuid.UUID, in ProgressInput) (Order, error) {
	err := s.mutate(ctx, id, in.Version, fulfiller, func(tx pgx.Tx, actor auth.Principal, o *Order) (audit.Entry, error) {
		if o.Status.Closed() {
			return audit.Entry{}, errClosed
		}
		lines, err := listLines(ctx, tx, id)
		if err != nil {
			return audit.Entry{}, err
		}
		byID := map[uuid.UUID]*Line{}
		for i := range lines {
			byID[lines[i].ID] = &lines[i]
		}
		fe := httpx.FieldErrors{}
		var said []string
		for i, p := range in.Lines {
			l, ok := byID[p.ID]
			if !ok {
				fe.Add(fmt.Sprintf("lines.%d", i), "That line isn't on this order.")
				continue
			}
			var parts []string
			if p.Source != nil && *p.Source != l.Source {
				if !p.Source.Valid() {
					fe.Add(fmt.Sprintf("lines.%d.source", i), "Choose stock, supplier or service.")
					continue
				}
				parts = append(parts, "from "+sourceLabel(*p.Source))
				l.Source = *p.Source
			}
			if p.SupplierNote != nil {
				v := cleanShort(p.SupplierNote)
				if deref(v) != deref(l.SupplierNote) {
					if v != nil {
						parts = append(parts, "“"+*v+"”")
					}
					l.SupplierNote = v
				}
			}
			if p.QtyReady != nil {
				r, err := parseQty(*p.QtyReady)
				switch {
				case err != nil:
					fe.Add(fmt.Sprintf("lines.%d.qty_ready", i), "Enter a number, e.g. 2 or 2.5.")
				case r > mustQty(l.Quantity):
					fe.Add(fmt.Sprintf("lines.%d.qty_ready", i), "Can't be more than the "+l.Quantity+" ordered.")
				case r < mustQty(l.QtyDelivered):
					fe.Add(fmt.Sprintf("lines.%d.qty_ready", i), l.QtyDelivered+" already delivered, so at least that many were ready.")
				case r.String() != l.QtyReady:
					parts = append(parts, r.String()+" of "+l.Quantity+" ready")
					l.QtyReady = r.String()
				}
			}
			if len(parts) > 0 {
				said = append(said, firstLine(l.Description, 60)+": "+strings.Join(parts, ", "))
				if _, err := tx.Exec(ctx, `UPDATE customer_order_lines SET source = $2, supplier_note = $3, qty_ready = $4 WHERE id = $1`,
					l.ID, l.Source, l.SupplierNote, l.QtyReady); err != nil {
					return audit.Entry{}, err
				}
			}
		}
		if err := fe.Err(); err != nil {
			return audit.Entry{}, err
		}
		if len(said) == 0 {
			return audit.Entry{}, errNoChange
		}
		from := o.Status
		st, err := setStatusFromLines(ctx, tx, o, lines, s.now())
		if err != nil {
			return audit.Entry{}, err
		}
		summary := "Progress updated"
		if st != from {
			summary += " — now " + statusLabel(st)
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: "progress", Summary: summary,
			Changes: map[string]any{"lines": said, "status": map[string]any{"from": from, "to": st}}}, nil
	})
	if err != nil {
		return Order{}, err
	}
	return s.after(ctx, id, "Progress updated")
}

// RecordDelivery records goods handed to the client. Until delivery notes are
// built, staff record each signed delivery by hand with its paper number.
func (s *Service) RecordDelivery(ctx context.Context, id uuid.UUID, in DeliveryInput) (Order, error) {
	var became Status
	err := s.mutate(ctx, id, in.Version, fulfiller, func(tx pgx.Tx, actor auth.Principal, o *Order) (audit.Entry, error) {
		if o.Status.Closed() {
			return audit.Entry{}, errClosed
		}
		if o.Status == StatusOnHold {
			return audit.Entry{}, errOnHold
		}
		fe := httpx.FieldErrors{}
		on := s.checkDate(fe, "delivered_on", &in.DeliveredOn, o.OrderDate, s.today())
		if on == nil && fe["delivered_on"] == "" {
			fe.Add("delivered_on", "When was it delivered?")
		}
		ref, recv, note := cleanShort(&in.Reference), cleanShort(&in.ReceivedBy), clean(&in.Note)
		checkLen(fe, "note", note)
		lines, err := listLines(ctx, tx, id)
		if err != nil {
			return audit.Entry{}, err
		}
		byID := map[uuid.UUID]*Line{}
		for i := range lines {
			byID[lines[i].ID] = &lines[i]
		}
		type part struct {
			l *Line
			q qty
		}
		var parts []part
		seen := map[uuid.UUID]bool{}
		for i, x := range in.Lines {
			l, ok := byID[x.ID]
			if !ok || seen[x.ID] {
				fe.Add(fmt.Sprintf("lines.%d", i), "That line isn't on this order.")
				continue
			}
			seen[x.ID] = true
			if strings.TrimSpace(x.Quantity) == "" || strings.TrimSpace(x.Quantity) == "0" {
				continue
			}
			q, err := parseQty(x.Quantity)
			left := mustQty(l.Quantity) - mustQty(l.QtyDelivered)
			switch {
			case err != nil:
				fe.Add(fmt.Sprintf("lines.%d.quantity", i), "Enter a number, e.g. 2 or 2.5.")
			case q > left:
				fe.Add(fmt.Sprintf("lines.%d.quantity", i), "Only "+left.String()+" left to deliver.")
			case q > 0:
				parts = append(parts, part{l, q})
			}
		}
		if len(parts) == 0 && len(fe) == 0 {
			fe.Add("lines", "Enter how many of at least one item were delivered.")
		}
		if err := fe.Err(); err != nil {
			return audit.Entry{}, err
		}
		var did uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO customer_order_deliveries (order_id, delivered_on, reference, received_by, note, created_by)
			VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, id, on, ref, recv, note, actor.UserID).Scan(&did); err != nil {
			return audit.Entry{}, err
		}
		var said []string
		for _, p := range parts {
			delivered := mustQty(p.l.QtyDelivered) + p.q
			ready := mustQty(p.l.QtyReady)
			if ready < delivered {
				ready = delivered
			}
			if _, err := tx.Exec(ctx, `INSERT INTO customer_order_delivery_lines (delivery_id, line_id, quantity) VALUES ($1,$2,$3)`,
				did, p.l.ID, p.q.String()); err != nil {
				return audit.Entry{}, err
			}
			if _, err := tx.Exec(ctx, `UPDATE customer_order_lines SET qty_delivered = $2, qty_ready = $3 WHERE id = $1`,
				p.l.ID, delivered.String(), ready.String()); err != nil {
				return audit.Entry{}, err
			}
			p.l.QtyDelivered, p.l.QtyReady = delivered.String(), ready.String()
			said = append(said, p.q.String()+" × "+firstLine(p.l.Description, 60))
		}
		became, err = setStatusFromLines(ctx, tx, o, lines, s.now())
		if err != nil {
			return audit.Entry{}, err
		}
		summary := "Delivered " + strings.Join(said, ", ")
		if ref != nil {
			summary += " (" + *ref + ")"
		}
		if became == StatusDelivered {
			summary += " — everything is now delivered"
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: "delivered", Summary: summary,
			Changes: map[string]any{"delivered_on": on, "received_by": recv, "lines": said}}, nil
	})
	if err != nil {
		return Order{}, err
	}
	o, err := s.after(ctx, id, "Delivery recorded")
	if err == nil && became == StatusDelivered {
		s.notifyRole(ctx, "finance", o, KindDelivered, "Ready to invoice: "+o.Reference,
			o.Client.LegalName+" · "+o.Currency+" "+grouped(o.Total)+" · fully delivered", "")
	}
	return o, err
}

// Hold pauses an order, e.g. while the client sorts out payment or a site.
func (s *Service) Hold(ctx context.Context, id uuid.UUID, version int, reason string) (Order, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Order{}, httpx.FieldErrors{"reason": "Say why, e.g. waiting for the client's deposit."}.Err()
	}
	err := s.mutate(ctx, id, version, ownerOnly, func(tx pgx.Tx, _ auth.Principal, o *Order) (audit.Entry, error) {
		if o.Status.Closed() {
			return audit.Entry{}, errClosed
		}
		if o.Status == StatusOnHold {
			return audit.Entry{}, httpx.NewError(http.StatusConflict, "wrong_status", "This order is already on hold.")
		}
		if o.Status == StatusDelivered {
			return audit.Entry{}, httpx.NewError(http.StatusConflict, "wrong_status", "Everything is delivered — there's nothing left to hold.")
		}
		if _, err := tx.Exec(ctx, `UPDATE customer_orders SET status='on_hold', hold_reason=$2, held_at=now() WHERE id=$1`, id, reason); err != nil {
			return audit.Entry{}, err
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: "held", Summary: "Put on hold", Reason: reason}, nil
	})
	if err != nil {
		return Order{}, err
	}
	return s.after(ctx, id, "Order put on hold")
}

func (s *Service) Resume(ctx context.Context, id uuid.UUID, version int) (Order, error) {
	err := s.mutate(ctx, id, version, ownerOnly, func(tx pgx.Tx, _ auth.Principal, o *Order) (audit.Entry, error) {
		if o.Status != StatusOnHold {
			return audit.Entry{}, httpx.NewError(http.StatusConflict, "wrong_status", "This order isn't on hold.")
		}
		lines, err := listLines(ctx, tx, id)
		if err != nil {
			return audit.Entry{}, err
		}
		_, _, st := progress(lines)
		if _, err := tx.Exec(ctx, `UPDATE customer_orders SET status=$2, hold_reason=NULL, held_at=NULL WHERE id=$1`, id, st); err != nil {
			return audit.Entry{}, err
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: "resumed", Summary: "Taken off hold — " + statusLabel(st)}, nil
	})
	if err != nil {
		return Order{}, err
	}
	return s.after(ctx, id, "Order resumed")
}

// Cancel closes an order nothing has been delivered on. Once goods have gone
// out, the order has to be completed and the rest dealt with on the invoice.
func (s *Service) Cancel(ctx context.Context, id uuid.UUID, version int, reason string) (Order, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Order{}, httpx.FieldErrors{"reason": "Say why the order is cancelled."}.Err()
	}
	err := s.mutate(ctx, id, version, ownerOnly, func(tx pgx.Tx, _ auth.Principal, o *Order) (audit.Entry, error) {
		if o.Status.Closed() {
			return audit.Entry{}, errClosed
		}
		var delivered bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM customer_order_lines WHERE order_id=$1 AND qty_delivered > 0)`, id).Scan(&delivered); err != nil {
			return audit.Entry{}, err
		}
		if delivered {
			return audit.Entry{}, httpx.NewError(http.StatusConflict, "already_delivered",
				"Goods have already been delivered on this order, so it can't be cancelled.")
		}
		if _, err := tx.Exec(ctx, `UPDATE customer_orders SET status='cancelled', cancelled_at=now(), closed_reason=$2,
			hold_reason=NULL, held_at=NULL WHERE id=$1`, id, reason); err != nil {
			return audit.Entry{}, err
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: "cancelled", Summary: "Order cancelled", Reason: reason}, nil
	})
	if err != nil {
		return Order{}, err
	}
	o, err := s.after(ctx, id, "Order cancelled")
	if err == nil {
		s.notifyRole(ctx, "operations", o, KindCancelled, "Order cancelled: "+o.Reference,
			o.Client.LegalName+" — "+shortText(reason, 120)+". Stop any supplier orders for it.", "")
	}
	return o, err
}

// MarkInvoiced closes a delivered order with the invoice number from the
// accounts system. The invoicing module (phase 4) will do this itself.
func (s *Service) MarkInvoiced(ctx context.Context, id uuid.UUID, version int, invoiceRef, invoicedOn string) (Order, error) {
	err := s.mutate(ctx, id, version, invoicer, func(tx pgx.Tx, _ auth.Principal, o *Order) (audit.Entry, error) {
		if o.Status != StatusDelivered {
			return audit.Entry{}, httpx.NewError(http.StatusConflict, "not_delivered",
				"Only a fully delivered order can be marked as invoiced.").With(map[string]any{"status": o.Status})
		}
		fe := httpx.FieldErrors{}
		ref := cleanShort(&invoiceRef)
		if ref == nil {
			fe.Add("invoice_ref", "Enter the invoice number.")
		}
		on := s.checkDate(fe, "invoiced_on", &invoicedOn, o.OrderDate, s.today())
		if on == nil && fe["invoiced_on"] == "" {
			t := s.today()
			on = &t
		}
		if err := fe.Err(); err != nil {
			return audit.Entry{}, err
		}
		actor, _ := auth.FromContext(ctx)
		if _, err := tx.Exec(ctx, `UPDATE customer_orders SET status='invoiced', invoice_ref=$2, invoiced_on=$3, invoiced_by=$4 WHERE id=$1`,
			id, ref, on, actor.UserID); err != nil {
			return audit.Entry{}, err
		}
		return audit.Entry{EntityType: entityType, EntityID: id, Action: "invoiced", Summary: "Invoiced — " + *ref,
			Changes: map[string]any{"invoice_ref": ref, "invoiced_on": on}}, nil
	})
	if err != nil {
		return Order{}, err
	}
	return s.after(ctx, id, "Order invoiced")
}

// ---- internals ----

type who int

const (
	ownerOnly who = iota // the owner or a manager
	fulfiller            // also operations
	invoicer             // finance (checked by the route) or a manager
)

func (s *Service) mutate(ctx context.Context, id uuid.UUID, version int, w who,
	fn func(tx pgx.Tx, actor auth.Principal, o *Order) (audit.Entry, error)) error {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return httpx.ErrUnauthorized
	}
	if version < 1 {
		return httpx.FieldErrors{"version": "Version is required."}.Err()
	}
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		o, err := getOrder(ctx, tx, id, s.today(), true)
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		mine := o.ownerID == actor.UserID || actor.Can(PermManage)
		switch w {
		case ownerOnly:
			if !mine {
				return errNotYours
			}
		case fulfiller:
			if !mine && !actor.Can(PermFulfil) {
				return errCannotFulfil
			}
		case invoicer:
			if !actor.Can(PermInvoice) && !actor.Can(PermManage) {
				return httpx.ErrForbidden
			}
		}
		if o.Version != version {
			return httpx.ErrStaleVersion.With(map[string]any{"current_version": o.Version})
		}
		entry, err := fn(tx, actor, &o)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE customer_orders SET updated_at=now(), version=version+1 WHERE id=$1`, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, entry)
	})
}

// after reloads the order and tells its owner when someone else changed it.
func (s *Service) after(ctx context.Context, id uuid.UUID, what string) (Order, error) {
	o, err := s.Get(ctx, id)
	if err != nil {
		return o, err
	}
	actor, _ := auth.FromContext(ctx)
	if actor.UserID != o.Owner.ID {
		oid := o.ID
		if _, err := notify.Create(ctx, s.pool, notify.Notification{UserID: o.Owner.ID, Kind: KindChanged,
			Title: what + ": " + o.Reference, Body: actor.Name + " · " + o.Client.LegalName + " · now " + statusLabel(o.Status),
			Link: "/orders/" + o.ID.String(), EntityType: entityType, EntityID: &oid}); err != nil {
			s.log.Error("order notification failed", "order", o.ID, "err", err)
		}
	}
	return o, nil
}

// notifyRole tells every active person with the role (but not whoever did it).
func (s *Service) notifyRole(ctx context.Context, role string, o Order, kind, title, body, dedupe string) {
	actor, _ := auth.FromContext(ctx)
	rows, err := s.pool.Query(ctx, `SELECT id FROM users WHERE active AND role = $1`, role)
	if err != nil {
		s.log.Error("order notification lookup failed", "err", err)
		return
	}
	ids, _ := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	oid := o.ID
	for _, uid := range ids {
		if uid == actor.UserID {
			continue
		}
		key := ""
		if dedupe != "" {
			key = dedupe + ":" + uid.String()
		}
		if _, err := notify.Create(ctx, s.pool, notify.Notification{UserID: uid, Kind: kind, Title: title, Body: body,
			Link: "/orders/" + o.ID.String(), EntityType: entityType, EntityID: &oid, DedupeKey: key}); err != nil {
			s.log.Error("order notification failed", "err", err)
		}
	}
}

// checkDate reads an optional YYYY-MM-DD date, within [min, max] when given.
func (s *Service) checkDate(fe httpx.FieldErrors, field string, v *string, min, max string) *string {
	if v == nil || strings.TrimSpace(*v) == "" {
		return nil
	}
	d := strings.TrimSpace(*v)
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		fe.Add(field, "Use a date like 2026-10-31.")
		return nil
	}
	d = t.Format("2006-01-02")
	if min != "" && d < min {
		fe.Add(field, "Can't be before "+dayLabel(min)+".")
		return nil
	}
	if max != "" && d > max {
		fe.Add(field, "Can't be in the future.")
		return nil
	}
	return &d
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

func cleanShort(s *string) *string {
	t := clean(s)
	if t != nil && utf8.RuneCountInString(*t) > maxShortTxtLen {
		r := []rune(*t)
		v := string(r[:maxShortTxtLen])
		return &v
	}
	return t
}

func checkLen(fe httpx.FieldErrors, field string, v *string) {
	if v != nil && utf8.RuneCountInString(*v) > maxTextLen {
		fe.Add(field, fmt.Sprintf("Keep it under %d characters.", maxTextLen))
	}
}

func shortText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// firstLine is the first line of a description (product names often run on
// with the spec), shortened.
func firstLine(s string, n int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return shortText(line, n)
}

func idStr(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}

func dayLabel(d string) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	return t.Format("Mon 2 Jan 2006")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// grouped renders "125000.50" as "125,000.50".
func grouped(amount string) string {
	whole, frac, _ := strings.Cut(amount, ".")
	neg := strings.HasPrefix(whole, "-")
	whole = strings.TrimPrefix(whole, "-")
	var b []string
	for len(whole) > 3 {
		b = append([]string{whole[len(whole)-3:]}, b...)
		whole = whole[:len(whole)-3]
	}
	b = append([]string{whole}, b...)
	out := strings.Join(b, ",")
	if neg {
		out = "-" + out
	}
	if frac != "" {
		out += "." + frac
	}
	return out
}

func statusLabel(s Status) string {
	return map[Status]string{
		StatusOpen: "new", StatusInProgress: "in progress", StatusReady: "ready to deliver",
		StatusPartDelivered: "part delivered", StatusDelivered: "delivered", StatusInvoiced: "invoiced",
		StatusOnHold: "on hold", StatusCancelled: "cancelled",
	}[s]
}

func sourceLabel(s Source) string {
	return map[Source]string{SourceStock: "stock", SourceSupplier: "a supplier", SourceService: "our own service"}[s]
}
