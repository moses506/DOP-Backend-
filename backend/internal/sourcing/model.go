package sourcing

import (
	"time"

	"github.com/google/uuid"
)

// ---- suppliers ----

// Regions DOS buys through. A distributor often has one account per region.
var regions = map[string]string{
	"ZA":    "South Africa",
	"ZM":    "Zambia",
	"GB":    "United Kingdom",
	"OTHER": "Other",
}

type Person struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Position string    `json:"position"`
}

type Supplier struct {
	ID             uuid.UUID `json:"id"`
	Reference      string    `json:"reference"`
	Name           string    `json:"name"`
	Website        *string   `json:"website"`
	Brands         *string   `json:"brands"`
	Notes          *string   `json:"notes"`
	Status         string    `json:"status"`
	InactiveReason *string   `json:"inactive_reason"`
	Accounts       []Account `json:"accounts"`
	// How often they've been asked and how often they answered, for choosing whom to ask.
	Asked     int        `json:"asked"`
	Replied   int        `json:"replied"`
	LastAsked *time.Time `json:"last_asked"`
	CreatedBy Person     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Version   int        `json:"version"`
}

type Account struct {
	ID            uuid.UUID `json:"id"`
	Region        string    `json:"region"`
	RegionName    string    `json:"region_name"`
	AccountNumber *string   `json:"account_number"`
	Terms         *string   `json:"terms"` // credit or cod; null when not known
	CreditLimit   *string   `json:"credit_limit"`
	Currency      string    `json:"currency"`
	ContactName   *string   `json:"contact_name"`
	ContactEmail  string    `json:"contact_email"`
	OtherEmails   []string  `json:"other_emails"`
	Note          *string   `json:"note"`
	Active        bool      `json:"active"`
}

type SupplierInput struct {
	Version  int            `json:"version"` // for updates
	Name     string         `json:"name"`
	Website  *string        `json:"website"`
	Brands   *string        `json:"brands"`
	Notes    *string        `json:"notes"`
	Accounts []AccountInput `json:"accounts"`
}

type AccountInput struct {
	ID            *uuid.UUID `json:"id"` // existing account; leave out for a new one
	Region        string     `json:"region"`
	AccountNumber *string    `json:"account_number"`
	Terms         *string    `json:"terms"`
	CreditLimit   *string    `json:"credit_limit"`
	Currency      string     `json:"currency"`
	ContactName   *string    `json:"contact_name"`
	ContactEmail  string     `json:"contact_email"`
	OtherEmails   []string   `json:"other_emails"`
	Note          *string    `json:"note"`
}

type SupplierFilter struct {
	Query  string
	Region string
	Terms  string
	Status string
	Limit  int
	Offset int
}

