package orders

import (
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusOpen          Status = "open"           // confirmed, nothing done yet
	StatusInProgress    Status = "in_progress"    // goods being sourced or prepared
	StatusReady         Status = "ready"          // everything ready to deliver
	StatusPartDelivered Status = "part_delivered" // some delivered, some still to go
	StatusDelivered     Status = "delivered"      // all delivered: ready to invoice
	StatusInvoiced      Status = "invoiced"       // invoiced: closed
	StatusOnHold        Status = "on_hold"
	StatusCancelled     Status = "cancelled"
)

// Live orders still have work to do.
func (s Status) Live() bool {
	return s == StatusOpen || s == StatusInProgress || s == StatusReady || s == StatusPartDelivered || s == StatusOnHold
}

// Closed orders can't change any more.
func (s Status) Closed() bool { return s == StatusInvoiced || s == StatusCancelled }

type Source string

const (
	SourceStock    Source = "stock"
	SourceSupplier Source = "supplier"
	SourceService  Source = "service"
)

func (s Source) Valid() bool { return s == SourceStock || s == SourceSupplier || s == SourceService }

type Person struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Position string    `json:"position"`
}

type ClientRef struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	LegalName string    `json:"legal_name"`
}

type QuoteRef struct {
	ID     uuid.UUID `json:"id"`
	Number string    `json:"number"`
}

type DealRef struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	Title     string    `json:"title"`
}

type SiteRef struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	Address      string    `json:"address"`
	ContactName  *string   `json:"contact_name"`
	ContactPhone *string   `json:"contact_phone"`
}

type ContactRef struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Phone *string   `json:"phone"`
	Email *string   `json:"email"`
}

type Order struct {
	ID        uuid.UUID   `json:"id"`
	Reference string      `json:"reference"`
	Client    ClientRef   `json:"client"`
	Quote     *QuoteRef   `json:"quote"`
	Deal      *DealRef    `json:"deal"`
	Contact   *ContactRef `json:"contact"`
	Site      *SiteRef    `json:"site"`
	Title     string      `json:"title"`
	Currency  string      `json:"currency"`

	ClientPO     *string `json:"client_po"`
	ClientPODate *string `json:"client_po_date"` // YYYY-MM-DD
	OrderDate    string  `json:"order_date"`
	DeliveryDue  *string `json:"delivery_due"`

	DeliveryInstructions *string `json:"delivery_instructions"`
	PaymentTerms         *string `json:"payment_terms"`
	InternalNote         *string `json:"internal_note"`

	Status     Status     `json:"status"`
	HoldReason *string    `json:"hold_reason"`
	HeldAt     *time.Time `json:"held_at"`

	Subtotal      string `json:"subtotal"`
	DiscountTotal string `json:"discount_total"`
	NetTotal      string `json:"net_total"`
	VATTotal      string `json:"vat_total"`
	Total         string `json:"total"`

	// Progress across all lines, 0–100.
	ReadyPct     int  `json:"ready_pct"`
	DeliveredPct int  `json:"delivered_pct"`
	Late         bool `json:"late"` // the promised date has passed and something is still to deliver

	DeliveredAt  *time.Time `json:"delivered_at"`
	InvoiceRef   *string    `json:"invoice_ref"`
	InvoicedOn   *string    `json:"invoiced_on"`
	ClosedReason *string    `json:"closed_reason"`
	CancelledAt  *time.Time `json:"cancelled_at"`

	Owner     Person    `json:"owner"`
	CreatedBy Person    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Version   int       `json:"version"`

	Lines      []Line     `json:"lines,omitempty"`
	Deliveries []Delivery `json:"deliveries,omitempty"`

	clientID, ownerID          uuid.UUID
	quoteID, dealID, contactID *uuid.UUID
	siteID                     *uuid.UUID
}

