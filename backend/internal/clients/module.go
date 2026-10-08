// Package clients is the client register: mandatory client registration,
// contacts, delivery sites, duplicate control and activity history
// (brief section 4.1). It is the reference module: copy its layout when
// adding deals, quotations and the rest.
//
//	module.go      – wiring: name, migrations, permissions, routes
//	handler.go     – HTTP only: parse request, call service, write response
//	service.go     – business rules, transactions, audit, events
//	repository.go  – all SQL for this module's tables
//	model.go       – types and validation
//	contactlog.go  – calls, visits and meetings, with follow-ups
//	equipment.go   – equipment installed at clients
//	reminders.go   – background reminders (follow-ups, ending warranties)
//	migrations/    – this module's tables
package clients

import (
	"context"
	"io/fs"
	"time"

	"dos-bpas/internal/clients/migrations"
	"dos-bpas/internal/platform/module"
)

const (
	PermRead              = "clients:read"
	PermCreate            = "clients:create"
	PermUpdate            = "clients:update"
	PermDeactivate        = "clients:deactivate"
	PermOverrideDuplicate = "clients:override_duplicate"
	PermLog               = "clients:log"       // log calls, visits and meetings; follow-ups
	PermEquipment         = "clients:equipment" // register and update equipment
	PermManage            = "clients:manage"    // change anyone's follow-ups
)

type Module struct {
	svc       *Service
	reminders *Reminders
}

func New(d module.Deps) *Module {
	svc := NewService(d.DB, d.Numbers, d.Events, d.Now)
	if d.Location != nil {
		svc.tz = d.Location.String()
	}
	return &Module{svc: svc, reminders: NewReminders(d.DB, d.Mail, d.AppURL, d.Location, d.Log)}
}

// Start runs the follow-up and equipment reminders in the background (see reminders.go).
func (m *Module) Start(ctx context.Context) { m.reminders.Start(ctx, 15*time.Minute) }

// API exposes the client service to other modules (by interface only).
func (m *Module) API() API { return m.svc }

func (m *Module) Name() string      { return "clients" }
func (m *Module) Migrations() fs.FS { return migrations.FS }
func (m *Module) Permissions() []string {
	return []string{PermRead, PermCreate, PermUpdate, PermDeactivate, PermOverrideDuplicate, PermLog, PermEquipment, PermManage}
}

func (m *Module) Routes(r module.Router) {
	h := &handler{svc: m.svc}
	h.routes(r.Authed.Group("/clients"))
}
