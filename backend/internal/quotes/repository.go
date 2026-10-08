package quotes

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dos-bpas/internal/platform/db"
)

// All SQL for the quotes, quote_lines and products tables lives in this
// module. Clients and deals are reached through their modules' APIs.

const quoteSelect = `
	SELECT q.id, q.reference, q.revision, q.client_id, q.deal_id, q.contact_id, q.title, q.currency,
	       to_char(q.quote_date, 'YYYY-MM-DD'), to_char(q.valid_until, 'YYYY-MM-DD'), q.status,
	       q.notes, q.terms, q.delivery_terms, q.payment_terms, q.warranty_terms, q.internal_note,
	       q.subtotal::text, q.discount_total::text, q.net_total::text, q.vat_total::text, q.total::text,
	       q.discount_pct::text, q.max_line_discount::text, q.client_po,
	       o.id, o.full_name, o.position, o.email, coalesce(o.phone, ''), cb.id, cb.full_name, cb.position,
	       q.submitted_at, ap.id, ap.full_name, ap.position, q.approved_at, q.approval_note,
	       q.sent_at, q.sent_to, q.decided_at, q.decision_note, q.closed_reason,
	       q.created_at, q.updated_at, q.version
	FROM quotes q
	JOIN users o ON o.id = q.owner_id
	JOIN users cb ON cb.id = q.created_by
	LEFT JOIN users ap ON ap.id = q.approved_by`

func scanQuote(r pgx.Row, extra ...any) (Quote, error) {
	var q Quote
	var apID *uuid.UUID
	var apName, apPos *string
	dest := []any{&q.ID, &q.Reference, &q.Revision, &q.clientID, &q.dealID, &q.contactID, &q.Title, &q.Currency,
		&q.QuoteDate, &q.ValidUntil, &q.Status, &q.Notes, &q.Terms, &q.DeliveryTerms, &q.PaymentTerms, &q.WarrantyTerms, &q.InternalNote,
		&q.Subtotal, &q.DiscountTotal, &q.NetTotal, &q.VATTotal, &q.Total, &q.DiscountPct, &q.MaxLineDiscount, &q.ClientPO,
		&q.Owner.ID, &q.Owner.Name, &q.Owner.Position, &q.Owner.Email, &q.Owner.Phone, &q.CreatedBy.ID, &q.CreatedBy.Name, &q.CreatedBy.Position,
		&q.SubmittedAt, &apID, &apName, &apPos, &q.ApprovedAt, &q.ApprovalNote,
		&q.SentAt, &q.SentTo, &q.DecidedAt, &q.DecisionNote, &q.ClosedReason,
		&q.CreatedAt, &q.UpdatedAt, &q.Version}
	if err := r.Scan(append(dest, extra...)...); err != nil {
		return q, err
	}
	q.ownerID = q.Owner.ID
	q.Client.ID = q.clientID
	if apID != nil {
		q.ApprovedBy = &Person{ID: *apID, Name: derefStr(apName), Position: derefStr(apPos)}
	}
	q.DiscountPct, q.MaxLineDiscount = trimPct(q.DiscountPct), trimPct(q.MaxLineDiscount)
	q.Number = number(q.Reference, q.Revision)
	return q, nil
}

func number(ref string, rev int) string {
	if rev <= 1 {
		return ref
	}
	return ref + "-R" + strconv.Itoa(rev)
}

func getQuote(ctx context.Context, q db.Querier, id uuid.UUID, forUpdate bool) (Quote, error) {
	sql := quoteSelect + ` WHERE q.id = $1`
	if forUpdate {
		sql += ` FOR UPDATE OF q`
	}
	return scanQuote(q.QueryRow(ctx, sql, id))
}

