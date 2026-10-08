package sourcing

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dos-bpas/internal/platform/audit"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/httpx"
	mailer "dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/notify"
	"dos-bpas/internal/platform/numbering"
)

// Price requests (RFQs).
//
// Someone lists what's needed, picks supplier accounts to ask, and the system
// emails each one (or records that they were asked by phone or WhatsApp).
// Replies come back to that person's own inbox; they type each supplier's
// prices in, and the request compares them in kwacha and records which
// supplier was chosen for each item.

const maxItems = 100

var (
	errNotYours = httpx.NewError(http.StatusForbidden, "not_your_request",
		"Only the person who made this request or a manager can change it.")
	errCancelled = httpx.NewError(http.StatusConflict, "request_cancelled", "This request is cancelled.")
	errNoItems   = httpx.NewError(http.StatusUnprocessableEntity, "no_items", "Add at least one item before asking suppliers.")
)

const KindNoReply = "rfq_no_reply"

// ---- queries ----

const requestSelect = `
	SELECT r.id, r.reference, r.title, to_char(r.needed_by, 'YYYY-MM-DD'), r.client_id, r.deal_id, r.notes, r.internal_note,
	       r.status, o.id, o.full_name, o.position, cb.id, cb.full_name, cb.position,
	       r.created_at, r.updated_at, r.sent_at, r.decided_at, r.cancelled_at, r.closed_reason, r.version,
	       (SELECT count(*) FROM price_request_items i WHERE i.request_id = r.id),
	       (SELECT count(*) FROM price_request_invites v WHERE v.request_id = r.id),
	       (SELECT count(*) FROM price_request_invites v WHERE v.request_id = r.id AND v.status <> 'waiting'),
	       (SELECT count(*) FROM price_request_items i WHERE i.request_id = r.id AND i.chosen_invite_id IS NOT NULL)
	FROM price_requests r
	JOIN users o ON o.id = r.owner_id
	JOIN users cb ON cb.id = r.created_by`

func scanRequest(row pgx.Row, extra ...any) (Request, error) {
	var r Request
	dest := []any{&r.ID, &r.Reference, &r.Title, &r.NeededBy, &r.clientID, &r.dealID, &r.Notes, &r.InternalNote,
		&r.Status, &r.Owner.ID, &r.Owner.Name, &r.Owner.Position, &r.CreatedBy.ID, &r.CreatedBy.Name, &r.CreatedBy.Position,
		&r.CreatedAt, &r.UpdatedAt, &r.SentAt, &r.DecidedAt, &r.CancelledAt, &r.ClosedReason, &r.Version,
		&r.ItemCount, &r.Invited, &r.Replied, &r.Chosen}
	err := row.Scan(append(dest, extra...)...)
	r.ownerID = r.Owner.ID
	r.ItemsToPick = r.ItemCount - r.Chosen
	return r, err
}

func getRequest(ctx context.Context, q db.Querier, id uuid.UUID, forUpdate bool) (Request, error) {
	sql := requestSelect + ` WHERE r.id = $1`
	if forUpdate {
		sql += ` FOR UPDATE OF r`
	}
	return scanRequest(q.QueryRow(ctx, sql, id))
}

func (s *Service) attachRefs(ctx context.Context, rs []*Request) {
	var ids []uuid.UUID
	for _, r := range rs {
		if r.clientID != nil {
			ids = append(ids, *r.clientID)
		}
	}
	if len(ids) > 0 {
		if refs, err := s.clients.Lookup(ctx, ids); err == nil {
			for _, r := range rs {
				if r.clientID != nil {
					c := refs[*r.clientID]
					r.Client = &Ref{ID: *r.clientID, Reference: c.Reference, Name: c.LegalName}
				}
			}
		}
	}
	for _, r := range rs {
		if r.dealID != nil {
			if d, err := s.deals.Get(ctx, *r.dealID); err == nil {
				r.Deal = &Ref{ID: d.ID, Reference: d.Reference, Name: d.Title}
			}
		}
	}
}

