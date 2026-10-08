package orders

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dos-bpas/internal/platform/db"
)

// All SQL for the customer_orders tables lives here. Clients, quotations and
// deals are reached through their modules' APIs; only their names are joined
// for display.

const orderSelect = `
	SELECT o.id, o.reference, o.client_id, cl.reference, cl.legal_name,
	       o.quote_id, o.quote_number, o.deal_id, o.contact_id, o.site_id,
	       o.title, o.currency, o.client_po, to_char(o.client_po_date, 'YYYY-MM-DD'),
	       to_char(o.order_date, 'YYYY-MM-DD'), to_char(o.delivery_due, 'YYYY-MM-DD'),
	       o.delivery_instructions, o.payment_terms, o.internal_note,
	       o.status, o.hold_reason, o.held_at,
	       o.subtotal::text, o.discount_total::text, o.net_total::text, o.vat_total::text, o.total::text,
	       o.delivered_at, o.invoice_ref, to_char(o.invoiced_on, 'YYYY-MM-DD'), o.closed_reason, o.cancelled_at,
	       ow.id, ow.full_name, ow.position, cb.id, cb.full_name, cb.position,
	       o.created_at, o.updated_at, o.version,
	       coalesce(p.ready_pct, 0), coalesce(p.delivered_pct, 0), coalesce(p.outstanding, false)
	FROM customer_orders o
	JOIN clients cl ON cl.id = o.client_id
	JOIN users ow ON ow.id = o.owner_id
	JOIN users cb ON cb.id = o.created_by
	LEFT JOIN LATERAL (
		-- Weighted by value, so one copier counts for more than a box of toner;
		-- by quantity when every line is free.
		SELECT floor(coalesce(sum(l.line_net * greatest(l.qty_ready, l.qty_delivered) / l.quantity) * 100 / nullif(sum(l.line_net), 0),
		                      sum(greatest(l.qty_ready, l.qty_delivered)) * 100 / nullif(sum(l.quantity), 0)))::int AS ready_pct,
		       floor(coalesce(sum(l.line_net * l.qty_delivered / l.quantity) * 100 / nullif(sum(l.line_net), 0),
		                      sum(l.qty_delivered) * 100 / nullif(sum(l.quantity), 0)))::int AS delivered_pct,
		       bool_or(l.qty_delivered < l.quantity) AS outstanding
		FROM customer_order_lines l WHERE l.order_id = o.id
	) p ON true`

func scanOrder(r pgx.Row, today string, extra ...any) (Order, error) {
	var o Order
	var quoteNumber *string
	var outstanding bool
	dest := []any{&o.ID, &o.Reference, &o.clientID, &o.Client.Reference, &o.Client.LegalName,
		&o.quoteID, &quoteNumber, &o.dealID, &o.contactID, &o.siteID,
		&o.Title, &o.Currency, &o.ClientPO, &o.ClientPODate, &o.OrderDate, &o.DeliveryDue,
		&o.DeliveryInstructions, &o.PaymentTerms, &o.InternalNote,
		&o.Status, &o.HoldReason, &o.HeldAt,
		&o.Subtotal, &o.DiscountTotal, &o.NetTotal, &o.VATTotal, &o.Total,
		&o.DeliveredAt, &o.InvoiceRef, &o.InvoicedOn, &o.ClosedReason, &o.CancelledAt,
		&o.Owner.ID, &o.Owner.Name, &o.Owner.Position, &o.CreatedBy.ID, &o.CreatedBy.Name, &o.CreatedBy.Position,
		&o.CreatedAt, &o.UpdatedAt, &o.Version,
		&o.ReadyPct, &o.DeliveredPct, &outstanding}
	if err := r.Scan(append(dest, extra...)...); err != nil {
		return o, err
	}
	o.Client.ID = o.clientID
	o.ownerID = o.Owner.ID
	if o.quoteID != nil {
		o.Quote = &QuoteRef{ID: *o.quoteID, Number: deref(quoteNumber)}
	}
	o.Late = o.DeliveryDue != nil && *o.DeliveryDue < today && outstanding && !o.Status.Closed()
	return o, nil
}

func getOrder(ctx context.Context, q db.Querier, id uuid.UUID, today string, forUpdate bool) (Order, error) {
	sql := orderSelect + ` WHERE o.id = $1`
	if forUpdate {
		sql += ` FOR UPDATE OF o`
	}
	return scanOrder(q.QueryRow(ctx, sql, id), today)
}