type Line struct {
	ID           uuid.UUID  `json:"id"`
	Position     int        `json:"position"`
	ProductID    *uuid.UUID `json:"product_id"`
	SKU          *string    `json:"sku"`
	Description  string     `json:"description"`
	Quantity     string     `json:"quantity"`
	Unit         string     `json:"unit"`
	UnitPrice    string     `json:"unit_price"`
	DiscountPct  string     `json:"discount_pct"`
	VATRate      string     `json:"vat_rate"`
	LineNet      string     `json:"line_net"`
	LineVAT      string     `json:"line_vat"`
	LineTotal    string     `json:"line_total"`
	Source       Source     `json:"source"`
	SupplierNote *string    `json:"supplier_note"`
	QtyReady     string     `json:"qty_ready"`
	QtyDelivered string     `json:"qty_delivered"`
}

type Delivery struct {
	ID          uuid.UUID      `json:"id"`
	DeliveredOn string         `json:"delivered_on"`
	Reference   *string        `json:"reference"`
	ReceivedBy  *string        `json:"received_by"`
	Note        *string        `json:"note"`
	Lines       []DeliveryLine `json:"lines"`
	CreatedBy   Person         `json:"created_by"`
	CreatedAt   time.Time      `json:"created_at"`
}

type DeliveryLine struct {
	LineID      uuid.UUID `json:"line_id"`
	Description string    `json:"description"`
	Quantity    string    `json:"quantity"`
	Unit        string    `json:"unit"`
}

// ---- inputs ----

// CreateInput turns an accepted quotation into an order.
type CreateInput struct {
	QuoteID              uuid.UUID    `json:"quote_id"`
	ClientPO             *string      `json:"client_po"`
	ClientPODate         *string      `json:"client_po_date"`
	DeliveryDue          *string      `json:"delivery_due"`
	SiteID               *uuid.UUID   `json:"site_id"`
	DeliveryInstructions *string      `json:"delivery_instructions"`
	InternalNote         *string      `json:"internal_note"`
	Sources              []LineSource `json:"lines"` // where each quotation line comes from, by position
}

type LineSource struct {
	Position     int     `json:"position"`
	Source       Source  `json:"source"`
	SupplierNote *string `json:"supplier_note"`
}

// UpdateInput changes the order's details. Fields left out keep their value.
type UpdateInput struct {
	Version              int     `json:"version"`
	ClientPO             *string `json:"client_po"`
	ClientPODate         *string `json:"client_po_date"`
	DeliveryDue          *string `json:"delivery_due"`
	SiteID               *string `json:"site_id"` // "" clears it
	DeliveryInstructions *string `json:"delivery_instructions"`
	InternalNote         *string `json:"internal_note"`
	Reason               string  `json:"reason"` // needed when the promised date moves
}

// ProgressInput updates where each line has got to.
type ProgressInput struct {
	Version int            `json:"version"`
	Lines   []LineProgress `json:"lines"`
}

type LineProgress struct {
	ID           uuid.UUID `json:"id"`
	Source       *Source   `json:"source"`
	SupplierNote *string   `json:"supplier_note"`
	QtyReady     *string   `json:"qty_ready"`
}

// DeliveryInput records goods handed over to the client.
type DeliveryInput struct {
	Version     int    `json:"version"`
	DeliveredOn string `json:"delivered_on"`
	Reference   string `json:"reference"`
	ReceivedBy  string `json:"received_by"`
	Note        string `json:"note"`
	Lines       []struct {
		ID       uuid.UUID `json:"id"`
		Quantity string    `json:"quantity"`
	} `json:"lines"`
}

type ListFilter struct {
	Query    string
	Status   string // a status, "live" or "late"
	ClientID *uuid.UUID
	QuoteID  *uuid.UUID
	DealID   *uuid.UUID
	OwnerID  *uuid.UUID
	Limit    int
	Offset   int
}

type Summary struct {
	Open           int    `json:"open"`  // open + in progress
	Ready          int    `json:"ready"` // ready to deliver
	PartDelivered  int    `json:"part_delivered"`
	Late           int    `json:"late"`
	DueThisWeek    int    `json:"due_this_week"`
	ToInvoice      int    `json:"to_invoice"` // delivered, not yet invoiced
	OnHold         int    `json:"on_hold"`
	LiveValue      string `json:"live_value"` // total of live orders in ZMW
	LiveValueUSD   string `json:"live_value_usd"`
	ToInvoiceValue string `json:"to_invoice_value"`
}
