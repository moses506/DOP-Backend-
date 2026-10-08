package sourcing

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dos-bpas/internal/clients"
	"dos-bpas/internal/deals"
	"dos-bpas/internal/platform/audit"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/httpx"
	mailer "dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/numbering"
)

const (
	supplierEntity = "supplier"
	requestEntity  = "price_request"
)

type Service struct {
	pool    *pgxpool.Pool
	numbers *numbering.Service
	clients clients.API
	deals   deals.API
	mail    *mailer.Service
	log     *slog.Logger
	now     func() time.Time
	loc     *time.Location
	appURL  string
	company string
}

type Options struct {
	AppURL      string
	CompanyName string
	Location    *time.Location
	Log         *slog.Logger
}

func NewService(pool *pgxpool.Pool, numbers *numbering.Service, cl clients.API, dl deals.API, m *mailer.Service, now func() time.Time, o Options) *Service {
	if o.Location == nil {
		o.Location, _ = time.LoadLocation("Africa/Lusaka")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.CompanyName == "" {
		o.CompanyName = "Digital Office Solutions Limited"
	}
	return &Service{pool: pool, numbers: numbers, clients: cl, deals: dl, mail: m, log: o.Log, now: now, loc: o.Location,
		appURL: strings.TrimRight(o.AppURL, "/"), company: o.CompanyName}
}

func (s *Service) today() string { return s.now().In(s.loc).Format("2006-01-02") }

var errNoChange = httpx.NewError(http.StatusUnprocessableEntity, "no_change", "Nothing was changed.")

func duplicateSupplier(id uuid.UUID, ref, name string) error {
	return httpx.NewError(http.StatusConflict, "duplicate_supplier",
		name+" is already registered as "+ref+".").With(map[string]any{"supplier_id": id, "reference": ref, "name": name})
}

// ---- names ----

var (
	nonWord  = regexp.MustCompile(`[^a-z0-9 ]+`)
	suffixes = regexp.MustCompile(`\b(pty|ltd|limited|inc|incorporated|co|company|plc)\b`)
	spaces   = regexp.MustCompile(`\s+`)
)

// NameKey makes "Axiz (Pty) Ltd" and "AXIZ" the same supplier.
func NameKey(name string) string {
	k := strings.ToLower(strings.ReplaceAll(name, "&", " and "))
	k = nonWord.ReplaceAllString(k, " ")
	k = suffixes.ReplaceAllString(k, " ")
	return strings.TrimSpace(spaces.ReplaceAllString(k, " "))
}

// ---- queries ----

const supplierSelect = `
	SELECT s.id, s.reference, s.name, s.website, s.brands, s.notes, s.status, s.inactive_reason,
	       u.id, u.full_name, u.position, s.created_at, s.updated_at, s.version,
	       coalesce(st.asked, 0), coalesce(st.replied, 0), st.last_asked
	FROM suppliers s
	JOIN users u ON u.id = s.created_by
	LEFT JOIN LATERAL (
		SELECT count(*) AS asked, count(*) FILTER (WHERE i.status = 'replied') AS replied, max(i.sent_at) AS last_asked
		FROM price_request_invites i WHERE i.supplier_id = s.id
	) st ON true`

func scanSupplier(r pgx.Row, extra ...any) (Supplier, error) {
	var x Supplier
	dest := []any{&x.ID, &x.Reference, &x.Name, &x.Website, &x.Brands, &x.Notes, &x.Status, &x.InactiveReason,
		&x.CreatedBy.ID, &x.CreatedBy.Name, &x.CreatedBy.Position, &x.CreatedAt, &x.UpdatedAt, &x.Version,
		&x.Asked, &x.Replied, &x.LastAsked}
	err := r.Scan(append(dest, extra...)...)
	x.Accounts = []Account{}
	return x, err
}

func accountsFor(ctx context.Context, q db.Querier, ids []uuid.UUID, includeInactive bool) (map[uuid.UUID][]Account, error) {
	sql := `SELECT supplier_id, id, region, account_number, terms, credit_limit::text, currency, contact_name, contact_email,
	               other_emails, note, active
	        FROM supplier_accounts WHERE supplier_id = ANY($1)`
	if !includeInactive {
		sql += ` AND active`
	}
	sql += ` ORDER BY CASE region WHEN 'ZA' THEN 1 WHEN 'ZM' THEN 2 WHEN 'GB' THEN 3 ELSE 4 END, created_at`
	rows, err := q.Query(ctx, sql, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]Account{}
	for rows.Next() {
		var sid uuid.UUID
		var a Account
		if err := rows.Scan(&sid, &a.ID, &a.Region, &a.AccountNumber, &a.Terms, &a.CreditLimit, &a.Currency, &a.ContactName,
			&a.ContactEmail, &a.OtherEmails, &a.Note, &a.Active); err != nil {
			return nil, err
		}
		a.RegionName = regions[a.Region]
		if a.OtherEmails == nil {
			a.OtherEmails = []string{}
		}
		out[sid] = append(out[sid], a)
	}
	return out, rows.Err()
}

func (s *Service) GetSupplier(ctx context.Context, id uuid.UUID) (Supplier, error) {
	x, err := scanSupplier(s.pool.QueryRow(ctx, supplierSelect+` WHERE s.id = $1`, id))
	if db.IsNoRows(err) {
		return Supplier{}, httpx.ErrNotFound
	}
	if err != nil {
		return Supplier{}, err
	}
	accts, err := accountsFor(ctx, s.pool, []uuid.UUID{id}, false)
	if err != nil {
		return Supplier{}, err
	}
	if a := accts[id]; a != nil {
		x.Accounts = a
	}
	return x, nil
}

func (s *Service) ListSuppliers(ctx context.Context, f SupplierFilter) (httpx.List[Supplier], error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := arg("%" + escapeLike(q) + "%")
		where = append(where, `(s.name ILIKE `+like+` OR s.brands ILIKE `+like+` OR s.reference ILIKE `+like+` OR s.website ILIKE `+like+
			` OR EXISTS (SELECT 1 FROM supplier_accounts a WHERE a.supplier_id = s.id AND a.active AND
			      (a.contact_email ILIKE `+like+` OR a.account_number ILIKE `+like+` OR a.contact_name ILIKE `+like+`)))`)
	}
	if f.Region != "" || f.Terms != "" {
		cond := `EXISTS (SELECT 1 FROM supplier_accounts a WHERE a.supplier_id = s.id AND a.active`
		if f.Region != "" {
			cond += ` AND a.region = ` + arg(f.Region)
		}
		if f.Terms != "" {
			cond += ` AND a.terms = ` + arg(f.Terms)
		}
		where = append(where, cond+`)`)
	}
	switch f.Status {
	case "", "active":
		where = append(where, `s.status = 'active'`)
	case "inactive":
		where = append(where, `s.status = 'inactive'`)
	}
	sql := strings.Replace(supplierSelect, "st.last_asked\n", "st.last_asked, count(*) OVER ()\n", 1)
	if len(where) > 0 {
		sql += ` WHERE ` + strings.Join(where, " AND ")
	}
	sql += ` ORDER BY coalesce(st.asked, 0) DESC, s.name LIMIT ` + arg(f.Limit) + ` OFFSET ` + arg(f.Offset)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return httpx.List[Supplier]{}, err
	}
	items := []Supplier{}
	total := 0
	ids := []uuid.UUID{}
	for rows.Next() {
		x, err := scanSupplier(rows, &total)
		if err != nil {
			rows.Close()
			return httpx.List[Supplier]{}, err
		}
		items = append(items, x)
		ids = append(ids, x.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return httpx.List[Supplier]{}, err
	}
	accts, err := accountsFor(ctx, s.pool, ids, false)
	if err != nil {
		return httpx.List[Supplier]{}, err
	}
	for i := range items {
		if a := accts[items[i].ID]; a != nil {
			items[i].Accounts = a
		}
	}
	return httpx.List[Supplier]{Items: items, Total: total, Limit: f.Limit, Offset: f.Offset}, nil
}

