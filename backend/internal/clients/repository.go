package clients

import (
	"context"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dos-bpas/internal/platform/db"
)

// All SQL for the clients module lives in this file. Only this module touches
// the clients, client_contacts and client_sites tables.

const clientSelect = `
	SELECT c.id, c.reference, c.legal_name, c.trading_name, c.registration_no, c.tpin, c.sector,
	       c.billing_address, c.default_currency, c.credit_terms_days, c.credit_limit::text, c.status,
	       c.notes, o.id, o.full_name, o.position, cb.id, cb.full_name, cb.position,
	       c.created_at, c.updated_at, c.deactivated_at, c.deactivation_reason,
	       c.last_contact_at,
	       CASE WHEN c.last_contact_at IS NULL THEN 'never'
	            WHEN c.last_contact_at > now() - interval '30 days' THEN 'good'
	            WHEN c.last_contact_at > now() - interval '60 days' THEN 'fading'
	            ELSE 'cold' END,
	       to_char(fu.next_on, 'YYYY-MM-DD'), coalesce(fu.open, 0)::int,
	       c.version
	FROM clients c
	JOIN users o  ON o.id  = c.owner_id
	JOIN users cb ON cb.id = c.created_by
	LEFT JOIN LATERAL (
	    SELECT min(i.follow_up_on) AS next_on, count(*) AS open
	    FROM client_interactions i
	    WHERE i.client_id = c.id AND i.follow_up_on IS NOT NULL AND i.follow_up_done_at IS NULL
	) fu ON true`

func scanClient(row pgx.Row, extra ...any) (Client, error) {
	var c Client
	dest := []any{&c.ID, &c.Reference, &c.LegalName, &c.TradingName, &c.RegistrationNo, &c.TPIN, &c.Sector,
		&c.BillingAddress, &c.DefaultCurrency, &c.CreditTermsDays, &c.CreditLimit, &c.Status,
		&c.Notes, &c.Owner.ID, &c.Owner.Name, &c.Owner.Position, &c.CreatedBy.ID, &c.CreatedBy.Name,
		&c.CreatedBy.Position, &c.CreatedAt, &c.UpdatedAt, &c.DeactivatedAt, &c.DeactivationReason,
		&c.LastContactAt, &c.ContactHealth, &c.NextFollowUpOn, &c.OpenFollowUps, &c.Version}
	err := row.Scan(append(dest, extra...)...)
	return c, err
}

func getClient(ctx context.Context, q db.Querier, id uuid.UUID, forUpdate bool) (Client, error) {
	sql := clientSelect + ` WHERE c.id = $1`
	if forUpdate {
		sql += ` FOR UPDATE OF c`
	}
	return scanClient(q.QueryRow(ctx, sql, id))
}