func listOrders(ctx context.Context, q db.Querier, f ListFilter, today string, clientMatches []uuid.UUID) ([]Order, int, error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if s := strings.TrimSpace(f.Query); s != "" {
		like := arg("%" + escapeLike(s) + "%")
		cond := `(o.reference ILIKE ` + like + ` OR o.title ILIKE ` + like + ` OR o.client_po ILIKE ` + like + ` OR o.quote_number ILIKE ` + like
		if len(clientMatches) > 0 {
			cond += ` OR o.client_id = ANY(` + arg(clientMatches) + `)`
		}
		where = append(where, cond+`)`)
	}
	switch f.Status {
	case "":
	case "live":
		where = append(where, `o.status IN ('open','in_progress','ready','part_delivered','on_hold')`)
	case "late":
		where = append(where, `o.status IN ('open','in_progress','ready','part_delivered','on_hold') AND o.delivery_due < `+arg(today)+`::date`)
	case "open":
		where = append(where, `o.status IN ('open','in_progress')`)
	default:
		where = append(where, `o.status = `+arg(f.Status))
	}
	for _, x := range []struct {
		col string
		v   *uuid.UUID
	}{{"o.client_id", f.ClientID}, {"o.quote_id", f.QuoteID}, {"o.deal_id", f.DealID}, {"o.owner_id", f.OwnerID}} {
		if x.v != nil {
			where = append(where, x.col+` = `+arg(*x.v))
		}
	}
	sql := strings.Replace(orderSelect, "coalesce(p.outstanding, false)\n", "coalesce(p.outstanding, false), count(*) OVER ()\n", 1)
	if len(where) > 0 {
		sql += ` WHERE ` + strings.Join(where, " AND ")
	}
	// Live orders by promised date (soonest first), then the rest newest first.
	sql += ` ORDER BY (o.status IN ('open','in_progress','ready','part_delivered','on_hold')) DESC,
		CASE WHEN o.status IN ('open','in_progress','ready','part_delivered','on_hold') THEN o.delivery_due END ASC NULLS LAST,
		o.created_at DESC LIMIT ` + arg(f.Limit) + ` OFFSET ` + arg(f.Offset)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Order{}
	total := 0
	for rows.Next() {
		o, err := scanOrder(rows, today, &total)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, o)
	}
	return out, total, rows.Err()
}

func listLines(ctx context.Context, q db.Querier, orderID uuid.UUID) ([]Line, error) {
	rows, err := q.Query(ctx, `
		SELECT id, position, product_id, sku, description, quantity::text, unit, unit_price::text, discount_pct::text,
		       vat_rate::text, line_net::text, line_vat::text, line_total::text,
		       source, supplier_note, qty_ready::text, qty_delivered::text
		FROM customer_order_lines WHERE order_id = $1 ORDER BY position`, orderID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Line, error) {
		var l Line
		err := r.Scan(&l.ID, &l.Position, &l.ProductID, &l.SKU, &l.Description, &l.Quantity, &l.Unit, &l.UnitPrice,
			&l.DiscountPct, &l.VATRate, &l.LineNet, &l.LineVAT, &l.LineTotal, &l.Source, &l.SupplierNote, &l.QtyReady, &l.QtyDelivered)
		l.Quantity, l.QtyReady, l.QtyDelivered = trimQty(l.Quantity), trimQty(l.QtyReady), trimQty(l.QtyDelivered)
		l.DiscountPct, l.VATRate = trimQty(l.DiscountPct), trimQty(l.VATRate)
		return l, err
	})
}

func listDeliveries(ctx context.Context, q db.Querier, orderID uuid.UUID) ([]Delivery, error) {
	rows, err := q.Query(ctx, `
		SELECT d.id, to_char(d.delivered_on, 'YYYY-MM-DD'), d.reference, d.received_by, d.note,
		       u.id, u.full_name, u.position, d.created_at
		FROM customer_order_deliveries d JOIN users u ON u.id = d.created_by
		WHERE d.order_id = $1 ORDER BY d.delivered_on DESC, d.created_at DESC`, orderID)
	if err != nil {
		return nil, err
	}
	ds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Delivery, error) {
		var d Delivery
		err := r.Scan(&d.ID, &d.DeliveredOn, &d.Reference, &d.ReceivedBy, &d.Note, &d.CreatedBy.ID, &d.CreatedBy.Name, &d.CreatedBy.Position, &d.CreatedAt)
		d.Lines = []DeliveryLine{}
		return d, err
	})
	if err != nil || len(ds) == 0 {
		return ds, err
	}
	idx := map[uuid.UUID]int{}
	ids := make([]uuid.UUID, len(ds))
	for i, d := range ds {
		idx[d.ID], ids[i] = i, d.ID
	}
	rows, err = q.Query(ctx, `
		SELECT dl.delivery_id, dl.line_id, l.description, dl.quantity::text, l.unit
		FROM customer_order_delivery_lines dl JOIN customer_order_lines l ON l.id = dl.line_id
		WHERE dl.delivery_id = ANY($1) ORDER BY l.position`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var did uuid.UUID
		var l DeliveryLine
		if err := rows.Scan(&did, &l.LineID, &l.Description, &l.Quantity, &l.Unit); err != nil {
			return nil, err
		}
		l.Quantity = trimQty(l.Quantity)
		ds[idx[did]].Lines = append(ds[idx[did]].Lines, l)
	}
	return ds, rows.Err()
}

// setStatusFromLines moves a working order to the status its lines imply.
// Orders on hold, invoiced or cancelled keep their status.
func setStatusFromLines(ctx context.Context, tx pgx.Tx, o *Order, lines []Line, now time.Time) (Status, error) {
	_, _, st := progress(lines)
	if o.Status == StatusOnHold || o.Status.Closed() || st == o.Status {
		return o.Status, nil
	}
	var deliveredAt *time.Time
	if st == StatusDelivered {
		deliveredAt = &now
	}
	_, err := tx.Exec(ctx, `UPDATE customer_orders SET status = $2, delivered_at = $3 WHERE id = $1`, o.ID, st, deliveredAt)
	return st, err
}

func trimQty(s string) string {
	if q, err := parseQty(s); err == nil {
		return q.String()
	}
	return s
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