func listQuotes(ctx context.Context, q db.Querier, f ListFilter, clientMatches []uuid.UUID) ([]Quote, int, error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if s := strings.TrimSpace(f.Query); s != "" {
		like := arg("%" + escapeLike(s) + "%")
		cond := `(q.reference ILIKE ` + like + ` OR q.title ILIKE ` + like
		if len(clientMatches) > 0 {
			cond += ` OR q.client_id = ANY(` + arg(clientMatches) + `)`
		}
		where = append(where, cond+`)`)
	}
	switch f.Status {
	case "":
		// Older revisions are kept for the record but not listed.
		where = append(where, `q.status <> 'superseded'`)
	case "open":
		where = append(where, `q.status IN ('draft','pending_approval','ready','sent')`)
	default:
		where = append(where, `q.status = `+arg(f.Status))
	}
	if f.ClientID != nil {
		where = append(where, `q.client_id = `+arg(*f.ClientID))
	}
	if f.DealID != nil {
		where = append(where, `q.deal_id = `+arg(*f.DealID))
	}
	if f.OwnerID != nil {
		where = append(where, `q.owner_id = `+arg(*f.OwnerID))
	}
	sql := strings.Replace(quoteSelect, "q.version\n", "q.version, count(*) OVER ()\n", 1)
	if len(where) > 0 {
		sql += ` WHERE ` + strings.Join(where, " AND ")
	}
	sql += ` ORDER BY q.created_at DESC, q.reference DESC, q.revision DESC LIMIT ` + arg(f.Limit) + ` OFFSET ` + arg(f.Offset)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Quote{}
	total := 0
	for rows.Next() {
		qt, err := scanQuote(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, qt)
	}
	return out, total, rows.Err()
}

func listLines(ctx context.Context, q db.Querier, quoteID uuid.UUID) ([]Line, error) {
	rows, err := q.Query(ctx, `
		SELECT id, position, product_id, sku, description, quantity::text, unit, unit_price::text, discount_pct::text,
		       vat_rate::text, line_net::text, line_vat::text, line_total::text
		FROM quote_lines WHERE quote_id = $1 ORDER BY position`, quoteID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Line, error) {
		var l Line
		err := r.Scan(&l.ID, &l.Position, &l.ProductID, &l.SKU, &l.Description, &l.Quantity, &l.Unit, &l.UnitPrice,
			&l.DiscountPct, &l.VATRate, &l.LineNet, &l.LineVAT, &l.LineTotal)
		l.Quantity, l.DiscountPct, l.VATRate = trimPct(l.Quantity), trimPct(l.DiscountPct), trimPct(l.VATRate)
		return l, err
	})
}

func replaceLines(ctx context.Context, tx pgx.Tx, quoteID uuid.UUID, lines []lineRow) error {
	if _, err := tx.Exec(ctx, `DELETE FROM quote_lines WHERE quote_id = $1`, quoteID); err != nil {
		return err
	}
	for i, l := range lines {
		if _, err := tx.Exec(ctx, `
			INSERT INTO quote_lines (quote_id, position, product_id, sku, description, quantity, unit, unit_price,
			                         discount_pct, vat_rate, line_net, line_vat, line_total)
			VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8::numeric,$9::numeric,$10::numeric,$11::numeric,$12::numeric,$13::numeric)`,
			quoteID, i+1, l.ProductID, l.SKU, l.Description, l.qty.String(), l.Unit, l.price.String(), l.disc.String(),
			l.vat.String(), l.net.String(), l.lineVAT.String(), l.total.String()); err != nil {
			return err
		}
	}
	return nil
}

func revisions(ctx context.Context, q db.Querier, reference string) ([]RevisionRef, error) {
	rows, err := q.Query(ctx, `SELECT id, revision, status, total::text FROM quotes WHERE reference = $1 ORDER BY revision DESC`, reference)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RevisionRef, error) {
		var x RevisionRef
		err := r.Scan(&x.ID, &x.Revision, &x.Status, &x.Total)
		return x, err
	})
}

// header is the part of a quotation saved by create and update.
type header struct {
	ClientID                    uuid.UUID
	DealID, ContactID           *uuid.UUID
	Title, Currency             string
	QuoteDate, ValidUntil       time.Time
	Notes, Terms, InternalNote  *string
	Delivery, Payment, Warranty *string
	totals                      computed
}

func saveTotals(ctx context.Context, tx pgx.Tx, id uuid.UUID, h header) error {
	t := h.totals
	_, err := tx.Exec(ctx, `
		UPDATE quotes SET deal_id=$2, contact_id=$3, title=$4, currency=$5, valid_until=$6, notes=$7, terms=$8, internal_note=$9,
		       delivery_terms=$17, payment_terms=$18, warranty_terms=$19,
		       subtotal=$10::numeric, discount_total=$11::numeric, net_total=$12::numeric, vat_total=$13::numeric,
		       total=$14::numeric, discount_pct=$15::numeric, max_line_discount=$16::numeric
		WHERE id = $1`,
		id, h.DealID, h.ContactID, h.Title, h.Currency, h.ValidUntil, h.Notes, h.Terms, h.InternalNote,
		t.subtotal.String(), t.discount.String(), t.net.String(), t.vat.String(), t.total.String(),
		t.discountPct.String(), t.maxLineDisc.String(), h.Delivery, h.Payment, h.Warranty)
	return err
}
