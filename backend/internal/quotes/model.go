package quotes

import (
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusDraft           Status = "draft"
	StatusPendingApproval Status = "pending_approval" // discount above the limit: waiting for a manager
	StatusReady           Status = "ready"            // approved or within limits; can be sent
	StatusSent            Status = "sent"
	StatusAccepted        Status = "accepted"
	StatusDeclined        Status = "declined"
	StatusExpired         Status = "expired"
	StatusSuperseded      Status = "superseded" // replaced by a newer revision
	StatusCancelled       Status = "cancelled"
)

// Open quotations can still change or be decided on.
func (s Status) Open() bool {
	return s == StatusDraft || s == StatusPendingApproval || s == StatusReady || s == StatusSent
}

// Editable quotations can have their lines and details changed.
func (s Status) Editable() bool { return s == StatusDraft || s == StatusPendingApproval }

type Person struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Position string    `json:"position"`
	Email    string    `json:"email,omitempty"`
	Phone    string    `json:"phone,omitempty"`
}

type ClientRef struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	LegalName string    `json:"legal_name"`
	Status    string    `json:"status"`
}

type DealRef struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	Title     string    `json:"title"`
	Stage     string    `json:"stage"`
}

type ContactRef struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Position *string   `json:"position"`
	Email    *string   `json:"email"`
	Phone    *string   `json:"phone"`
}

type Quote struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	Revision  int       `json:"revision"`
	// Number is the reference as printed: DOS-Q-2026-0001, or DOS-Q-2026-0001-R2 for revision 2.
	Number   string      `json:"number"`
	Client   ClientRef   `json:"client"`
	Deal     *DealRef    `json:"deal"`
	Contact  *ContactRef `json:"contact"`
	Title    string      `json:"title"`
	Currency string      `json:"currency"`
	// Dates are YYYY-MM-DD.
	QuoteDate  string `json:"quote_date"`
	ValidUntil string `json:"valid_until"`
	Status     Status `json:"status"`

	Notes         *string `json:"notes"`
	Terms         *string `json:"terms"` // other terms, after the standard ones
	DeliveryTerms *string `json:"delivery_terms"`
	PaymentTerms  *string `json:"payment_terms"`
	WarrantyTerms *string `json:"warranty_terms"`
	InternalNote  *string `json:"internal_note"`

	// Amounts are decimals as strings, e.g. "12500.00".
	Subtotal        string `json:"subtotal"`
	DiscountTotal   string `json:"discount_total"`
	NetTotal        string `json:"net_total"`
	VATTotal        string `json:"vat_total"`
	Total           string `json:"total"`
	DiscountPct     string `json:"discount_pct"`
	MaxLineDiscount string `json:"max_line_discount"`
	// NeedsApproval is true when a discount is above the approval limit.
	NeedsApproval bool `json:"needs_approval"`

	ClientPO     *string    `json:"client_po"`
	Owner        Person     `json:"owner"`
	CreatedBy    Person     `json:"created_by"`
	SubmittedAt  *time.Time `json:"submitted_at"`
	ApprovedBy   *Person    `json:"approved_by"`
	ApprovedAt   *time.Time `json:"approved_at"`
	ApprovalNote *string    `json:"approval_note"`
	SentAt       *time.Time `json:"sent_at"`
	SentTo       *string    `json:"sent_to"`
	DecidedAt    *time.Time `json:"decided_at"`
	DecisionNote *string    `json:"decision_note"`
	ClosedReason *string    `json:"closed_reason"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	Version      int        `json:"version"`

	Lines []Line `json:"lines,omitempty"`
	// Revisions lists every revision of this quotation, newest first (on the detail only).
	Revisions []RevisionRef `json:"revisions,omitempty"`

	clientID, ownerID uuid.UUID
	dealID, contactID *uuid.UUID
}

type RevisionRef struct {
	ID       uuid.UUID `json:"id"`
	Revision int       `json:"revision"`
	Status   Status    `json:"status"`
	Total    string    `json:"total"`
}

type Line struct {
	ID          uuid.UUID  `json:"id"`
	Position    int        `json:"position"`
	ProductID   *uuid.UUID `json:"product_id"`
	SKU         *string    `json:"sku"`
	Description string     `json:"description"`
	Quantity    string     `json:"quantity"`
	Unit        string     `json:"unit"`
	UnitPrice   string     `json:"unit_price"`
	DiscountPct string     `json:"discount_pct"`
	VATRate     string     `json:"vat_rate"`
	LineNet     string     `json:"line_net"`
	LineVAT     string     `json:"line_vat"`
	LineTotal   string     `json:"line_total"`
}

// ---- inputs ----

type LineInput struct {
	ProductID   *uuid.UUID `json:"product_id"`
	SKU         *string    `json:"sku"`
	Description string     `json:"description"`
	Quantity    string     `json:"quantity"`
	Unit        string     `json:"unit"`
	UnitPrice   string     `json:"unit_price"`
	DiscountPct string     `json:"discount_pct"`
	VATRate     string     `json:"vat_rate"`
}

// QuoteInput creates a quotation, or replaces a draft's details and lines.
type QuoteInput struct {
	Version       int         `json:"version"` // for updates
	ClientID      uuid.UUID   `json:"client_id"`
	DealID        *uuid.UUID  `json:"deal_id"`
	ContactID     *uuid.UUID  `json:"contact_id"`
	Title         string      `json:"title"`
	Currency      string      `json:"currency"`
	ValidUntil    string      `json:"valid_until"`
	Notes         *string     `json:"notes"`
	Terms         *string     `json:"terms"`
	DeliveryTerms *string     `json:"delivery_terms"`
	PaymentTerms  *string     `json:"payment_terms"`
	WarrantyTerms *string     `json:"warranty_terms"`
	InternalNote  *string     `json:"internal_note"`
	Lines         []LineInput `json:"lines"`
}

type ListFilter struct {
	Query    string
	Status   string // a status, or "open"
	ClientID *uuid.UUID
	DealID   *uuid.UUID
	OwnerID  *uuid.UUID
	Limit    int
	Offset   int
}