type Rate struct {
	Currency  string    `json:"currency"`
	RateToZMW string    `json:"rate_to_zmw"`
	UpdatedBy string    `json:"updated_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ---- price requests ----

const (
	StatusDraft     = "draft"
	StatusSent      = "sent"
	StatusDecided   = "decided"
	StatusCancelled = "cancelled"
)

type Ref struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	Name      string    `json:"name"`
}

type Request struct {
	ID           uuid.UUID  `json:"id"`
	Reference    string     `json:"reference"`
	Title        string     `json:"title"`
	NeededBy     *string    `json:"needed_by"`
	Client       *Ref       `json:"client"`
	Deal         *Ref       `json:"deal"`
	Notes        *string    `json:"notes"`
	InternalNote *string    `json:"internal_note"`
	Status       string     `json:"status"`
	Owner        Person     `json:"owner"`
	CreatedBy    Person     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	SentAt       *time.Time `json:"sent_at"`
	DecidedAt    *time.Time `json:"decided_at"`
	CancelledAt  *time.Time `json:"cancelled_at"`
	ClosedReason *string    `json:"closed_reason"`
	Version      int        `json:"version"`

	// Counts, on lists and the detail.
	ItemCount   int `json:"item_count"`
	Invited     int `json:"invited"`
	Replied     int `json:"replied"`
	Chosen      int `json:"chosen"`
	ItemsToPick int `json:"items_to_pick"`

	Items   []Item   `json:"items,omitempty"`
	Invites []Invite `json:"invites,omitempty"`
	// Rates used to compare in ZMW (only the currencies quoted).
	Rates []Rate `json:"rates,omitempty"`

	clientID, dealID *uuid.UUID
	ownerID          uuid.UUID
}

type Item struct {
	ID             uuid.UUID  `json:"id"`
	Position       int        `json:"position"`
	Description    string     `json:"description"`
	PartNo         *string    `json:"part_no"`
	Brand          *string    `json:"brand"`
	Quantity       string     `json:"quantity"`
	Unit           string     `json:"unit"`
	ChosenInviteID *uuid.UUID `json:"chosen_invite_id"`
	// Filled in on the detail: the lowest price in ZMW among the replies, when rates allow comparing.
	BestInviteID *uuid.UUID `json:"best_invite_id"`
}

type Invite struct {
	ID         uuid.UUID  `json:"id"`
	Supplier   Ref        `json:"supplier"`
	AccountID  uuid.UUID  `json:"account_id"`
	Region     string     `json:"region"`
	Terms      *string    `json:"terms"`
	SentTo     string     `json:"sent_to"`
	Emailed    bool       `json:"emailed"`
	SentAt     time.Time  `json:"sent_at"`
	SentBy     string     `json:"sent_by"`
	Status     string     `json:"status"` // waiting, replied, declined
	RepliedAt  *time.Time `json:"replied_at"`
	Currency   *string    `json:"currency"`
	LeadTime   *string    `json:"lead_time"`
	ValidUntil *string    `json:"valid_until"`
	ReplyNote  *string    `json:"reply_note"`
	Prices     []Price    `json:"prices"`
	// Totals across the items they priced, in their currency and in ZMW (when a rate is known).
	Total     *string `json:"total"`
	TotalZMW  *string `json:"total_zmw"`
	Complete  bool    `json:"complete"` // priced every item
	Cheapest  bool    `json:"cheapest"` // lowest complete total in ZMW
	DaysQuiet int     `json:"days_quiet"`
}

type Price struct {
	ItemID       uuid.UUID `json:"item_id"`
	UnitCost     *string   `json:"unit_cost"` // nil: can't supply
	Availability *string   `json:"availability"`
	LineTotal    *string   `json:"line_total"`
	LineTotalZMW *string   `json:"line_total_zmw"`
}

type ItemInput struct {
	Description string  `json:"description"`
	PartNo      *string `json:"part_no"`
	Brand       *string `json:"brand"`
	Quantity    string  `json:"quantity"`
	Unit        string  `json:"unit"`
}

type RequestInput struct {
	Version      int         `json:"version"`
	Title        string      `json:"title"`
	NeededBy     *string     `json:"needed_by"`
	ClientID     *uuid.UUID  `json:"client_id"`
	DealID       *uuid.UUID  `json:"deal_id"`
	Notes        *string     `json:"notes"`
	InternalNote *string     `json:"internal_note"`
	Items        []ItemInput `json:"items"`
}

// SendInput asks suppliers for prices: by email from the system, or recorded
// as asked another way (phone, WhatsApp, their portal).
type SendInput struct {
	Version    int         `json:"version"`
	AccountIDs []uuid.UUID `json:"account_ids"`
	Email      bool        `json:"email"`
	CopyOthers bool        `json:"copy_others"` // also send to the account's other addresses
	CopyMe     bool        `json:"copy_me"`
	Message    string      `json:"message"`
	Via        string      `json:"via"` // when not emailed
}

type SendResult struct {
	Request Request      `json:"request"`
	Sent    []string     `json:"sent"`   // supplier names
	Failed  []SendFailed `json:"failed"` // emails that didn't go
	Emailed bool         `json:"emailed"`
	Note    string       `json:"note,omitempty"`
}

type SendFailed struct {
	Supplier string `json:"supplier"`
	Reason   string `json:"reason"`
}

type ReplyInput struct {
	Version    int     `json:"version"`
	Declined   bool    `json:"declined"`
	Currency   string  `json:"currency"`
	LeadTime   *string `json:"lead_time"`
	ValidUntil *string `json:"valid_until"`
	Note       *string `json:"note"`
	Prices     []struct {
		ItemID       uuid.UUID `json:"item_id"`
		UnitCost     *string   `json:"unit_cost"` // empty or null: can't supply
		Availability *string   `json:"availability"`
	} `json:"prices"`
}

type DecideInput struct {
	Version int `json:"version"`
	Choices []struct {
		ItemID   uuid.UUID  `json:"item_id"`
		InviteID *uuid.UUID `json:"invite_id"`
	} `json:"choices"`
	Note string `json:"note"`
}

type RequestFilter struct {
	Query    string
	Status   string // a status, or "open" (draft or sent)
	OwnerID  *uuid.UUID
	ClientID *uuid.UUID
	Limit    int
	Offset   int
}
