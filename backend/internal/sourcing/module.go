// Package sourcing is where DOS gets its prices: the suppliers register
// (each supplier with its accounts per region) and price requests (RFQs)
// emailed to suppliers, their replies compared in kwacha, and the supplier
// chosen for each item. Supplier purchase orders will build on this.
// It follows the clients module's layout (see clients/module.go).
//
//	suppliers.go – the register, accounts and exchange rates
//	requests.go  – price requests: ask, record replies, compare, choose
//	money.go     – exact amounts and currency conversion
package sourcing

import (
	"context"
	"io/fs"
	"time"

	"dos-bpas/internal/clients"
	"dos-bpas/internal/deals"
	"dos-bpas/internal/platform/module"
	"dos-bpas/internal/sourcing/migrations"
)

const (
	PermSuppliersRead   = "suppliers:read"
	PermSuppliersManage = "suppliers:manage" // add and change suppliers, their accounts and exchange rates
	PermRead            = "sourcing:read"
	PermCreate          = "sourcing:create" // ask suppliers for prices
	PermManage          = "sourcing:manage" // change anyone's price requests
)

type Module struct{ svc *Service }

func New(d module.Deps, cl clients.API, dl deals.API) *Module {
	return &Module{svc: NewService(d.DB, d.Numbers, cl, dl, d.Mail, d.Now, Options{
		AppURL: d.AppURL, CompanyName: d.Company.Name, Location: d.Location, Log: d.Log,
	})}
}

// Service is used by the admin import command.
func (m *Module) Service() *Service { return m.svc }

// Start reminds people to chase suppliers who haven't replied.
func (m *Module) Start(ctx context.Context) { m.svc.StartReminders(ctx, time.Hour) }

func (m *Module) Name() string      { return "sourcing" }
func (m *Module) Migrations() fs.FS { return migrations.FS }
func (m *Module) Permissions() []string {
	return []string{PermSuppliersRead, PermSuppliersManage, PermRead, PermCreate, PermManage}
}

func (m *Module) Routes(r module.Router) {
	h := &handler{svc: m.svc}
	h.routes(r.Authed.Group("/suppliers"), r.Authed.Group("/sourcing"))
}
