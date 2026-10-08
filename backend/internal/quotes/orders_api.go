package quotes

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/httpx"
)

// API is what other modules may ask of quotations.
type API interface {
	// ForOrder returns an accepted quotation and its lines, to turn into a
	// customer order. Pass the order's transaction so both read the same data.
	ForOrder(ctx context.Context, q db.Querier, id uuid.UUID) (Accepted, error)
}

// Accepted is an accepted quotation as the orders module sees it.
type Accepted struct {
	ID            uuid.UUID
	Number        string
	ClientID      uuid.UUID
	DealID        *uuid.UUID
	ContactID     *uuid.UUID
	OwnerID       uuid.UUID
	Title         string
	Currency      string
	ClientPO      *string
	DeliveryTerms *string
	PaymentTerms  *string
	Subtotal      string
	DiscountTotal string
	NetTotal      string
	VATTotal      string
	Total         string
	Lines         []Line
}

var errNotAccepted = httpx.NewError(http.StatusConflict, "quote_not_accepted",
	"Only an accepted quotation can become an order. Record the client's acceptance first.")

func (m *Module) API() API { return m.svc }

func (s *Service) ForOrder(ctx context.Context, q db.Querier, id uuid.UUID) (Accepted, error) {
	qt, err := getQuote(ctx, q, id, false)
	if db.IsNoRows(err) {
		return Accepted{}, httpx.ErrNotFound
	}
	if err != nil {
		return Accepted{}, err
	}
	if qt.Status != StatusAccepted {
		return Accepted{}, errNotAccepted.With(map[string]any{"status": qt.Status})
	}
	lines, err := listLines(ctx, q, id)
	if err != nil {
		return Accepted{}, err
	}
	return Accepted{ID: qt.ID, Number: qt.Number, ClientID: qt.clientID, DealID: qt.dealID, ContactID: qt.contactID,
		OwnerID: qt.ownerID, Title: qt.Title, Currency: qt.Currency, ClientPO: qt.ClientPO,
		DeliveryTerms: qt.DeliveryTerms, PaymentTerms: qt.PaymentTerms,
		Subtotal: qt.Subtotal, DiscountTotal: qt.DiscountTotal, NetTotal: qt.NetTotal, VATTotal: qt.VATTotal, Total: qt.Total,
		Lines: lines}, nil
}
