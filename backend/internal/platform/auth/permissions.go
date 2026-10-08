package auth

import "strings"

// Role names match the access roles in the implementation brief (section 2).
type Role string

const (
	RoleCEO            Role = "ceo"
	RoleSalesManager   Role = "sales_manager"
	RoleSalesExecutive Role = "sales_executive"
	RoleOperations     Role = "operations"
	RoleTechnical      Role = "technical"
	RoleFinance        Role = "finance"
	RoleAdmin          Role = "admin"
)

var Roles = []Role{RoleCEO, RoleSalesManager, RoleSalesExecutive, RoleOperations, RoleTechnical, RoleFinance, RoleAdmin}

func (r Role) Valid() bool {
	for _, x := range Roles {
		if r == x {
			return true
		}
	}
	return false
}

// rolePermissions is the single place to review who can do what.
// Permissions are "resource:action". Patterns may use "*" for either part.
// Each module declares its own permission constants; add them here when a
// module is introduced.
var rolePermissions = map[Role][]string{
	RoleAdmin: {"*:*"},
	RoleCEO: {
		"*:read",
		"clients:*",
		"deals:*",
		"quotes:*",
		"products:*",
		"orders:*",
		"suppliers:*",
		"sourcing:*",
		"audit:read",
	},
	RoleSalesManager: {
		"clients:read", "clients:create", "clients:update",
		"clients:deactivate", "clients:override_duplicate",
		"clients:log", "clients:equipment", "clients:manage",
		"deals:read", "deals:create", "deals:update", "deals:manage",
		"quotes:read", "quotes:create", "quotes:approve", "quotes:manage", "products:read", "products:manage",
		"orders:read", "orders:create", "orders:fulfil", "orders:manage",
		"suppliers:read", "suppliers:manage", "sourcing:read", "sourcing:create", "sourcing:manage",
	},
	RoleSalesExecutive: {
		"clients:read", "clients:create", "clients:update", "clients:log", "clients:equipment",
		"deals:read", "deals:create", "deals:update", // their own deals; notes on any
		"quotes:read", "quotes:create", "products:read", // their own quotations
		"orders:read", "orders:create", // orders from their own quotations
		"suppliers:read", "sourcing:read", "sourcing:create", // ask suppliers for prices before quoting
	},
	RoleOperations: {"clients:read", "clients:create", "clients:update", "clients:log", "clients:equipment", "deals:read", "quotes:read", "products:read",
		"orders:read", "orders:fulfil", // they source and deliver
		"suppliers:read", "suppliers:manage", "sourcing:read", "sourcing:create"},
	RoleTechnical: {"clients:read", "clients:log", "clients:equipment", "deals:read", "quotes:read", "products:read",
		"orders:read",                                         // they install and service equipment
		"suppliers:read", "sourcing:read", "sourcing:create"}, // and ask for spare parts prices
	RoleFinance: {"clients:read", "clients:update", "clients:log", "deals:read", "quotes:read", "products:read",
		"orders:read", "orders:invoice",
		"suppliers:read", "suppliers:manage", "sourcing:read"}, // supplier accounts and credit
}

// Can reports whether the role grants the permission.
func (r Role) Can(perm string) bool {
	res, act, _ := strings.Cut(perm, ":")
	for _, p := range rolePermissions[r] {
		pr, pa, _ := strings.Cut(p, ":")
		if (pr == "*" || pr == res) && (pa == "*" || pa == act) {
			return true
		}
	}
	return false
}

// Expand lists concrete permissions the role holds out of the known set, so
// the frontend can show or hide actions.
func (r Role) Expand(known []string) []string {
	out := make([]string, 0, len(known))
	for _, k := range known {
		if r.Can(k) {
			out = append(out, k)
		}
	}
	return out
}