func (s *Service) ListRequests(ctx context.Context, f RequestFilter) (httpx.List[Request], error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := arg("%" + escapeLike(q) + "%")
		where = append(where, `(r.reference ILIKE `+like+` OR r.title ILIKE `+like+
			` OR EXISTS (SELECT 1 FROM price_request_items i WHERE i.request_id = r.id AND (i.description ILIKE `+like+` OR i.part_no ILIKE `+like+` OR i.brand ILIKE `+like+`))`+
			` OR EXISTS (SELECT 1 FROM price_request_invites v JOIN suppliers sp ON sp.id = v.supplier_id WHERE v.request_id = r.id AND sp.name ILIKE `+like+`))`)
	}
	switch f.Status {
	case "":
	case "open":
		where = append(where, `r.status IN ('draft','sent')`)
	default:
		where = append(where, `r.status = `+arg(f.Status))
	}
	if f.OwnerID != nil {
		where = append(where, `r.owner_id = `+arg(*f.OwnerID))
	}
	if f.ClientID != nil {
		where = append(where, `r.client_id = `+arg(*f.ClientID))
	}
	sql := strings.Replace(requestSelect, "i.chosen_invite_id IS NOT NULL)\n", "i.chosen_invite_id IS NOT NULL), count(*) OVER ()\n", 1)
	if len(where) > 0 {
		sql += ` WHERE ` + strings.Join(where, " AND ")
	}
	sql += ` ORDER BY (r.status IN ('draft','sent')) DESC, r.needed_by ASC NULLS LAST, r.created_at DESC LIMIT ` + arg(f.Limit) + ` OFFSET ` + arg(f.Offset)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return httpx.List[Request]{}, err
	}
	defer rows.Close()
	items := []Request{}
	total := 0
	for rows.Next() {
		r, err := scanRequest(rows, &total)
		if err != nil {
			return httpx.List[Request]{}, err
		}
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return httpx.List[Request]{}, err
	}
	ptrs := make([]*Request, len(items))
	for i := range items {
		ptrs[i] = &items[i]
	}
	s.attachRefs(ctx, ptrs)
	return httpx.List[Request]{Items: items, Total: total, Limit: f.Limit, Offset: f.Offset}, nil
}

func (s *Service) GetRequest(ctx context.Context, id uuid.UUID) (Request, error) {
	r, err := getRequest(ctx, s.pool, id, false)
	if db.IsNoRows(err) {
		return Request{}, httpx.ErrNotFound
	}
	if err != nil {
		return Request{}, err
	}
	if err := s.loadDetail(ctx, s.pool, &r); err != nil {
		return Request{}, err
	}
	s.attachRefs(ctx, []*Request{&r})
	return r, nil
}

func listItems(ctx context.Context, q db.Querier, id uuid.UUID) ([]Item, error) {
	rows, err := q.Query(ctx, `SELECT id, position, description, part_no, brand, quantity::text, unit, chosen_invite_id
		FROM price_request_items WHERE request_id = $1 ORDER BY position`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Item, error) {
		var it Item
		err := r.Scan(&it.ID, &it.Position, &it.Description, &it.PartNo, &it.Brand, &it.Quantity, &it.Unit, &it.ChosenInviteID)
		if q, e := parseMoney(it.Quantity); e == nil {
			it.Quantity = q.Trimmed()
		}
		return it, err
	})
}

// loadDetail fills in items, invites with prices, and the comparison.
func (s *Service) loadDetail(ctx context.Context, q db.Querier, r *Request) error {
	var err error
	if r.Items, err = listItems(ctx, q, r.ID); err != nil {
		return err
	}
	rows, err := q.Query(ctx, `
		SELECT v.id, sp.id, sp.reference, sp.name, v.account_id, a.region, a.terms, v.sent_to, v.emailed, v.sent_at, u.full_name,
		       v.status, v.replied_at, v.currency, v.lead_time, to_char(v.valid_until, 'YYYY-MM-DD'), v.reply_note
		FROM price_request_invites v
		JOIN suppliers sp ON sp.id = v.supplier_id
		JOIN supplier_accounts a ON a.id = v.account_id
		JOIN users u ON u.id = v.sent_by
		WHERE v.request_id = $1 ORDER BY v.sent_at, sp.name`, r.ID)
	if err != nil {
		return err
	}
	r.Invites, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Invite, error) {
		var v Invite
		err := row.Scan(&v.ID, &v.Supplier.ID, &v.Supplier.Reference, &v.Supplier.Name, &v.AccountID, &v.Region, &v.Terms,
			&v.SentTo, &v.Emailed, &v.SentAt, &v.SentBy, &v.Status, &v.RepliedAt, &v.Currency, &v.LeadTime, &v.ValidUntil, &v.ReplyNote)
		v.Prices = []Price{}
		return v, err
	})
	if err != nil {
		return err
	}
	idx := map[uuid.UUID]int{}
	for i, v := range r.Invites {
		idx[v.ID] = i
	}
	prows, err := q.Query(ctx, `SELECT p.invite_id, p.item_id, p.unit_cost::text, p.availability
		FROM price_request_prices p JOIN price_request_invites v ON v.id = p.invite_id WHERE v.request_id = $1`, r.ID)
	if err != nil {
		return err
	}
	for prows.Next() {
		var inv uuid.UUID
		var p Price
		if err := prows.Scan(&inv, &p.ItemID, &p.UnitCost, &p.Availability); err != nil {
			prows.Close()
			return err
		}
		if i, ok := idx[inv]; ok {
			r.Invites[i].Prices = append(r.Invites[i].Prices, p)
		}
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return err
	}
	rates, err := s.ratesMap(ctx, q)
	if err != nil {
		return err
	}
	s.compare(r, rates)
	return nil
}

