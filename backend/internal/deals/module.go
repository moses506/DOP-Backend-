// Package deals is the sales pipeline: opportunities with registered clients,
// moved through stages until won or lost, with calls, meetings and notes
// logged against them. Quotations (the next module) will link to a deal.
// It follows the clients module's layout (see clients/module.go).
package deals

import (
	"context"
	"io/fs"
	"time"

	"dos-bpas/internal/clients"
	"dos-bpas/internal/deals/migrations"
	"dos-bpas/internal/platform/module"
)

const (
	PermRead   = "deals:read"
	PermCreate = "deals:create"
	PermUpdate = "deals:update" // change your own deals and log notes
	PermManage = "deals:manage" // change anyone's deals and hand deals over
)

type Module struct {
	svc       *Service
	reminders *Reminders
}

func New(d module.Deps, cl clients.API) *Module {
	svc := NewService(d.DB, d.Numbers, d.Events, cl, d.Now)
	svc.log = d.Log
	if d.Location != nil {
		svc.tz = d.Location.String()
	}
	return &Module{svc: svc, reminders: NewReminders(d.DB, cl, d.Mail, d.AppURL, d.Location, d.Log)}
}

// Start runs the follow-up reminders in the background (see reminders.go).
func (m *Module) Start(ctx context.Context) { m.reminders.Start(ctx, 15*time.Minute) }

// API exposes the deal service to other modules (by interface only).
func (m *Module) API() API { return m.svc }

func (m *Module) Name() string      { return "deals" }
func (m *Module) Migrations() fs.FS { return migrations.FS }
func (m *Module) Permissions() []string {
	return []string{PermRead, PermCreate, PermUpdate, PermManage}
}

func (m *Module) Routes(r module.Router) {
	h := &handler{svc: m.svc}
	h.routes(r.Authed.Group("/deals"))
}