// ---- changes ----

type cleanAccount struct {
	AccountInput
	limit *string
}

func (s *Service) checkSupplier(in *SupplierInput) ([]cleanAccount, error) {
	fe := httpx.FieldErrors{}
	in.Name = strings.Join(strings.Fields(in.Name), " ")
	if in.Name == "" {
		fe.Add("name", "Enter the supplier's name.")
	} else if utf8.RuneCountInString(in.Name) > 160 {
		fe.Add("name", "Keep the name under 160 characters.")
	}
	in.Website, in.Brands, in.Notes = clean(in.Website), clean(in.Brands), clean(in.Notes)
	if in.Website != nil && !strings.Contains(*in.Website, ".") {
		fe.Add("website", "Enter a web address, e.g. https://www.axiz.com.")
	}
	for _, f := range []struct {
		k string
		v *string
	}{{"brands", in.Brands}, {"notes", in.Notes}} {
		if f.v != nil && utf8.RuneCountInString(*f.v) > 2000 {
			fe.Add(f.k, "Keep it under 2,000 characters.")
		}
	}
	if len(in.Accounts) == 0 {
		fe.Add("accounts", "Add at least one account, with the email address to send price requests to.")
	}
	seen := map[string]bool{}
	var out []cleanAccount
	for i, a := range in.Accounts {
		p := fmt.Sprintf("accounts.%d.", i)
		a.Region = strings.ToUpper(strings.TrimSpace(a.Region))
		if _, ok := regions[a.Region]; !ok {
			fe.Add(p+"region", "Choose South Africa, Zambia, United Kingdom or Other.")
		} else if seen[a.Region] {
			fe.Add(p+"region", "There's already an account for "+regions[a.Region]+". Use one account per region.")
		}
		seen[a.Region] = true
		a.AccountNumber, a.ContactName, a.Note = clean(a.AccountNumber), clean(a.ContactName), clean(a.Note)
		a.Currency = strings.ToUpper(strings.TrimSpace(a.Currency))
		if len(a.Currency) != 3 {
			fe.Add(p+"currency", "Choose the currency they bill in.")
		}
		a.ContactEmail = strings.TrimSpace(a.ContactEmail)
		if _, err := mail.ParseAddress(a.ContactEmail); err != nil || a.ContactEmail == "" {
			fe.Add(p+"contact_email", "Enter the email address price requests go to.")
		}
		var others []string
		for _, e := range a.OtherEmails {
			for _, part := range strings.FieldsFunc(e, func(r rune) bool { return r == ',' || r == ';' || r == ' ' }) {
				if part == "" || strings.EqualFold(part, a.ContactEmail) {
					continue
				}
				if _, err := mail.ParseAddress(part); err != nil {
					fe.Add(p+"other_emails", part+" isn't a valid email address.")
					continue
				}
				others = append(others, part)
			}
		}
		a.OtherEmails = others
		ca := cleanAccount{AccountInput: a}
		if a.Terms != nil && strings.TrimSpace(*a.Terms) == "" {
			a.Terms = nil
		}
		ca.Terms = a.Terms
		if a.Terms != nil && *a.Terms != "credit" && *a.Terms != "cod" {
			fe.Add(p+"terms", "Choose credit or cash on delivery.")
		}
		if lim := clean(a.CreditLimit); lim != nil {
			if a.Terms == nil || *a.Terms != "credit" {
				fe.Add(p+"credit_limit", "Only credit accounts have a credit limit.")
			} else if v, err := parseMoney(*lim); err != nil {
				fe.Add(p+"credit_limit", "Enter an amount, e.g. 250000.")
			} else {
				str := v.String()
				ca.limit = &str
			}
		}
		out = append(out, ca)
	}
	return out, fe.Err()
}

