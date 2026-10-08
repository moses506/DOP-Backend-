package clients

import (
	"context"
	"errors"
	"net/http"
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

// The equipment register: the copiers, printers, computers and systems DOS has
// supplied to or looks after at each client, with warranty and contract dates.
// Service jobs, meter readings and contract renewals will build on it.
//
// Equipment is never deleted. Taken out of use, it is marked removed with a
// reason, and its serial number can then be registered again elsewhere.

// ExpiringWithinDays is how far ahead warranties and contracts count as ending soon.
const ExpiringWithinDays = 60

var equipmentCategories = map[string]bool{
	"copier": true, "printer": true, "scanner": true, "computer": true, "server": true,
	"network": true, "projector": true, "phone_system": true, "software": true, "other": true,
}

var contractTypes = map[string]bool{"none": true, "service": true, "rental": true, "lease": true}

type Equipment struct {
	ID            uuid.UUID  `json:"id"`
	ClientID      uuid.UUID  `json:"client_id"`
	Site          *SiteRef   `json:"site"`
	Category      string     `json:"category"`
	Make          string     `json:"make"`
	Model         string     `json:"model"`
	SerialNo      *string    `json:"serial_no"`
	AssetTag      *string    `json:"asset_tag"`
	InstalledOn   *string    `json:"installed_on"`
	WarrantyUntil *string    `json:"warranty_until"`
	Contract      string     `json:"contract"`
	ContractUntil *string    `json:"contract_until"`
	Notes         *string    `json:"notes"`
	Status        string     `json:"status"` // in_use or removed
	RemovedAt     *time.Time `json:"removed_at"`
	RemovedReason *string    `json:"removed_reason"`
	CreatedBy     Person     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	Version       int        `json:"version"`

	// Cover is what protects the equipment today: contract, warranty, expired or none.
	Cover     string  `json:"cover"`
	CoverEnds *string `json:"cover_ends"`
	// EndingSoon is true when the cover ends within ExpiringWithinDays days.
	EndingSoon bool `json:"ending_soon"`
}

type SiteRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type EquipmentInput struct {
	Version       int        `json:"version"` // for updates
	SiteID        *uuid.UUID `json:"site_id"`
	Category      string     `json:"category"`
	Make          string     `json:"make"`
	Model         string     `json:"model"`
	SerialNo      *string    `json:"serial_no"`
	AssetTag      *string    `json:"asset_tag"`
	InstalledOn   *string    `json:"installed_on"`
	WarrantyUntil *string    `json:"warranty_until"`
	Contract      string     `json:"contract"`
	ContractUntil *string    `json:"contract_until"`
	Notes         *string    `json:"notes"`
}

// SerialKey normalises a serial number for matching: upper case, no spaces or dashes.
func SerialKey(s string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "/", "", ".", "").Replace(strings.TrimSpace(s)))
}

// ---- queries ----

func (s *Service) Equipment(ctx context.Context, clientID uuid.UUID) ([]Equipment, error) {
	if _, err := getClient(ctx, s.pool, clientID, false); db.IsNoRows(err) {
		return nil, httpx.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	items, err := listEquipment(ctx, s.pool, `e.client_id = $1`, false, clientID)
	if err != nil {
		return nil, err
	}
	for i := range items {
		s.setCover(&items[i])
	}
	return items, nil
}

// ---- commands ----

func (s *Service) AddEquipment(ctx context.Context, clientID uuid.UUID, in EquipmentInput) (Equipment, error) {
	actor, ok := auth.FromContext(ctx)
	if !ok {
		return Equipment{}, httpx.ErrUnauthorized
	}
	row, err := s.validateEquipment(in)
	if err != nil {
		return Equipment{}, err
	}
	var id uuid.UUID
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		c, err := getClient(ctx, tx, clientID, true)
		if db.IsNoRows(err) {
			return httpx.ErrNotFound
		}
		if err != nil {
			return err
		}
		if c.Status != StatusActive {
			return ErrInactive
		}
		if err := checkEquipmentRefs(ctx, tx, clientID, row, nil); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO client_equipment (client_id, site_id, category, make, model, serial_no, serial_key, asset_tag,
			                              installed_on, warranty_until, contract, contract_until, notes, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
			clientID, row.SiteID, row.Category, row.Make, row.Model, row.SerialNo, serialKeyOf(row.SerialNo), row.AssetTag,
			row.installed, row.warranty, row.Contract, row.contractEnd, row.Notes, actor.UserID).Scan(&id)
		if err != nil {
			return serialTaken(err)
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: entityType, EntityID: clientID, Action: "equipment_added",
			Summary: "Equipment added: " + equipmentName(row.Make, row.Model, row.SerialNo)})
	})
	if err != nil {
		return Equipment{}, err
	}
	return s.oneEquipment(ctx, id)
}