func (s *Service) ratesMap(ctx context.Context, q db.Querier) (map[string]Rate, error) {
	rows, err := q.Query(ctx, `SELECT r.currency, trim_scale(r.rate_to_zmw)::text, u.full_name, r.updated_at
		FROM fx_rates r JOIN users u ON u.id = r.updated_by`)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Rate, error) {
		var x Rate
		err := row.Scan(&x.Currency, &x.RateToZMW, &x.UpdatedBy, &x.UpdatedAt)
		return x, err
	})
	out := map[string]Rate{}
	for _, x := range list {
		out[x.Currency] = x
	}
	return out, err
}

// compare works out each reply's totals, in its own currency and in ZMW, and
// marks the cheapest complete reply and the cheapest price for each item.
// Replies in a currency with no exchange rate can't be compared with others.
func (s *Service) compare(r *Request, rates map[string]Rate) {
	qty := map[uuid.UUID]cents{}
	for _, it := range r.Items {
		q, _ := parseMoney(it.Quantity)
		qty[it.ID] = q
	}
	rateOf := func(cur string) (string, bool) {
		if cur == "ZMW" {
			return "1", true
		}
		x, ok := rates[cur]
		return x.RateToZMW, ok
	}
	used := map[string]bool{}
	type best struct {
		invite uuid.UUID
		v      cents
	}
	bestItem := map[uuid.UUID]best{}
	var cheapest *int
	var cheapestV cents
	now := s.now()
	for i := range r.Invites {
		v := &r.Invites[i]
		if v.Status == "waiting" {
			v.DaysQuiet = int(now.Sub(v.SentAt).Hours() / 24)
		}
		if v.Status != "replied" || v.Currency == nil {
			continue
		}
		cur := *v.Currency
		if cur != "ZMW" {
			used[cur] = true
		}
		rate, haveRate := rateOf(cur)
		var total, totalZ cents
		priced := 0
		for j := range v.Prices {
			p := &v.Prices[j]
			if p.UnitCost == nil {
				continue
			}
			u, err := parseMoney(*p.UnitCost)
			if err != nil {
				continue
			}
			lt := lineTotal(qty[p.ItemID], u)
			ls := lt.String()
			p.LineTotal = &ls
			total += lt
			priced++
			if haveRate {
				z, _ := toZMW(lt, rate)
				zs := z.String()
				p.LineTotalZMW = &zs
				totalZ += z
				if b, ok := bestItem[p.ItemID]; !ok || z < b.v {
					bestItem[p.ItemID] = best{v.ID, z}
				}
			}
		}
		ts := total.String()
		v.Total = &ts
		v.Complete = priced == len(r.Items) && priced > 0
		if haveRate {
			zs := totalZ.String()
			v.TotalZMW = &zs
			if v.Complete && (cheapest == nil || totalZ < cheapestV) {
				ii := i
				cheapest, cheapestV = &ii, totalZ
			}
		}
	}
	if cheapest != nil {
		r.Invites[*cheapest].Cheapest = true
	}
	for i := range r.Items {
		if b, ok := bestItem[r.Items[i].ID]; ok {
			id := b.invite
			r.Items[i].BestInviteID = &id
		}
	}
	r.Rates = []Rate{}
	for cur := range used {
		if x, ok := rates[cur]; ok {
			r.Rates = append(r.Rates, x)
		} else {
			r.Rates = append(r.Rates, Rate{Currency: cur})
		}
	}
	sort.Slice(r.Rates, func(i, j int) bool { return r.Rates[i].Currency < r.Rates[j].Currency })
}

// ---- create and edit ----

type cleanItem struct {
	ItemInput
	qty cents
}

func (s *Service) checkRequest(ctx context.Context, in *RequestInput) ([]cleanItem, error) {
	fe := httpx.FieldErrors{}
	in.Title = strings.Join(strings.Fields(in.Title), " ")
	if in.Title == "" {
		fe.Add("title", "Say what it's for, e.g. 3 colour copiers for Zanaco.")
	} else if utf8.RuneCountInString(in.Title) > 200 {
		fe.Add("title", "Keep the title under 200 characters.")
	}
	in.Notes, in.InternalNote = clean(in.Notes), clean(in.InternalNote)
	for _, f := range []struct {
		k string
		v *string
	}{{"notes", in.Notes}, {"internal_note", in.InternalNote}} {
		if f.v != nil && utf8.RuneCountInString(*f.v) > 2000 {
			fe.Add(f.k, "Keep it under 2,000 characters.")
		}
	}
	if nb := clean(in.NeededBy); nb != nil {
		t, err := time.Parse("2006-01-02", *nb)
		if err != nil {
			fe.Add("needed_by", "Use a date like 2026-10-31.")
		}
		d := t.Format("2006-01-02")
		in.NeededBy = &d
	} else {
		in.NeededBy = nil
	}
	if in.ClientID != nil {
		if _, err := s.clients.Get(ctx, *in.ClientID); err != nil {
			fe.Add("client_id", "Choose a client from the list.")
		}
	}
	if in.DealID != nil {
		d, err := s.deals.Get(ctx, *in.DealID)
		if err != nil {
			fe.Add("deal_id", "Choose a deal from the list.")
		} else if in.ClientID == nil {
			cid := d.Client.ID
			in.ClientID = &cid
		} else if d.Client.ID != *in.ClientID {
			fe.Add("deal_id", "That deal is for a different client.")
		}
	}
	if len(in.Items) > maxItems {
		fe.Add("items", fmt.Sprintf("Up to %d items per request.", maxItems))
	}
	var out []cleanItem
	for i, it := range in.Items {
		p := fmt.Sprintf("items.%d.", i)
		it.Description = strings.TrimSpace(it.Description)
		if it.Description == "" {
			fe.Add(p+"description", "Describe the item.")
		} else if utf8.RuneCountInString(it.Description) > 1000 {
			fe.Add(p+"description", "Keep it under 1,000 characters.")
		}
		it.PartNo, it.Brand = clean(it.PartNo), clean(it.Brand)
		it.Unit = strings.TrimSpace(it.Unit)
		if it.Unit == "" {
			it.Unit = "each"
		}
		q, err := parseMoney(it.Quantity)
		if err != nil || q <= 0 {
			fe.Add(p+"quantity", "Enter how many, e.g. 3.")
		}
		out = append(out, cleanItem{ItemInput: it, qty: q})
	}
	return out, fe.Err()
}