func (s *Service) CreateSupplier(ctx context.Context, in SupplierInput) (Supplier, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return Supplier{}, httpx.ErrUnauthorized
	}
	accts, err := s.checkSupplier(&in)
	if err != nil {
		return Supplier{}, err
	}
	var id uuid.UUID
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var dup uuid.UUID
		var dupRef, dupName string
		err := tx.QueryRow(ctx, `SELECT id, reference, name FROM suppliers WHERE name_key = $1`, NameKey(in.Name)).Scan(&dup, &dupRef, &dupName)
		if err == nil {
			return duplicateSupplier(dup, dupRef, dupName)
		}
		if !db.IsNoRows(err) {
			return err
		}
		ref, err := s.numbers.Next(ctx, tx, numbering.Supplier)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO suppliers (reference, name, name_key, website, brands, notes, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`, ref, in.Name, NameKey(in.Name), in.Website, in.Brands, in.Notes, actor.UserID).Scan(&id); err != nil {
			if db.IsUniqueViolation(err) {
				return httpx.NewError(http.StatusConflict, "duplicate_supplier", in.Name+" is already registered.")
			}
			return err
		}
		for _, a := range accts {
			if err := insertAccount(ctx, tx, id, a); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: supplierEntity, EntityID: id, Action: "created",
			Summary: fmt.Sprintf("Supplier registered with %d account(s)", len(accts))})
	})
	if err != nil {
		return Supplier{}, err
	}
	return s.GetSupplier(ctx, id)
}

func insertAccount(ctx context.Context, tx pgx.Tx, supplierID uuid.UUID, a cleanAccount) error {
	_, err := tx.Exec(ctx, `INSERT INTO supplier_accounts (supplier_id, region, account_number, terms, credit_limit, currency,
		contact_name, contact_email, other_emails, note) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		supplierID, a.Region, a.AccountNumber, a.Terms, a.limit, a.Currency, a.ContactName, a.ContactEmail, nonNil(a.OtherEmails), a.Note)
	return err
}