func (s *Service) UpdateEquipment(ctx context.Context, clientID, id uuid.UUID, in EquipmentInput) (Equipment, error) {
	if in.Version < 1 {
		return Equipment{}, httpx.FieldErrors{"version": "Version is required."}.Err()
	}
	row, err := s.validateEquipment(in)
	if err != nil {
		return Equipment{}, err
	}
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := lockEquipment(ctx, tx, clientID, id)
		if err != nil {
			return err
		}
		if cur.Status != "in_use" {
			return httpx.NewError(http.StatusConflict, "equipment_removed", "This equipment has been removed. It can't be changed.")
		}
		if cur.Version != in.Version {
			return httpx.ErrStaleVersion.With(map[string]any{"current_version": cur.Version})
		}
		if err := checkEquipmentRefs(ctx, tx, clientID, row, &id); err != nil {
			return err
		}
		before := equipmentSnapshot(cur)
		var after Equipment
		if row.SiteID != nil {
			after.Site = &SiteRef{ID: *row.SiteID}
		}
		after.Category, after.Make, after.Model, after.SerialNo, after.AssetTag = row.Category, row.Make, row.Model, row.SerialNo, row.AssetTag
		after.InstalledOn, after.WarrantyUntil, after.Contract, after.ContractUntil, after.Notes =
			dayString(row.installed), dayString(row.warranty), row.Contract, dayString(row.contractEnd), row.Notes
		changes := audit.Diff(before, equipmentSnapshot(after))
		if len(changes) == 0 {
			return errNoChange
		}
		if _, err := tx.Exec(ctx, `
			UPDATE client_equipment SET site_id = $2, category = $3, make = $4, model = $5, serial_no = $6, serial_key = $7,
			       asset_tag = $8, installed_on = $9, warranty_until = $10, contract = $11, contract_until = $12, notes = $13,
			       updated_at = now(), version = version + 1
			WHERE id = $1`,
			id, row.SiteID, row.Category, row.Make, row.Model, row.SerialNo, serialKeyOf(row.SerialNo), row.AssetTag,
			row.installed, row.warranty, row.Contract, row.contractEnd, row.Notes); err != nil {
			return serialTaken(err)
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: entityType, EntityID: clientID, Action: "equipment_updated",
			Summary: "Equipment updated: " + equipmentName(row.Make, row.Model, row.SerialNo), Changes: changes})
	})
	if err != nil {
		return Equipment{}, err
	}
	return s.oneEquipment(ctx, id)
}

// RemoveEquipment marks equipment as no longer in use (returned, replaced, written off).
func (s *Service) RemoveEquipment(ctx context.Context, clientID, id uuid.UUID, version int, reason string) (Equipment, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Equipment{}, httpx.FieldErrors{"reason": "Say why it's being removed, e.g. returned, replaced or written off."}.Err()
	}
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := lockEquipment(ctx, tx, clientID, id)
		if err != nil {
			return err
		}
		if cur.Status != "in_use" {
			return errNoChange
		}
		if cur.Version != version {
			return httpx.ErrStaleVersion.With(map[string]any{"current_version": cur.Version})
		}
		if _, err := tx.Exec(ctx, `UPDATE client_equipment SET status = 'removed', removed_at = now(), removed_reason = $2,
			updated_at = now(), version = version + 1 WHERE id = $1`, id, reason); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{EntityType: entityType, EntityID: clientID, Action: "equipment_removed",
			Summary: "Equipment removed: " + equipmentName(cur.Make, cur.Model, cur.SerialNo), Reason: reason})
	})
	if err != nil {
		return Equipment{}, err
	}
	return s.oneEquipment(ctx, id)
}

