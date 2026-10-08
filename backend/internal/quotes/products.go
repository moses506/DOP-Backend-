package quotes

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"dos-bpas/internal/platform/audit"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/httpx"
)

// The product and price list: what DOS sells and at what price, so quotation
// lines are picked rather than retyped. Prices on a quotation are copied from
// the list when the line is added and can then be changed on that quotation;
// changing the list never changes quotations already made.

var productCategories = map[string]bool{
	"copier": true, "printer": true, "scanner": true, "computer": true, "server": true, "network": true,
	"projector": true, "phone_system": true, "software": true, "consumable": true, "service": true, "other": true,
}

const productEntity = "product"

type Product struct {
	ID          uuid.UUID `json:"id"`
	SKU         *string   `json:"sku"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	Category    string    `json:"category"`
	Unit        string    `json:"unit"`
	UnitPrice   string    `json:"unit_price"`
	Currency    string    `json:"currency"`
	VATRate     string    `json:"vat_rate"`
	Active      bool      `json:"active"`
	UpdatedAt   time.Time `json:"updated_at"`
	Version     int       `json:"version"`
}

type ProductInput struct {
	Version     int     `json:"version"`
	SKU         *string `json:"sku"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	Category    string  `json:"category"`
	Unit        string  `json:"unit"`
	UnitPrice   string  `json:"unit_price"`
	Currency    string  `json:"currency"`
	VATRate     string  `json:"vat_rate"`
	Active      *bool   `json:"active"`
}

const productCols = `id, sku, name, description, category, unit, unit_price::text, currency, vat_rate::text, active, updated_at, version`

func scanProduct(r pgx.Row) (Product, error) {
	var p Product
	err := r.Scan(&p.ID, &p.SKU, &p.Name, &p.Description, &p.Category, &p.Unit, &p.UnitPrice, &p.Currency, &p.VATRate,
		&p.Active, &p.UpdatedAt, &p.Version)
	if err == nil {
		p.VATRate = trimPct(p.VATRate)
	}
	return p, err
}

// Products lists the price list, searched by name or item code.
func (s *Service) Products(ctx context.Context, query string, includeInactive bool, limit int) ([]Product, error) {
	args := []any{}
	where := []string{"true"}
	if q := strings.TrimSpace(query); q != "" {
		args = append(args, "%"+escapeLike(q)+"%")
		where = append(where, `(name || ' ' || coalesce(sku,'') || ' ' || coalesce(description,'')) ILIKE $1`)
	}
	if !includeInactive {
		where = append(where, "active")
	}
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, `SELECT `+productCols+` FROM products WHERE `+strings.Join(where, " AND ")+
		` ORDER BY active DESC, name LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Product, error) { return scanProduct(r) })
}

func (s *Service) SaveProduct(ctx context.Context, id *uuid.UUID, in ProductInput) (Product, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return Product{}, httpx.ErrUnauthorized
	}
	in.Name = strings.TrimSpace(in.Name)
	in.SKU, in.Description = clean(in.SKU), clean(in.Description)
	in.Unit = strings.TrimSpace(in.Unit)
	if in.Unit == "" {
		in.Unit = "each"
	}
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.Currency == "" {
		in.Currency = "ZMW"
	}
	if in.Category == "" {
		in.Category = "other"
	}
	if strings.TrimSpace(in.VATRate) == "" {
		in.VATRate = "16"
	}
	fe := httpx.FieldErrors{}
	if in.Name == "" {
		fe.Add("name", "Enter the product or service name.")
	}
	if !productCategories[in.Category] {
		fe.Add("category", "Choose a category.")
	}
	if !currencyOK(in.Currency) {
		fe.Add("currency", "Use a 3-letter currency code, e.g. ZMW or USD.")
	}
	price, err := parseHundredths(in.UnitPrice)
	if err != nil {
		fe.Add("unit_price", "Enter a price like 12500 or 12500.00.")
	}
	vat, err := parseHundredths(in.VATRate)
	if err != nil || vat > 10000 {
		fe.Add("vat_rate", "Enter a VAT rate between 0 and 100, e.g. 16.")
	}
	if err := fe.Err(); err != nil {
		return Product{}, err
	}

	var out Product
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if id == nil {
			p, err := scanProduct(tx.QueryRow(ctx, `
				INSERT INTO products (sku, name, description, category, unit, unit_price, currency, vat_rate, created_by)
				VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8::numeric,$9) RETURNING `+productCols,
				in.SKU, in.Name, in.Description, in.Category, in.Unit, price.String(), in.Currency, vat.String(), actor.UserID))
			if err != nil {
				return skuTaken(err)
			}
			out = p
			return audit.Record(ctx, tx, audit.Entry{EntityType: productEntity, EntityID: p.ID, Action: "created",
				Summary: "Added to the price list: " + p.Name})
		}
		cur, err := scanProduct(tx.QueryRow(ctx, `SELECT `+productCols+` FROM products WHERE id = $1 FOR UPDATE`, *id))
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Version != in.Version {
			return httpx.ErrStaleVersion.With(map[string]any{"current_version": cur.Version})
		}
		active := cur.Active
		if in.Active != nil {
			active = *in.Active
		}
		p, err := scanProduct(tx.QueryRow(ctx, `
			UPDATE products SET sku=$2, name=$3, description=$4, category=$5, unit=$6, unit_price=$7::numeric, currency=$8,
			       vat_rate=$9::numeric, active=$10, updated_at=now(), version=version+1
			WHERE id = $1 RETURNING `+productCols,
			*id, in.SKU, in.Name, in.Description, in.Category, in.Unit, price.String(), in.Currency, vat.String(), active))
		if err != nil {
			return skuTaken(err)
		}
		changes := audit.Diff(productSnapshot(cur), productSnapshot(p))
		if len(changes) == 0 {
			return errNoChange
		}
		out = p
		return audit.Record(ctx, tx, audit.Entry{EntityType: productEntity, EntityID: p.ID, Action: "updated",
			Summary: "Price list item updated: " + p.Name, Changes: changes})
	})
	return out, err
}

func productSnapshot(p Product) map[string]any {
	return map[string]any{"sku": derefStr(p.SKU), "name": p.Name, "description": derefStr(p.Description), "category": p.Category,
		"unit": p.Unit, "unit_price": p.UnitPrice, "currency": p.Currency, "vat_rate": p.VATRate, "active": p.Active}
}

func skuTaken(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return httpx.FieldErrors{"sku": "Another item already uses this code."}.Err()
	}
	return err
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func trimPct(s string) string {
	h, err := parseHundredths(s)
	if err != nil {
		return s
	}
	return h.Trimmed()
}