// UpdateSupplier replaces the supplier's details and accounts. Accounts left
// out are made inactive, not deleted, because past price requests point at them.
func (s *Service) UpdateSupplier(ctx context.Context, id uuid.UUID, in SupplierInput) (Supplier, error) {
	accts, err := s.checkSupplier(&in)
	if err != nil {
		return Supplier{}, err
	}
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var version int
		var name string
		err := tx.QueryRow(ctx, `SELECT version, name FROM suppliers WHERE id = $1 FOR UPDATE`, id).Scan(&version, &name)
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if in.Version != version {
			return httpx.ErrStaleVersion.With(map[string]any{"current_version": version})
		}
		var dup uuid.UUID
		var dupRef, dupName string
		err = tx.QueryRow(ctx, `SELECT id, reference, name FROM suppliers WHERE name_key = $1 AND id <> $2`, NameKey(in.Name), id).Scan(&dup, &dupRef, &dupName)
		if err == nil {
			return duplicateSupplier(dup, dupRef, dupName)
		}
		if !db.IsNoRows(err) {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE suppliers SET name=$2, name_key=$3, website=$4, brands=$5, notes=$6, updated_at=now(), version=version+1
			WHERE id=$1`, id, in.Name, NameKey(in.Name), in.Website, in.Brands, in.Notes); err != nil {
			return err
		}
		existing, err := accountsFor(ctx, tx, []uuid.UUID{id}, false)
		if err != nil {
			return err
		}
		keep := map[uuid.UUID]bool{}
		for _, a := range accts {
			if a.ID != nil {
				keep[*a.ID] = true
			}
		}
		removed := 0
		// Retire first, so a region freed by a retired account can be reused.
		for _, old := range existing[id] {
			if !keep[old.ID] {
				removed++
				if _, err := tx.Exec(ctx, `UPDATE supplier_accounts SET active=false, updated_at=now() WHERE id=$1`, old.ID); err != nil {
					return err
				}
			}
		}
		for _, a := range accts {
			if a.ID == nil {
				if err := insertAccount(ctx, tx, id, a); err != nil {
					if db.IsUniqueViolation(err) {
						return httpx.FieldErrors{"accounts": "One account per region."}.Err()
					}
					return err
				}
				continue
			}
			tag, err := tx.Exec(ctx, `UPDATE supplier_accounts SET region=$3, account_number=$4, terms=$5, credit_limit=$6, currency=$7,
				contact_name=$8, contact_email=$9, other_emails=$10, note=$11, updated_at=now()
				WHERE id=$1 AND supplier_id=$2 AND active`, *a.ID, id, a.Region, a.AccountNumber, a.Terms, a.limit, a.Currency,
				a.ContactName, a.ContactEmail, nonNil(a.OtherEmails), a.Note)
			if err != nil {
				if db.IsUniqueViolation(err) {
					return httpx.FieldErrors{"accounts": "One account per region."}.Err()
				}
				return err
			}
			if tag.RowsAffected() == 0 {
				return httpx.FieldErrors{"accounts": "One of these accounts no longer exists. Reload the supplier."}.Err()
			}
		}
		summary := "Details updated"
		if removed > 0 {
			summary += fmt.Sprintf(", %d account(s) removed", removed)
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: supplierEntity, EntityID: id, Action: "updated", Summary: summary})
	})
	if err != nil {
		return Supplier{}, err
	}
	return s.GetSupplier(ctx, id)
}

func (s *Service) SetSupplierActive(ctx context.Context, id uuid.UUID, version int, active bool, reason string) (Supplier, error) {
	reason = strings.TrimSpace(reason)
	if !active && reason == "" {
		return Supplier{}, httpx.FieldErrors{"reason": "Say why, e.g. stopped trading with them."}.Err()
	}
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var v int
		var st string
		err := tx.QueryRow(ctx, `SELECT version, status FROM suppliers WHERE id=$1 FOR UPDATE`, id).Scan(&v, &st)
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if v != version {
			return httpx.ErrStaleVersion.With(map[string]any{"current_version": v})
		}
		if (st == "active") == active {
			return errNoChange
		}
		var r *string
		action, summary := "reactivated", "Made active again"
		if !active {
			r, action, summary = &reason, "deactivated", "Made inactive"
		}
		if _, err := tx.Exec(ctx, `UPDATE suppliers SET status=$2, inactive_reason=$3, updated_at=now(), version=version+1 WHERE id=$1`,
			id, map[bool]string{true: "active", false: "inactive"}[active], r); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: supplierEntity, EntityID: id, Action: action, Summary: summary, Reason: reason})
	})
	if err != nil {
		return Supplier{}, err
	}
	return s.GetSupplier(ctx, id)
}

func (s *Service) SupplierActivity(ctx context.Context, id uuid.UUID) ([]audit.Event, error) {
	return audit.Timeline(ctx, s.pool, supplierEntity, id, 100)
}

// ---- exchange rates ----

func (s *Service) Rates(ctx context.Context) ([]Rate, error) {
	rows, err := s.pool.Query(ctx, `SELECT r.currency, trim_scale(r.rate_to_zmw)::text, u.full_name, r.updated_at
		FROM fx_rates r JOIN users u ON u.id = r.updated_by ORDER BY r.currency`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Rate, error) {
		var x Rate
		err := r.Scan(&x.Currency, &x.RateToZMW, &x.UpdatedBy, &x.UpdatedAt)
		return x, err
	})
}

func (s *Service) SetRate(ctx context.Context, currency, rate string) (Rate, error) {
	actor, _ := auth.FromContext(ctx)
	currency = strings.ToUpper(strings.TrimSpace(currency))
	fe := httpx.FieldErrors{}
	if len(currency) != 3 || currency == "ZMW" {
		fe.Add("currency", "Choose a currency other than ZMW.")
	}
	r, err := parseRate(rate)
	if err != nil {
		fe.Add("rate_to_zmw", "Enter how many kwacha one unit is worth, e.g. 1.48 or 26.85.")
	}
	if err := fe.Err(); err != nil {
		return Rate{}, err
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO fx_rates (currency, rate_to_zmw, updated_by) VALUES ($1,$2,$3)
		ON CONFLICT (currency) DO UPDATE SET rate_to_zmw = EXCLUDED.rate_to_zmw, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		currency, r, actor.UserID); err != nil {
		return Rate{}, err
	}
	rates, err := s.Rates(ctx)
	if err != nil {
		return Rate{}, err
	}
	for _, x := range rates {
		if x.Currency == currency {
			return x, nil
		}
	}
	return Rate{}, httpx.ErrNotFound
}

// ---- helpers ----

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

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