// ---- internals ----

type equipmentRow struct {
	EquipmentInput
	installed, warranty, contractEnd *time.Time
}

func (s *Service) validateEquipment(in EquipmentInput) (equipmentRow, error) {
	r := equipmentRow{EquipmentInput: in}
	r.Category = strings.TrimSpace(in.Category)
	r.Make, r.Model = strings.TrimSpace(in.Make), strings.TrimSpace(in.Model)
	r.SerialNo, r.AssetTag, r.Notes = clean(in.SerialNo), clean(in.AssetTag), clean(in.Notes)
	r.Contract = strings.TrimSpace(in.Contract)
	if r.Contract == "" {
		r.Contract = "none"
	}

	fe := httpx.FieldErrors{}
	if !equipmentCategories[r.Category] {
		fe.Add("category", "Choose what kind of equipment this is.")
	}
	if r.Make == "" {
		fe.Add("make", "Enter the make, e.g. Canon or HP.")
	}
	if r.Model == "" {
		fe.Add("model", "Enter the model.")
	}
	if r.SerialNo != nil && SerialKey(*r.SerialNo) == "" {
		r.SerialNo = nil
	}
	if !contractTypes[r.Contract] {
		fe.Add("contract", "Choose none, service, rental or lease.")
	}
	date := func(field string, v *string) *time.Time {
		if v == nil || strings.TrimSpace(*v) == "" {
			return nil
		}
		d, err := parseDay(*v)
		if err != nil {
			fe.Add(field, "Enter a date.")
			return nil
		}
		return &d
	}
	r.installed = date("installed_on", in.InstalledOn)
	r.warranty = date("warranty_until", in.WarrantyUntil)
	r.contractEnd = date("contract_until", in.ContractUntil)
	if r.installed != nil && r.installed.After(s.today()) {
		fe.Add("installed_on", "The install date can't be in the future.")
	}
	if r.installed != nil && r.warranty != nil && r.warranty.Before(*r.installed) {
		fe.Add("warranty_until", "The warranty can't end before it was installed.")
	}
	if r.Contract != "none" && r.contractEnd == nil && fe["contract_until"] == "" {
		fe.Add("contract_until", "Enter when the contract ends.")
	}
	if r.Contract == "none" {
		r.contractEnd = nil
	}
	return r, fe.Err()
}

