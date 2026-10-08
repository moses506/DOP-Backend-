// Package numbering allocates controlled, sequential document numbers
// (brief section 7).
//
// Numbers are allocated by the server only, inside the same transaction that
// creates the document. The counter row is locked by the UPSERT, so two
// concurrent requests (or two tablets syncing at once) can never receive the
// same number, and a rolled-back transaction releases its number.
package numbering

import (
	"context"
	"fmt"
	"strings"
	"time"

	"dos-bpas/internal/platform/db"
)

type DocType string

// Document types and formats from the brief's illustrative table. Final
// prefixes and reset rules must be confirmed by DOS management before
// production; change them here only.
const (
	Client        DocType = "client"
	Deal          DocType = "deal"
	Quotation     DocType = "quotation"
	CustomerOrder DocType = "customer_order"
	SupplierPO    DocType = "supplier_po"
	DeliveryNote  DocType = "delivery_note"
	JobCard       DocType = "job_card"
	Invoice       DocType = "invoice"
	Tender        DocType = "tender"
	CreditNote    DocType = "credit_note"
	Supplier      DocType = "supplier"
	PriceRequest  DocType = "price_request"
)

type Format struct {
	Prefix    string // e.g. "DOS-Q"
	Yearly    bool   // include the year and restart at 1 each year
	MinDigits int    // zero-padding width
}

var formats = map[DocType]Format{
	Client:        {Prefix: "DOS-CL", Yearly: false, MinDigits: 5}, // not in brief: confirm with DOS
	Deal:          {Prefix: "DOS-D", Yearly: true, MinDigits: 4},
	Quotation:     {Prefix: "DOS-Q", Yearly: true, MinDigits: 4},
	CustomerOrder: {Prefix: "DOS-SO", Yearly: true, MinDigits: 4},
	SupplierPO:    {Prefix: "DOS-PO", Yearly: true, MinDigits: 4},
	DeliveryNote:  {Prefix: "DOS-DN", Yearly: true, MinDigits: 4},
	JobCard:       {Prefix: "DOS-JC", Yearly: true, MinDigits: 4},
	Invoice:       {Prefix: "DOS-INV", Yearly: true, MinDigits: 4},
	Tender:        {Prefix: "DOS-TND", Yearly: true, MinDigits: 4},
	CreditNote:    {Prefix: "DOS-CN", Yearly: true, MinDigits: 4},
	Supplier:      {Prefix: "DOS-SUP", Yearly: false, MinDigits: 4}, // not in brief: confirm with DOS
	PriceRequest:  {Prefix: "DOS-RFQ", Yearly: true, MinDigits: 4},  // not in brief: confirm with DOS
}

type Service struct {
	loc *time.Location
	now func() time.Time
}

func New(loc *time.Location) *Service {
	return &Service{loc: loc, now: time.Now}
}

// Next allocates the next number for docType. It must be called with the
// transaction that inserts the document.
func (s *Service) Next(ctx context.Context, tx db.Querier, t DocType) (string, error) {
	f, ok := formats[t]
	if !ok {
		return "", fmt.Errorf("numbering: no format for %q", t)
	}
	period := 0
	if f.Yearly {
		period = s.now().In(s.loc).Year()
	}
	var n int64
	err := tx.QueryRow(ctx, `
		INSERT INTO number_sequences (doc_type, period, last_value)
		VALUES ($1, $2, 1)
		ON CONFLICT (doc_type, period)
		DO UPDATE SET last_value = number_sequences.last_value + 1, updated_at = now()
		RETURNING last_value`, string(t), period).Scan(&n)
	if err != nil {
		return "", fmt.Errorf("numbering %s: %w", t, err)
	}
	return Render(f, period, n), nil
}

// Render renders a number, e.g. DOS-Q-2026-0001 or DOS-CL-00042.
func Render(f Format, period int, n int64) string {
	parts := []string{f.Prefix}
	if f.Yearly {
		parts = append(parts, fmt.Sprintf("%d", period))
	}
	parts = append(parts, fmt.Sprintf("%0*d", f.MinDigits, n))
	return strings.Join(parts, "-")
}

// Revision renders a quotation revision suffix, e.g. DOS-Q-2026-0001-R1.
func Revision(base string, rev int) string {
	return fmt.Sprintf("%s-R%d", base, rev)
}
