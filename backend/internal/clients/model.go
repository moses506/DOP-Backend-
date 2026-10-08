package clients

import (
	"net/mail"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"dos-bpas/internal/platform/httpx"
)

type Status string

const (
	StatusActive   Status = "active"
	StatusInactive Status = "inactive"
)

type Client struct {
	ID                 uuid.UUID  `json:"id"`
	Reference          string     `json:"reference"`
	LegalName          string     `json:"legal_name"`
	TradingName        *string    `json:"trading_name"`
	RegistrationNo     *string    `json:"registration_no"`
	TPIN               *string    `json:"tpin"`
	Sector             *string    `json:"sector"`
	BillingAddress     *string    `json:"billing_address"`
	DefaultCurrency    string     `json:"default_currency"`
	CreditTermsDays    int        `json:"credit_terms_days"`
	CreditLimit        *string    `json:"credit_limit"` // decimal as string, e.g. "150000.00"
	Status             Status     `json:"status"`
	Notes              *string    `json:"notes"`
	Owner              Person     `json:"owner"`
	CreatedBy          Person     `json:"created_by"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	DeactivatedAt      *time.Time `json:"deactivated_at"`
	DeactivationReason *string    `json:"deactivation_reason"`
	Version            int        `json:"version"`

	// From the contact log (contactlog.go).
	LastContactAt  *time.Time `json:"last_contact_at"`
	ContactHealth  string     `json:"contact_health"`    // good, fading, cold or never
	NextFollowUpOn *string    `json:"next_follow_up_on"` // earliest open follow-up, YYYY-MM-DD
	OpenFollowUps  int        `json:"open_follow_ups"`

	Contacts []Contact `json:"contacts,omitempty"`
	Sites    []Site    `json:"sites,omitempty"`
}

// Person is the "responsible staff member + position" shown on every record.
type Person struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Position string    `json:"position"`
}

type Contact struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Position  *string   `json:"position"`
	Email     *string   `json:"email"`
	Phone     *string   `json:"phone"`
	IsPrimary bool      `json:"is_primary"`
	Active    bool      `json:"active"`
}

type Site struct {
	ID           uuid.UUID `json:"id"`
	Name         string    `json:"name"`
	Address      string    `json:"address"`
	ContactName  *string   `json:"contact_name"`
	ContactPhone *string   `json:"contact_phone"`
	Notes        *string   `json:"notes"`
	Active       bool      `json:"active"`
}

// PossibleDuplicate is returned when a new client looks like an existing one.
type PossibleDuplicate struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	LegalName string    `json:"legal_name"`
	Status    Status    `json:"status"`
	MatchedOn []string  `json:"matched_on"` // "name", "tpin", "registration_no"
}

// ---- inputs ----

type ContactInput struct {
	Name      string  `json:"name"`
	Position  *string `json:"position"`
	Email     *string `json:"email"`
	Phone     *string `json:"phone"`
	IsPrimary bool    `json:"is_primary"`
}

type SiteInput struct {
	Name         string  `json:"name"`
	Address      string  `json:"address"`
	ContactName  *string `json:"contact_name"`
	ContactPhone *string `json:"contact_phone"`
	Notes        *string `json:"notes"`
}

type CreateInput struct {
	LegalName       string         `json:"legal_name"`
	TradingName     *string        `json:"trading_name"`
	RegistrationNo  *string        `json:"registration_no"`
	TPIN            *string        `json:"tpin"`
	Sector          *string        `json:"sector"`
	BillingAddress  *string        `json:"billing_address"`
	DefaultCurrency string         `json:"default_currency"`
	CreditTermsDays *int           `json:"credit_terms_days"`
	CreditLimit     *string        `json:"credit_limit"`
	Notes           *string        `json:"notes"`
	OwnerID         *uuid.UUID     `json:"owner_id"` // defaults to the creator
	Contacts        []ContactInput `json:"contacts"`
	Sites           []SiteInput    `json:"sites"`

	// Set only to register despite a possible-duplicate warning. Requires the
	// clients:override_duplicate permission and a reason.
	AllowDuplicate  bool   `json:"allow_duplicate"`
	DuplicateReason string `json:"duplicate_reason"`
}

// UpdateInput uses pointers so only supplied fields change (PATCH semantics).
// Version must match the stored version.
type UpdateInput struct {
	Version         int        `json:"version"`
	LegalName       *string    `json:"legal_name"`
	TradingName     *string    `json:"trading_name"`
	RegistrationNo  *string    `json:"registration_no"`
	TPIN            *string    `json:"tpin"`
	Sector          *string    `json:"sector"`
	BillingAddress  *string    `json:"billing_address"`
	DefaultCurrency *string    `json:"default_currency"`
	CreditTermsDays *int       `json:"credit_terms_days"`
	CreditLimit     *string    `json:"credit_limit"` // "" clears the limit
	Notes           *string    `json:"notes"`
	OwnerID         *uuid.UUID `json:"owner_id"`
	Reason          string     `json:"reason"` // required when changing owner (handover)
}

type ListFilter struct {
	Query   string
	Status  Status
	OwnerID *uuid.UUID
	// Attention narrows the list to clients needing action:
	// "followups" – with a follow-up of Me's due today or earlier;
	// "cold" – active clients not contacted in ColdAfterDays days (or never).
	Attention string
	Me        uuid.UUID
	TZ        string // business timezone, for "today"
	Page      httpx.Page
}

// ---- validation helpers ----

var (
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
	moneyRe    = regexp.MustCompile(`^\d{1,12}(\.\d{1,2})?$`)
	tpinRe     = regexp.MustCompile(`^\d{10}$`)
)

// clean trims a nullable string and turns empty into nil.
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

func validateCommon(fe httpx.FieldErrors, legalName *string, currency *string, terms *int, limit *string, tpin *string) {
	if legalName != nil && strings.TrimSpace(*legalName) == "" {
		fe.Add("legal_name", "Legal name is required.")
	}
	if currency != nil && !currencyRe.MatchString(*currency) {
		fe.Add("default_currency", "Use a 3-letter currency code, e.g. ZMW or USD.")
	}
	if terms != nil && (*terms < 0 || *terms > 365) {
		fe.Add("credit_terms_days", "Credit terms must be between 0 and 365 days.")
	}
	if limit != nil && *limit != "" && !moneyRe.MatchString(*limit) {
		fe.Add("credit_limit", "Enter an amount like 150000 or 150000.00.")
	}
	if tpin != nil && !tpinRe.MatchString(*tpin) {
		fe.Add("tpin", "TPIN must be 10 digits.")
	}
}

func validateContact(fe httpx.FieldErrors, prefix string, c ContactInput) {
	if strings.TrimSpace(c.Name) == "" {
		fe.Add(prefix+"name", "Contact name is required.")
	}
	if c.Email != nil && *c.Email != "" {
		if _, err := mail.ParseAddress(*c.Email); err != nil {
			fe.Add(prefix+"email", "Enter a valid email address.")
		}
	}
}

func validateSite(fe httpx.FieldErrors, prefix string, s SiteInput) {
	if strings.TrimSpace(s.Name) == "" {
		fe.Add(prefix+"name", "Site name is required.")
	}
	if strings.TrimSpace(s.Address) == "" {
		fe.Add(prefix+"address", "Site address is required.")
	}
}