func (s *Service) CreateRequest(ctx context.Context, in RequestInput) (Request, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return Request{}, httpx.ErrUnauthorized
	}
	items, err := s.checkRequest(ctx, &in)
	if err != nil {
		return Request{}, err
	}
	var id uuid.UUID
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		ref, err := s.numbers.Next(ctx, tx, numbering.PriceRequest)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO price_requests (reference, title, needed_by, client_id, deal_id, notes, internal_note, owner_id, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8) RETURNING id`, ref, in.Title, in.NeededBy, in.ClientID, in.DealID, in.Notes, in.InternalNote, actor.UserID).Scan(&id); err != nil {
			return err
		}
		if err := replaceItems(ctx, tx, id, items); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: requestEntity, EntityID: id, Action: "created",
			Summary: fmt.Sprintf("Price request started with %d item(s)", len(items))})
	})
	if err != nil {
		return Request{}, err
	}
	return s.GetRequest(ctx, id)
}

func replaceItems(ctx context.Context, tx pgx.Tx, id uuid.UUID, items []cleanItem) error {
	if _, err := tx.Exec(ctx, `DELETE FROM price_request_items WHERE request_id = $1`, id); err != nil {
		return err
	}
	for i, it := range items {
		if _, err := tx.Exec(ctx, `INSERT INTO price_request_items (request_id, position, description, part_no, brand, quantity, unit)
			VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, i+1, it.Description, it.PartNo, it.Brand, it.qty.String(), it.Unit); err != nil {
			return err
		}
	}
	return nil
}

// UpdateRequest changes the details. Items can change only until a supplier
// has replied, because replies are priced against them.
func (s *Service) UpdateRequest(ctx context.Context, id uuid.UUID, in RequestInput) (Request, error) {
	items, err := s.checkRequest(ctx, &in)
	if err != nil {
		return Request{}, err
	}
	err = s.mutate(ctx, id, in.Version, func(tx pgx.Tx, _ auth.Principal, r *Request) (audit.Entry, error) {
		if r.Status == StatusCancelled {
			return audit.Entry{}, errCancelled
		}
		if _, err := tx.Exec(ctx, `UPDATE price_requests SET title=$2, needed_by=$3, client_id=$4, deal_id=$5, notes=$6, internal_note=$7
			WHERE id=$1`, id, in.Title, in.NeededBy, in.ClientID, in.DealID, in.Notes, in.InternalNote); err != nil {
			return audit.Entry{}, err
		}
		if in.Items != nil {
			old, err := listItems(ctx, tx, id)
			if err != nil {
				return audit.Entry{}, err
			}
			if itemsChanged(old, items) {
				if r.Replied > 0 {
					return audit.Entry{}, httpx.NewError(http.StatusConflict, "has_replies",
						"Suppliers have already replied, so the items can't change. Start a new request for different items.")
				}
				if len(items) == 0 {
					return audit.Entry{}, errNoItems
				}
				if err := replaceItems(ctx, tx, id, items); err != nil {
					return audit.Entry{}, err
				}
			}
		}
		return audit.Entry{EntityType: requestEntity, EntityID: id, Action: "updated", Summary: "Details changed"}, nil
	})
	if err != nil {
		return Request{}, err
	}
	return s.GetRequest(ctx, id)
}

func itemsChanged(old []Item, now []cleanItem) bool {
	if len(old) != len(now) {
		return true
	}
	for i := range old {
		o, n := old[i], now[i]
		oq, _ := parseMoney(o.Quantity)
		if o.Description != n.Description || deref(o.PartNo) != deref(n.PartNo) || deref(o.Brand) != deref(n.Brand) ||
			oq != n.qty || o.Unit != n.Unit {
			return true
		}
	}
	return false
}