func checkEquipmentRefs(ctx context.Context, tx pgx.Tx, clientID uuid.UUID, r equipmentRow, self *uuid.UUID) error {
	fe := httpx.FieldErrors{}
	if r.SiteID != nil {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM client_sites WHERE id = $1 AND client_id = $2 AND active)`,
			*r.SiteID, clientID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			fe.Add("site_id", "Choose one of this client's sites.")
		}
	}
	if r.SerialNo != nil {
		var ref, name string
		err := tx.QueryRow(ctx, `
			SELECT c.reference, c.legal_name FROM client_equipment e JOIN clients c ON c.id = e.client_id
			WHERE e.serial_key = $1 AND e.status = 'in_use' AND ($2::uuid IS NULL OR e.id <> $2)`,
			SerialKey(*r.SerialNo), self).Scan(&ref, &name)
		if err == nil {
			fe.Add("serial_no", "Already registered at "+ref+" – "+name+". If it has moved, mark it removed there first.")
		} else if !db.IsNoRows(err) {
			return err
		}
	}
	return fe.Err()
}

// serialTaken turns a race on the unique serial index into a field error.
func serialTaken(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "client_equipment_serial_uq" {
		return httpx.FieldErrors{"serial_no": "This serial number is already registered to another client."}.Err()
	}
	return err
}

func lockEquipment(ctx context.Context, tx pgx.Tx, clientID, id uuid.UUID) (Equipment, error) {
	items, err := listEquipment(ctx, tx, `e.id = $1 AND e.client_id = $2`, true, id, clientID)
	if err != nil {
		return Equipment{}, err
	}
	if len(items) == 0 {
		return Equipment{}, httpx.ErrNotFound
	}
	return items[0], nil
}

func (s *Service) oneEquipment(ctx context.Context, id uuid.UUID) (Equipment, error) {
	items, err := listEquipment(ctx, s.pool, `e.id = $1`, false, id)
	if err != nil {
		return Equipment{}, err
	}
	if len(items) == 0 {
		return Equipment{}, httpx.ErrNotFound
	}
	s.setCover(&items[0])
	return items[0], nil
}

func listEquipment(ctx context.Context, q db.Querier, where string, lock bool, args ...any) ([]Equipment, error) {
	tail := ` ORDER BY e.status, e.category, e.make, e.model`
	if lock {
		tail = ` FOR UPDATE OF e`
	}
	rows, err := q.Query(ctx, `
		SELECT e.id, e.client_id, st.id, st.name, e.category, e.make, e.model, e.serial_no, e.asset_tag,
		       to_char(e.installed_on, 'YYYY-MM-DD'), to_char(e.warranty_until, 'YYYY-MM-DD'), e.contract,
		       to_char(e.contract_until, 'YYYY-MM-DD'), e.notes, e.status, e.removed_at, e.removed_reason,
		       u.id, u.full_name, u.position, e.created_at, e.updated_at, e.version
		FROM client_equipment e
		JOIN users u ON u.id = e.created_by
		LEFT JOIN client_sites st ON st.id = e.site_id
		WHERE `+where+tail, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Equipment, error) {
		var e Equipment
		var siteID *uuid.UUID
		var siteName *string
		err := r.Scan(&e.ID, &e.ClientID, &siteID, &siteName, &e.Category, &e.Make, &e.Model, &e.SerialNo, &e.AssetTag,
			&e.InstalledOn, &e.WarrantyUntil, &e.Contract, &e.ContractUntil, &e.Notes, &e.Status, &e.RemovedAt, &e.RemovedReason,
			&e.CreatedBy.ID, &e.CreatedBy.Name, &e.CreatedBy.Position, &e.CreatedAt, &e.UpdatedAt, &e.Version)
		if siteID != nil {
			e.Site = &SiteRef{ID: *siteID, Name: deref(siteName)}
		}
		return e, err
	})
}

// setCover works out what protects the equipment today and when that ends.
func (s *Service) setCover(e *Equipment) {
	today := s.today().Format("2006-01-02")
	soon := s.today().AddDate(0, 0, ExpiringWithinDays).Format("2006-01-02")
	e.Cover, e.CoverEnds, e.EndingSoon = "none", nil, false
	if e.Status != "in_use" {
		return
	}
	switch {
	case e.Contract != "none" && e.ContractUntil != nil && *e.ContractUntil >= today:
		e.Cover, e.CoverEnds = "contract", e.ContractUntil
	case e.WarrantyUntil != nil && *e.WarrantyUntil >= today:
		e.Cover, e.CoverEnds = "warranty", e.WarrantyUntil
	case e.ContractUntil != nil || e.WarrantyUntil != nil:
		e.Cover = "expired"
		e.CoverEnds = e.ContractUntil
		if e.CoverEnds == nil || (e.WarrantyUntil != nil && *e.WarrantyUntil > *e.CoverEnds) {
			e.CoverEnds = e.WarrantyUntil
		}
	}
	if (e.Cover == "contract" || e.Cover == "warranty") && *e.CoverEnds <= soon {
		e.EndingSoon = true
	}
}

func equipmentSnapshot(e Equipment) map[string]any {
	site := ""
	if e.Site != nil {
		site = e.Site.ID.String()
	}
	str := func(s *string) any {
		if s == nil {
			return nil
		}
		return *s
	}
	return map[string]any{
		"site_id": site, "category": e.Category, "make": e.Make, "model": e.Model,
		"serial_no": str(e.SerialNo), "asset_tag": str(e.AssetTag), "installed_on": str(e.InstalledOn),
		"warranty_until": str(e.WarrantyUntil), "contract": e.Contract, "contract_until": str(e.ContractUntil),
		"notes": str(e.Notes),
	}
}

func equipmentName(make, model string, serial *string) string {
	n := make + " " + model
	if serial != nil {
		n += " (S/N " + *serial + ")"
	}
	return n
}

func serialKeyOf(s *string) *string {
	if s == nil {
		return nil
	}
	k := SerialKey(*s)
	return &k
}

func dayString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format("2006-01-02")
	return &s
}
