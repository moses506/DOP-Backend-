// Package quotes is quotations: the product and price list, quotations with
// priced lines and VAT, approval of large discounts, the PDF sent to the
// client, revisions, and the client's decision, which closes the deal.
// It follows the clients module's layout (see clients/module.go).
//
//	products.go   – the price list
//	service.go    – quotation rules: create, edit, submit, approve, send, accept, revise
//	money.go      – exact money arithmetic for lines and totals
//	document.go   – the PDF and the email it goes out with
//	reminders.go  – expiry and follow-up reminders
package quotes

import (
	"context"
	"io/fs"
	"time"

	"dos-bpas/internal/clients"
	"dos-bpas/internal/deals"
	"dos-bpas/internal/platform/module"
	"dos-bpas/internal/quotes/migrations"
)

const (
	PermRead           = "quotes:read"
	PermCreate         = "quotes:create"  // prepare and send your own quotations
	PermApprove        = "quotes:approve" // approve discounts above the limit
	PermManage         = "quotes:manage"  // change anyone's quotations
	PermProductsRead   = "products:read"
	PermProductsManage = "products:manage" // maintain the price list
)

type Module struct {
	svc       *Service
	reminders *Reminders
}

func New(d module.Deps, cl clients.API, dl deals.API) *Module {
	svc := NewService(d.DB, d.Numbers, cl, dl, d.Mail, d.Now, Options{
		Company: d.Company, ApprovalDiscount: d.QuoteApprovalDiscount, ValidDays: d.QuoteValidDays,
		AppURL: d.AppURL, Location: d.Location, Log: d.Log,
	})
	return &Module{svc: svc, reminders: NewReminders(svc)}
}

// Start runs the expiry reminders in the background (see reminders.go).
func (m *Module) Start(ctx context.Context) { m.reminders.Start(ctx, 30*time.Minute) }

func (m *Module) Name() string      { return "quotes" }
func (m *Module) Migrations() fs.FS { return migrations.FS }
func (m *Module) Permissions() []string {
	return []string{PermRead, PermCreate, PermApprove, PermManage, PermProductsRead, PermProductsManage}
}

func (m *Module) Routes(r module.Router) {
	h := &handler{svc: m.svc}
	h.routes(r.Authed.Group("/quotes"), r.Authed.Group("/products"))
}