// ---- asking suppliers ----

type target struct {
	accountID, supplierID uuid.UUID
	supplier, region      string
	accountNo, contact    *string
	email                 string
	others                []string
}

// Send asks the chosen supplier accounts for prices. Each supplier gets its
// own email; one failing doesn't stop the others.
func (s *Service) Send(ctx context.Context, id uuid.UUID, in SendInput) (SendResult, error) {
	var res SendResult
	res.Sent, res.Failed = []string{}, []SendFailed{}
	in.Message, in.Via = strings.TrimSpace(in.Message), strings.TrimSpace(in.Via)
	fe := httpx.FieldErrors{}
	if len(in.AccountIDs) == 0 {
		fe.Add("account_ids", "Choose at least one supplier to ask.")
	}
	if len(in.AccountIDs) > 30 {
		fe.Add("account_ids", "Ask up to 30 suppliers at a time.")
	}
	if utf8.RuneCountInString(in.Message) > 2000 {
		fe.Add("message", "Keep the message under 2,000 characters.")
	}
	if !in.Email && in.Via == "" {
		fe.Add("via", "Say how you asked them, e.g. by phone or on WhatsApp.")
	}
	if err := fe.Err(); err != nil {
		return res, err
	}
	err := s.mutate(ctx, id, in.Version, func(tx pgx.Tx, actor auth.Principal, r *Request) (audit.Entry, error) {
		if r.Status == StatusCancelled {
			return audit.Entry{}, errCancelled
		}
		items, err := listItems(ctx, tx, id)
		if err != nil {
			return audit.Entry{}, err
		}
		if len(items) == 0 {
			return audit.Entry{}, errNoItems
		}
		rows, err := tx.Query(ctx, `
			SELECT a.id, sp.id, sp.name, a.region, a.account_number, a.contact_name, a.contact_email, a.other_emails
			FROM supplier_accounts a JOIN suppliers sp ON sp.id = a.supplier_id
			WHERE a.id = ANY($1) AND a.active AND sp.status = 'active'
			  AND NOT EXISTS (SELECT 1 FROM price_request_invites v WHERE v.request_id = $2 AND v.account_id = a.id)
			ORDER BY sp.name`, in.AccountIDs, id)
		if err != nil {
			return audit.Entry{}, err
		}
		targets, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (target, error) {
			var t target
			err := row.Scan(&t.accountID, &t.supplierID, &t.supplier, &t.region, &t.accountNo, &t.contact, &t.email, &t.others)
			return t, err
		})
		if err != nil {
			return audit.Entry{}, err
		}
		if len(targets) == 0 {
			return audit.Entry{}, httpx.NewError(http.StatusConflict, "already_asked",
				"Those suppliers have already been asked on this request, or are no longer active.")
		}
		var myEmail string
		_ = tx.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, actor.UserID).Scan(&myEmail)
		for _, t := range targets {
			sentTo := in.Via
			if in.Email {
				msg := s.rfqEmail(r, items, t, actor.Name, myEmail, in)
				if err := s.mail.Send(ctx, msg); err != nil {
					s.log.Warn("price request email failed", "supplier", t.supplier, "err", err)
					res.Failed = append(res.Failed, SendFailed{Supplier: t.supplier, Reason: "The email couldn't be sent. Check the address and try again."})
					continue
				}
				sentTo = t.email
				if in.CopyOthers && len(t.others) > 0 {
					sentTo += " + " + strconv.Itoa(len(t.others)) + " more"
				}
			}
			if _, err := tx.Exec(ctx, `INSERT INTO price_request_invites (request_id, supplier_id, account_id, sent_to, emailed, sent_by)
				VALUES ($1,$2,$3,$4,$5,$6)`, id, t.supplierID, t.accountID, sentTo, in.Email, actor.UserID); err != nil {
				return audit.Entry{}, err
			}
			res.Sent = append(res.Sent, t.supplier+" ("+regions[t.region]+")")
		}
		if len(res.Sent) == 0 {
			return audit.Entry{}, httpx.NewError(http.StatusBadGateway, "email_failed",
				"None of the emails could be sent, so nobody is recorded as asked. Try again in a moment.")
		}
		if _, err := tx.Exec(ctx, `UPDATE price_requests SET status = CASE WHEN status = 'draft' THEN 'sent' ELSE status END,
			sent_at = coalesce(sent_at, now()) WHERE id = $1`, id); err != nil {
			return audit.Entry{}, err
		}
		how := "Emailed"
		if !in.Email {
			how = "Asked (" + in.Via + ")"
		}
		return audit.Entry{EntityType: requestEntity, EntityID: id, Action: "sent",
			Summary: how + ": " + strings.Join(res.Sent, ", ")}, nil
	})
	if err != nil {
		return res, err
	}
	res.Emailed = in.Email && s.mail.Enabled()
	if in.Email && !s.mail.Enabled() {
		res.Note = "Email isn't set up on this server, so the messages were written to the server log instead of being sent."
	}
	res.Request, err = s.GetRequest(ctx, id)
	return res, err
}