func listClients(ctx context.Context, q db.Querier, f ListFilter) ([]Client, int, error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if s := strings.TrimSpace(f.Query); s != "" {
		like := arg("%" + escapeLike(s) + "%")
		exact := arg(strings.ToUpper(strings.ReplaceAll(s, " ", "")))
		serial := arg(SerialKey(s))
		// A serial number or asset tag finds the client the equipment is at.
		where = append(where, `((c.legal_name || ' ' || coalesce(c.trading_name,'') || ' ' || c.reference) ILIKE `+
			like+` OR c.tpin = `+exact+` OR c.registration_no = `+exact+
			` OR EXISTS (SELECT 1 FROM client_equipment e WHERE e.client_id = c.id AND e.status = 'in_use'
			              AND (e.serial_key = `+serial+` OR upper(e.asset_tag) = `+exact+`)))`)
	}
	order := `c.legal_name, c.reference`
	switch f.Attention {
	case "followups":
		where = append(where, `EXISTS (SELECT 1 FROM client_interactions i WHERE i.client_id = c.id
			AND i.follow_up_owner_id = `+arg(f.Me)+` AND i.follow_up_done_at IS NULL
			AND i.follow_up_on <= (now() AT TIME ZONE `+arg(f.TZ)+`)::date)`)
		order = `fu.next_on, c.legal_name`
	case "cold":
		where = append(where, `c.status = 'active' AND (c.last_contact_at IS NULL OR c.last_contact_at < now() - interval '`+
			strconv.Itoa(ColdAfterDays)+` days')`)
		order = `c.last_contact_at NULLS FIRST, c.legal_name`
	}
	if f.Status != "" {
		where = append(where, `c.status = `+arg(string(f.Status)))
	}
	if f.OwnerID != nil {
		where = append(where, `c.owner_id = `+arg(*f.OwnerID))
	}
	// Append the total as the last column so scanClient's column order stays the same.
	sql := strings.Replace(clientSelect, "c.version\n", "c.version, count(*) OVER ()\n", 1)
	if len(where) > 0 {
		sql += ` WHERE ` + strings.Join(where, " AND ")
	}
	sql += ` ORDER BY ` + order + ` LIMIT ` + arg(f.Page.Limit) + ` OFFSET ` + arg(f.Page.Offset)

	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Client{}
	total := 0
	for rows.Next() {
		c, err := scanClient(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// findDuplicates matches on name key (skipped when empty), TPIN or registration number.
func findDuplicates(ctx context.Context, q db.Querier, nameKey string, tpin, regNo *string, exclude *uuid.UUID) ([]PossibleDuplicate, error) {
	rows, err := q.Query(ctx, `
		SELECT id, reference, legal_name, status,
		       array_remove(ARRAY[
		           CASE WHEN $1 <> '' AND name_key = $1 THEN 'name' END,
		           CASE WHEN $2::text IS NOT NULL AND tpin = $2 THEN 'tpin' END,
		           CASE WHEN $3::text IS NOT NULL AND registration_no = $3 THEN 'registration_no' END
		       ], NULL)
		FROM clients
		WHERE (($1 <> '' AND name_key = $1) OR ($2::text IS NOT NULL AND tpin = $2) OR ($3::text IS NOT NULL AND registration_no = $3))
		  AND ($4::uuid IS NULL OR id <> $4)
		ORDER BY legal_name
		LIMIT 10`, nameKey, tpin, regNo, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PossibleDuplicate{}
	for rows.Next() {
		var d PossibleDuplicate
		if err := rows.Scan(&d.ID, &d.Reference, &d.LegalName, &d.Status, &d.MatchedOn); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func activeUserExists(ctx context.Context, q db.Querier, id uuid.UUID) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1 AND active)`, id).Scan(&ok)
	return ok, err
}

type clientRow struct {
	Reference, LegalName, NameKey, Currency string
	TradingName, RegNo, TPIN, Sector        *string
	BillingAddress, CreditLimit, Notes      *string
	DuplicateReason                         *string
	CreditTermsDays                         int
	OwnerID, CreatedBy                      uuid.UUID
}

func insertClient(ctx context.Context, q db.Querier, r clientRow) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO clients (reference, legal_name, trading_name, name_key, registration_no, tpin, sector,
		                     billing_address, default_currency, credit_terms_days, credit_limit, notes,
		                     owner_id, created_by, duplicate_override_reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::numeric,$12,$13,$14,$15)
		RETURNING id`,
		r.Reference, r.LegalName, r.TradingName, r.NameKey, r.RegNo, r.TPIN, r.Sector,
		r.BillingAddress, r.Currency, r.CreditTermsDays, r.CreditLimit, r.Notes,
		r.OwnerID, r.CreatedBy, r.DuplicateReason).Scan(&id)
	return id, err
}

func updateClient(ctx context.Context, q db.Querier, c Client) error {
	_, err := q.Exec(ctx, `
		UPDATE clients SET
		    legal_name = $2, trading_name = $3, name_key = $4, registration_no = $5, tpin = $6,
		    sector = $7, billing_address = $8, default_currency = $9, credit_terms_days = $10,
		    credit_limit = $11::numeric, notes = $12, owner_id = $13,
		    status = $14, deactivated_at = $15, deactivation_reason = $16,
		    updated_at = now(), version = version + 1
		WHERE id = $1`,
		c.ID, c.LegalName, c.TradingName, NameKey(c.LegalName), c.RegistrationNo, c.TPIN,
		c.Sector, c.BillingAddress, c.DefaultCurrency, c.CreditTermsDays,
		c.CreditLimit, c.Notes, c.Owner.ID, c.Status, c.DeactivatedAt, c.DeactivationReason)
	return err
}

// ---- contacts ----

func listContacts(ctx context.Context, q db.Querier, clientID uuid.UUID) ([]Contact, error) {
	rows, err := q.Query(ctx, `
		SELECT id, name, position, email, phone, is_primary, active
		FROM client_contacts WHERE client_id = $1
		ORDER BY active DESC, is_primary DESC, name`, clientID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Contact, error) {
		var c Contact
		err := r.Scan(&c.ID, &c.Name, &c.Position, &c.Email, &c.Phone, &c.IsPrimary, &c.Active)
		return c, err
	})
}

func insertContact(ctx context.Context, q db.Querier, clientID uuid.UUID, in ContactInput) (Contact, error) {
	if in.IsPrimary {
		if _, err := q.Exec(ctx, `UPDATE client_contacts SET is_primary = false, updated_at = now()
			WHERE client_id = $1 AND is_primary`, clientID); err != nil {
			return Contact{}, err
		}
	}
	var c Contact
	err := q.QueryRow(ctx, `
		INSERT INTO client_contacts (client_id, name, position, email, phone, is_primary)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, name, position, email, phone, is_primary, active`,
		clientID, strings.TrimSpace(in.Name), clean(in.Position), clean(in.Email), clean(in.Phone), in.IsPrimary,
	).Scan(&c.ID, &c.Name, &c.Position, &c.Email, &c.Phone, &c.IsPrimary, &c.Active)
	return c, err
}

func updateContact(ctx context.Context, q db.Querier, clientID, contactID uuid.UUID, in ContactInput, active bool) (Contact, error) {
	if in.IsPrimary && active {
		if _, err := q.Exec(ctx, `UPDATE client_contacts SET is_primary = false, updated_at = now()
			WHERE client_id = $1 AND is_primary AND id <> $2`, clientID, contactID); err != nil {
			return Contact{}, err
		}
	}
	var c Contact
	err := q.QueryRow(ctx, `
		UPDATE client_contacts
		SET name = $3, position = $4, email = $5, phone = $6, is_primary = $7 AND $8, active = $8, updated_at = now()
		WHERE client_id = $1 AND id = $2
		RETURNING id, name, position, email, phone, is_primary, active`,
		clientID, contactID, strings.TrimSpace(in.Name), clean(in.Position), clean(in.Email), clean(in.Phone),
		in.IsPrimary, active,
	).Scan(&c.ID, &c.Name, &c.Position, &c.Email, &c.Phone, &c.IsPrimary, &c.Active)
	return c, err
}

// ---- sites ----

func listSites(ctx context.Context, q db.Querier, clientID uuid.UUID) ([]Site, error) {
	rows, err := q.Query(ctx, `
		SELECT id, name, address, contact_name, contact_phone, notes, active
		FROM client_sites WHERE client_id = $1 ORDER BY active DESC, name`, clientID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Site, error) {
		var s Site
		err := r.Scan(&s.ID, &s.Name, &s.Address, &s.ContactName, &s.ContactPhone, &s.Notes, &s.Active)
		return s, err
	})
}

func insertSite(ctx context.Context, q db.Querier, clientID uuid.UUID, in SiteInput) (Site, error) {
	var s Site
	err := q.QueryRow(ctx, `
		INSERT INTO client_sites (client_id, name, address, contact_name, contact_phone, notes)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, name, address, contact_name, contact_phone, notes, active`,
		clientID, strings.TrimSpace(in.Name), strings.TrimSpace(in.Address),
		clean(in.ContactName), clean(in.ContactPhone), clean(in.Notes),
	).Scan(&s.ID, &s.Name, &s.Address, &s.ContactName, &s.ContactPhone, &s.Notes, &s.Active)
	return s, err
}

// ---- helpers ----

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func lookupRefs(ctx context.Context, q db.Querier, ids []uuid.UUID) (map[uuid.UUID]Ref, error) {
	out := make(map[uuid.UUID]Ref, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT id, reference, legal_name, sector, status FROM clients WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Ref
		if err := rows.Scan(&r.ID, &r.Reference, &r.LegalName, &r.Sector, &r.Status); err != nil {
			return nil, err
		}
		out[r.ID] = r
	}
	return out, rows.Err()
}

func matchIDs(ctx context.Context, q db.Querier, query string, limit int) ([]uuid.UUID, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT id FROM clients
		WHERE (legal_name || ' ' || coalesce(trading_name,'') || ' ' || reference) ILIKE $1
		   OR tpin = $2
		LIMIT $3`, "%"+escapeLike(query)+"%", strings.ToUpper(strings.ReplaceAll(query, " ", "")), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
