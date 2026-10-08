// Package orders is customer orders: what a client has committed to buy,
// made from an accepted quotation. It tracks where each item comes from, how
// much is ready, what has been delivered, and when it was invoiced. It follows
// the clients module's layout (see clients/module.go).
//
//	service.go    – order rules: create, change, progress, deliver, hold, cancel, invoice
//	repository.go – all SQL for the orders tables
//	qty.go        – exact quantities and the status they imply
//	reminders.go  – promised dates coming up or missed
package orders

import (
	"context"
	"io/fs"
	"time"

	"dos-bpas/internal/clients"
	"dos-bpas/internal/deals"
	"dos-bpas/internal/orders/migrations"
	"dos-bpas/internal/platform/module"
	"dos-bpas/internal/quotes"
)

const (
	PermRead    = "orders:read"
	PermCreate  = "orders:create"  // turn your own accepted quotations into orders
	PermFulfil  = "orders:fulfil"  // update progress and record deliveries on any order
	PermInvoice = "orders:invoice" // mark delivered orders as invoiced
	PermManage  = "orders:manage"  // change, hold or cancel anyone's orders
)

type Module struct {
	svc       *Service
	reminders *Reminders
}

func New(d module.Deps, cl clients.API, qt quotes.API, dl deals.API) *Module {
	svc := NewService(d.DB, d.Numbers, cl, qt, dl, d.Now, Options{AppURL: d.AppURL, Location: d.Location, Log: d.Log})
	return &Module{svc: svc, reminders: NewReminders(svc)}
}

// Start runs the delivery-date reminders in the background (see reminders.go).
func (m *Module) Start(ctx context.Context) { m.reminders.Start(ctx, 30*time.Minute) }

func (m *Module) Name() string      { return "orders" }
func (m *Module) Migrations() fs.FS { return migrations.FS }
func (m *Module) Permissions() []string {
	return []string{PermRead, PermCreate, PermFulfil, PermInvoice, PermManage}
}

func (m *Module) Routes(r module.Router) {
	h := &handler{svc: m.svc}
	h.routes(r.Authed.Group("/orders"))
}