func (s *Service) rfqEmail(r *Request, items []Item, t target, myName, myEmail string, in SendInput) mailer.Message {
	greeting := "Good day"
	if t.contact != nil {
		greeting = "Dear " + strings.Fields(*t.contact)[0]
	}
	paras := []string{
		greeting + ",",
		fmt.Sprintf("%s would like your best price and availability for the item%s below.", s.company, map[bool]string{true: "", false: "s"}[len(items) == 1]),
	}
	if r.NeededBy != nil {
		if t, err := time.Parse("2006-01-02", *r.NeededBy); err == nil {
			paras[1] = strings.TrimSuffix(paras[1], ".") + ", needed by " + t.Format("Monday 2 January 2006") + "."
		}
	}
	if in.Message != "" {
		paras = append(paras, in.Message)
	}
	if r.Notes != nil {
		paras = append(paras, *r.Notes)
	}
	after := []string{"Please reply to this email with the unit price and currency for each item, stock availability or lead time, and how long your prices are valid."}
	var rows [][2]string
	for _, it := range items {
		label := fmt.Sprintf("%d. %s", it.Position, it.Description)
		var bits []string
		if it.Brand != nil {
			bits = append(bits, "Brand: "+*it.Brand)
		}
		if it.PartNo != nil {
			bits = append(bits, "Part no: "+*it.PartNo)
		}
		qty := "Qty " + it.Quantity
		if it.Unit != "each" {
			qty += " " + it.Unit
		}
		if len(bits) > 0 {
			qty += " · " + strings.Join(bits, " · ")
		}
		rows = append(rows, [2]string{label, qty})
	}
	foot := "Our reference: " + r.Reference
	if t.accountNo != nil {
		foot += " · Our account with you: " + *t.accountNo
	}
	foot += ". Please quote it in your reply."
	sign := myName
	if sign == "" {
		sign = s.company
	}
	after = append(after, "Kind regards,\n"+sign+"\n"+s.company)
	c := mailer.Content{
		Preheader:  "Request for quotation " + r.Reference + " from " + s.company,
		Heading:    "Request for quotation",
		Paragraphs: paras,
		Details:    rows,
		After:      after,
		Footnote:   foot,
	}
	htmlBody, text := mailer.Render(c)
	msg := mailer.Message{To: t.email, Kind: "price_request", HTML: htmlBody, Text: text,
		Subject: "Request for quotation " + r.Reference + " — " + s.company}
	if t.contact != nil {
		msg.ToName = *t.contact
	}
	if in.CopyOthers {
		msg.Cc = append(msg.Cc, t.others...)
	}
	if myEmail != "" {
		msg.ReplyTo = myEmail
		if in.CopyMe {
			msg.Cc = append(msg.Cc, myEmail)
		}
	}
	return msg
}

// ---- replies ----

