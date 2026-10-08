package deals

import (
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"dos-bpas/internal/platform/httpx"
)

// Stage is where a deal sits in the sales pipeline.
type Stage string

const (
	StageLead        Stage = "lead"        // an enquiry or opportunity, not yet qualified
	StageQualified   Stage = "qualified"   // real need, budget and decision-maker confirmed
	StageProposal    Stage = "proposal"    // quotation or proposal sent
	StageNegotiation Stage = "negotiation" // terms being agreed
	StageWon         Stage = "won"
	StageLost        Stage = "lost"
)

// Stages in pipeline order.
var Stages = []Stage{StageLead, StageQualified, StageProposal, StageNegotiation, StageWon, StageLost}

// OpenStages are the stages of a deal still being worked.
var OpenStages = []Stage{StageLead, StageQualified, StageProposal, StageNegotiation}

func (s Stage) Valid() bool {
	for _, x := range Stages {
		if s == x {
			return true
		}
	}
	return false
}

func (s Stage) Closed() bool { return s == StageWon || s == StageLost }

// DefaultProbability is the chance of winning assumed for each stage. It is
// applied whenever the stage changes, unless a probability is given.
func (s Stage) DefaultProbability() int {
	switch s {
	case StageLead:
		return 10
	case StageQualified:
		return 25
	case StageProposal:
		return 50
	case StageNegotiation:
		return 75
	case StageWon:
		return 100
	}
	return 0
}

// NoteKinds are the kinds of activity a sales person can log on a deal.
var NoteKinds = []string{"note", "call", "meeting", "visit", "email"}

// Sources are suggestions only; any text is accepted.
var Sources = []string{"Existing client", "Referral", "Tender", "Walk-in", "Website", "Cold call", "Event"}

type Deal struct {
	ID             uuid.UUID  `json:"id"`
	Reference      string     `json:"reference"`
	Client         ClientRef  `json:"client"`
	Title          string     `json:"title"`
	Description    *string    `json:"description"`
	Stage          Stage      `json:"stage"`
	Value          *string    `json:"value"` // decimal as string, e.g. "291338.00"
	Currency       string     `json:"currency"`
	Probability    int        `json:"probability"`
	ExpectedClose  *Date      `json:"expected_close"`
	Source         *string    `json:"source"`
	NextStep       *string    `json:"next_step"`
	NextStepDate   *Date      `json:"next_step_date"`
	LostReason     *string    `json:"lost_reason"`
	ClosedAt       *time.Time `json:"closed_at"`
	StageChangedAt time.Time  `json:"stage_changed_at"`
	Owner          Person     `json:"owner"`
	CreatedBy      Person     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Version        int        `json:"version"`

	clientID uuid.UUID
}

// ClientRef is the client a deal belongs to, as supplied by the clients module.
type ClientRef struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	LegalName string    `json:"legal_name"`
	Sector    *string   `json:"sector"`
	Status    string    `json:"status"`
}

// Person is the responsible staff member and their position.
type Person struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Position string    `json:"position"`
}

type Note struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind"`
	Body      string    `json:"body"`
	CreatedBy Person    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// Date is a calendar date (no time of day), sent as "2026-10-31".
type Date string

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func (d Date) Valid() bool {
	if !dateRe.MatchString(string(d)) {
		return false
	}
	_, err := time.Parse("2006-01-02", string(d))
	return err == nil
}

type CreateInput struct {
	ClientID      uuid.UUID  `json:"client_id"`
	Title         string     `json:"title"`
	Description   *string    `json:"description"`
	Stage         Stage      `json:"stage"`
	Value         *string    `json:"value"`
	Currency      string     `json:"currency"`
	Probability   *int       `json:"probability"`
	ExpectedClose *Date      `json:"expected_close"`
	Source        *string    `json:"source"`
	NextStep      *string    `json:"next_step"`
	NextStepDate  *Date      `json:"next_step_date"`
	OwnerID       *uuid.UUID `json:"owner_id"`
}

// UpdateInput changes the deal's details. Pointers left nil are unchanged;
// an empty string clears an optional field. The stage moves through
// MoveInput instead, so every stage change is deliberate and recorded.
type UpdateInput struct {
	Version       int        `json:"version"`
	Title         *string    `json:"title"`
	Description   *string    `json:"description"`
	Value         *string    `json:"value"`
	Currency      *string    `json:"currency"`
	Probability   *int       `json:"probability"`
	ExpectedClose *string    `json:"expected_close"`
	Source        *string    `json:"source"`
	NextStep      *string    `json:"next_step"`
	NextStepDate  *string    `json:"next_step_date"`
	OwnerID       *uuid.UUID `json:"owner_id"`
	Reason        string     `json:"reason"` // required when handing the deal to someone else
}

type MoveInput struct {
	Version     int    `json:"version"`
	Stage       Stage  `json:"stage"`
	Probability *int   `json:"probability"`
	Reason      string `json:"reason"` // required when marking lost or reopening
}

type NoteInput struct {
	Kind string `json:"kind"`
	Body string `json:"body"`
}

type ListFilter struct {
	Query    string
	Stage    string // a stage, or "open" / "closed"
	OwnerID  *uuid.UUID
	ClientID *uuid.UUID
	Page     httpx.Page
}

// ---- validation helpers ----

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
var amountRe = regexp.MustCompile(`^\d{1,12}(\.\d{1,2})?$`)

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

// cleanAmount accepts "291,338.00" or "291338" and returns "291338.00"-style text.
func cleanAmount(s *string) *string {
	s = clean(s)
	if s == nil {
		return nil
	}
	t := strings.ReplaceAll(strings.ReplaceAll(*s, ",", ""), " ", "")
	return &t
}

func cleanDate(s *string) *Date {
	s = clean(s)
	if s == nil {
		return nil
	}
	d := Date(*s)
	return &d
}

func validate(fe httpx.FieldErrors, title string, value *string, currency string, prob int, expected, nextDate *Date) {
	if strings.TrimSpace(title) == "" {
		fe.Add("title", "Give the deal a short title, e.g. “50 laptops for head office”.")
	} else if len(title) > 200 {
		fe.Add("title", "Keep the title under 200 characters.")
	}
	if value != nil && !amountRe.MatchString(*value) {
		fe.Add("value", "Enter an amount like 250000 or 250000.50.")
	}
	if !currencyRe.MatchString(currency) {
		fe.Add("currency", "Use a 3-letter currency code, e.g. ZMW or USD.")
	}
	if prob < 0 || prob > 100 {
		fe.Add("probability", "Probability must be between 0 and 100.")
	}
	if expected != nil && !expected.Valid() {
		fe.Add("expected_close", "Enter a valid date.")
	}
	if nextDate != nil && !nextDate.Valid() {
		fe.Add("next_step_date", "Enter a valid date.")
	}
}
