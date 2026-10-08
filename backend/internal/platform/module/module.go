// Package module defines the contract every business module implements.
//
// Rules (see README "Adding a module"):
//  1. A module owns its tables and migrations. Other modules refer to its
//     records by ID only and never query its tables directly.
//  2. Cross-module calls go through the other module's exported Service
//     interface, or through events.
//  3. Shared concerns (auth, audit, numbering, events, PDF, email) live in
//     internal/platform and are passed in through Deps.
package module

import (
	"context"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"

	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/config"
	"dos-bpas/internal/platform/events"
	"dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/notify"
	"dos-bpas/internal/platform/numbering"
)

type Deps struct {
	DB      *pgxpool.Pool
	Tokens  *auth.Tokens
	Numbers *numbering.Service
	Events  *events.Bus
	Log     *slog.Logger
	Now     func() time.Time
	Mail    *mail.Service
	AppURL  string // public web address, for links in emails
	// Location is the business timezone (APP_TIMEZONE), for "this month" style figures.
	Location *time.Location
	// Notify stores in-app notifications (the bell).
	Notify *notify.Service
	// SessionIdle ends a session after this long without activity.
	SessionIdle time.Duration
	// DataKey encrypts secrets stored in the database (DATA_ENCRYPTION_KEY, or JWT_SECRET).
	DataKey []byte
	// MFARoles must use two-step verification (MFA_REQUIRED_ROLES).
	MFARoles []string
	// Company details for document letterheads, and quotation settings.
	Company               config.Company
	QuoteApprovalDiscount float64
	QuoteValidDays        int
}

type Router struct {
	Public *echo.Group // no authentication, e.g. login
	Authed *echo.Group // bearer token required
}

type Module interface {
	Name() string
	// Migrations returns the module's embedded SQL files, or nil.
	Migrations() fs.FS
	// Permissions lists the "resource:action" permissions this module checks.
	Permissions() []string
	Routes(r Router)
}

// Starter is implemented by modules with background work, such as reminders.
// Start must return quickly and stop its work when ctx is cancelled.
type Starter interface {
	Start(ctx context.Context)
}