func (s *Service) RecordReply(ctx context.Context, id, inviteID uuid.UUID, in ReplyInput) (Request, error) {
	var supplier string
	err := s.mutate(ctx, id, in.Version, func(tx pgx.Tx, _ auth.Principal, r *Request) (audit.Entry, error) {
		if r.Status == StatusCancelled {
			return audit.Entry{}, errCancelled
		}
		var status string
		err := tx.QueryRow(ctx, `SELECT sp.name, v.status FROM price_request_invites v JOIN suppliers sp ON sp.id = v.supplier_id
			WHERE v.id = $1 AND v.request_id = $2`, inviteID, id).Scan(&supplier, &status)
		if db.IsNoRows(err) {
			return audit.Entry{}, httpx.ErrNotFound
		}
		if err != nil {
			return audit.Entry{}, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM price_request_prices WHERE invite_id = $1`, inviteID); err != nil {
			return audit.Entry{}, err
		}
		note := clean(in.Note)
		if in.Declined {
			if _, err := tx.Exec(ctx, `UPDATE price_request_items SET chosen_invite_id = NULL WHERE chosen_invite_id = $1`, inviteID); err != nil {
				return audit.Entry{}, err
			}
			if _, err := tx.Exec(ctx, `UPDATE price_request_invites SET status='declined', replied_at=now(), currency=NULL, lead_time=NULL,
				valid_until=NULL, reply_note=$2 WHERE id=$1`, inviteID, note); err != nil {
				return audit.Entry{}, err
			}
			return audit.Entry{EntityType: requestEntity, EntityID: id, Action: "declined_by_supplier",
				Summary: supplier + " can't quote", Reason: deref(note)}, nil
		}
		fe := httpx.FieldErrors{}
		cur := strings.ToUpper(strings.TrimSpace(in.Currency))
		if len(cur) != 3 {
			fe.Add("currency", "Choose the currency they quoted in.")
		}
		var vu *string
		if v := clean(in.ValidUntil); v != nil {
			t, err := time.Parse("2006-01-02", *v)
			if err != nil {
				fe.Add("valid_until", "Use a date like 2026-10-31.")
			} else {
				d := t.Format("2006-01-02")
				vu = &d
			}
		}
		items, err := listItems(ctx, tx, id)
		if err != nil {
			return audit.Entry{}, err
		}
		onRequest := map[uuid.UUID]bool{}
		for _, it := range items {
			onRequest[it.ID] = true
		}
		priced := 0
		type row struct {
			item  uuid.UUID
			cost  *string
			avail *string
		}
		var save []row
		seen := map[uuid.UUID]bool{}
		for i, p := range in.Prices {
			if !onRequest[p.ItemID] || seen[p.ItemID] {
				fe.Add(fmt.Sprintf("prices.%d", i), "That item isn't on this request.")
				continue
			}
			seen[p.ItemID] = true
			var cost *string
			if c := clean(p.UnitCost); c != nil {
				v, err := parseMoney(*c)
				if err != nil {
					fe.Add(fmt.Sprintf("prices.%d.unit_cost", i), "Enter a price, e.g. 12500 or 12,500.50.")
					continue
				}
				str := v.String()
				cost = &str
				priced++
			}
			av := clean(p.Availability)
			if cost != nil || av != nil {
				save = append(save, row{p.ItemID, cost, av})
			}
		}
		if priced == 0 && len(fe) == 0 {
			fe.Add("prices", "Enter at least one price, or mark the supplier as unable to quote.")
		}
		if err := fe.Err(); err != nil {
			return audit.Entry{}, err
		}
		for _, x := range save {
			if _, err := tx.Exec(ctx, `INSERT INTO price_request_prices (invite_id, item_id, unit_cost, availability) VALUES ($1,$2,$3,$4)`,
				inviteID, x.item, x.cost, x.avail); err != nil {
				return audit.Entry{}, err
			}
		}
		// A choice of this supplier for an item it no longer prices is cleared.
		if _, err := tx.Exec(ctx, `UPDATE price_request_items i SET chosen_invite_id = NULL
			WHERE i.chosen_invite_id = $1 AND NOT EXISTS (SELECT 1 FROM price_request_prices p WHERE p.invite_id = $1 AND p.item_id = i.id AND p.unit_cost IS NOT NULL)`,
			inviteID); err != nil {
			return audit.Entry{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE price_request_invites SET status='replied', replied_at=coalesce(CASE WHEN status='replied' THEN replied_at END, now()),
			currency=$2, lead_time=$3, valid_until=$4, reply_note=$5 WHERE id=$1`, inviteID, cur, clean(in.LeadTime), vu, note); err != nil {
			return audit.Entry{}, err
		}
		action, summary := "reply_recorded", fmt.Sprintf("%s replied: %d of %d item(s) priced in %s", supplier, priced, len(items), cur)
		if status == "replied" {
			action, summary = "reply_updated", supplier+"'s prices updated"
		}
		return audit.Entry{EntityType: requestEntity, EntityID: id, Action: action, Summary: summary}, nil
	})
	if err != nil {
		return Request{}, err
	}
	return s.GetRequest(ctx, id)
}

// Decide records which supplier was chosen for each item.
func (s *Service) Decide(ctx context.Context, id uuid.UUID, in DecideInput) (Request, error) {
	err := s.mutate(ctx, id, in.Version, func(tx pgx.Tx, _ auth.Principal, r *Request) (audit.Entry, error) {
		if r.Status == StatusCancelled {
			return audit.Entry{}, errCancelled
		}
		if err := s.loadDetail(ctx, tx, r); err != nil {
			return audit.Entry{}, err
		}
		priced := map[[2]uuid.UUID]bool{}
		names := map[uuid.UUID]string{}
		for _, v := range r.Invites {
			names[v.ID] = v.Supplier.Name
			if v.Status != "replied" {
				continue
			}
			for _, p := range v.Prices {
				if p.UnitCost != nil {
					priced[[2]uuid.UUID{v.ID, p.ItemID}] = true
				}
			}
		}
		items := map[uuid.UUID]Item{}
		for _, it := range r.Items {
			items[it.ID] = it
		}
		fe := httpx.FieldErrors{}
		chosen := 0
		picks := map[string]int{}
		for i, c := range in.Choices {
			it, ok := items[c.ItemID]
			if !ok {
				fe.Add(fmt.Sprintf("choices.%d", i), "That item isn't on this request.")
				continue
			}
			if c.InviteID != nil {
				if !priced[[2]uuid.UUID{*c.InviteID, c.ItemID}] {
					fe.Add(fmt.Sprintf("choices.%d", i), "That supplier didn't price "+it.Description+".")
					continue
				}
				chosen++
				picks[names[*c.InviteID]]++
			}
			if _, err := tx.Exec(ctx, `UPDATE price_request_items SET chosen_invite_id = $2 WHERE id = $1`, c.ItemID, c.InviteID); err != nil {
				return audit.Entry{}, err
			}
		}
		if chosen == 0 && len(fe) == 0 {
			fe.Add("choices", "Choose a supplier for at least one item.")
		}
		if err := fe.Err(); err != nil {
			return audit.Entry{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE price_requests SET status='decided', decided_at=now() WHERE id=$1`, id); err != nil {
			return audit.Entry{}, err
		}
		var parts []string
		for n, c := range picks {
			parts = append(parts, fmt.Sprintf("%s (%d item%s)", n, c, map[bool]string{true: "", false: "s"}[c == 1]))
		}
		sort.Strings(parts)
		return audit.Entry{EntityType: requestEntity, EntityID: id, Action: "decided",
			Summary: "Suppliers chosen: " + strings.Join(parts, ", "), Reason: strings.TrimSpace(in.Note)}, nil
	})
	if err != nil {
		return Request{}, err
	}
	return s.GetRequest(ctx, id)
}

func (s *Service) Cancel(ctx context.Context, id uuid.UUID, version int, reason string) (Request, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Request{}, httpx.FieldErrors{"reason": "Say why, e.g. the client went elsewhere."}.Err()
	}
	err := s.mutate(ctx, id, version, func(tx pgx.Tx, _ auth.Principal, r *Request) (audit.Entry, error) {
		if r.Status == StatusCancelled {
			return audit.Entry{}, errCancelled
		}
		if _, err := tx.Exec(ctx, `UPDATE price_requests SET status='cancelled', cancelled_at=now(), closed_reason=$2 WHERE id=$1`, id, reason); err != nil {
			return audit.Entry{}, err
		}
		return audit.Entry{EntityType: requestEntity, EntityID: id, Action: "cancelled", Summary: "Request cancelled", Reason: reason}, nil
	})
	if err != nil {
		return Request{}, err
	}
	return s.GetRequest(ctx, id)
}

func (s *Service) RequestActivity(ctx context.Context, id uuid.UUID) ([]audit.Event, error) {
	return audit.Timeline(ctx, s.pool, requestEntity, id, 200)
}

// mutate locks the request, checks the version and that the person may change
// it, and runs fn with its audit entry in one transaction.
func (s *Service) mutate(ctx context.Context, id uuid.UUID, version int, fn func(tx pgx.Tx, actor auth.Principal, r *Request) (audit.Entry, error)) error {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return httpx.ErrUnauthorized
	}
	if version < 1 {
		return httpx.FieldErrors{"version": "Version is required."}.Err()
	}
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		r, err := getRequest(ctx, tx, id, true)
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if r.ownerID != actor.UserID && !actor.Can(PermManage) {
			return errNotYours
		}
		if r.Version != version {
			return httpx.ErrStaleVersion.With(map[string]any{"current_version": r.Version})
		}
		entry, err := fn(tx, actor, &r)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE price_requests SET updated_at=now(), version=version+1 WHERE id=$1`, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, entry)
	})
}

// ---- reminders ----

// QuietAfterDays is how long a supplier can go without replying before the
// request's owner is reminded to chase them.
const QuietAfterDays = 2

// Remind tells owners about suppliers that haven't replied, once per supplier.
func (s *Service) Remind(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT v.id, r.id, r.reference, r.owner_id, sp.name, v.sent_at
		FROM price_request_invites v
		JOIN price_requests r ON r.id = v.request_id
		JOIN suppliers sp ON sp.id = v.supplier_id
		JOIN users o ON o.id = r.owner_id AND o.active
		WHERE v.status = 'waiting' AND r.status = 'sent' AND v.sent_at < $1`, now.Add(-QuietAfterDays*24*time.Hour))
	if err != nil {
		return 0, err
	}
	type quiet struct {
		invite, request, owner uuid.UUID
		ref, supplier          string
		sent                   time.Time
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (quiet, error) {
		var q quiet
		err := row.Scan(&q.invite, &q.request, &q.ref, &q.owner, &q.supplier, &q.sent)
		return q, err
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, q := range list {
		rid := q.request
		days := int(now.Sub(q.sent).Hours() / 24)
		ok, err := notify.Create(ctx, s.pool, notify.Notification{UserID: q.owner, Kind: KindNoReply,
			Title: "No reply yet from " + q.supplier, Body: fmt.Sprintf("%s · asked %d days ago. Chase them, or record that they can't quote.", q.ref, days),
			Link: "/sourcing/" + q.request.String(), EntityType: requestEntity, EntityID: &rid,
			DedupeKey: KindNoReply + ":" + q.invite.String()})
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	return n, nil
}

func (s *Service) StartReminders(ctx context.Context, interval time.Duration) {
	go func() {
		timer := time.NewTimer(40 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if n, err := s.Remind(ctx, time.Now()); err != nil && ctx.Err() == nil {
				s.log.Error("price request reminders failed", "err", err)
			} else if n > 0 {
				s.log.Info("price request reminders sent", "notifications", n)
			}
			timer.Reset(interval)
		}
	}()
}
