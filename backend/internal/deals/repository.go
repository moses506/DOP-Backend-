package deals

import (
	"context"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dos-bpas/internal/platform/db"
)

// All SQL for the deals module lives in this file. Only this module touches
// the deals and deal_notes tables. Client details come from the clients
// module's API, never from its tables.

const dealSelect = `
	SELECT d.id, d.reference, d.client_id, d.title, d.description, d.stage, d.value::text, d.currency,
	       d.probability, d.expected_close::text, d.source, d.next_step, d.next_step_date::text,
	       d.lost_reason, d.closed_at, d.stage_changed_at,
	       o.id, o.full_name, o.position, cb.id, cb.full_name, cb.position,
	       d.created_at, d.updated_at, d.version
	FROM deals d
	JOIN users o  ON o.id  = d.owner_id
	JOIN users cb ON cb.id = d.created_by`

func scanDeal(row pgx.Row, extra ...any) (Deal, error) {
	var d Deal
	var expected, nextDate *string
	dest := []any{&d.ID, &d.Reference, &d.clientID, &d.Title, &d.Description, &d.Stage, &d.Value, &d.Currency,
		&d.Probability, &expected, &d.Source, &d.NextStep, &nextDate,
		&d.LostReason, &d.ClosedAt, &d.StageChangedAt,
		&d.Owner.ID, &d.Owner.Name, &d.Owner.Position, &d.CreatedBy.ID, &d.CreatedBy.Name, &d.CreatedBy.Position,
		&d.CreatedAt, &d.UpdatedAt, &d.Version}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return d, err
	}
	if expected != nil {
		v := Date(*expected)
		d.ExpectedClose = &v
	}
	if nextDate != nil {
		v := Date(*nextDate)
		d.NextStepDate = &v
	}
	d.Client.ID = d.clientID
	return d, nil
}

func getDeal(ctx context.Context, q db.Querier, id uuid.UUID, forUpdate bool) (Deal, error) {
	sql := dealSelect + ` WHERE d.id = $1`
	if forUpdate {
		sql += ` FOR UPDATE OF d`
	}
	return scanDeal(q.QueryRow(ctx, sql, id))
}

// listDeals returns one page of deals. clientMatches are clients whose name
// matched the search text (looked up through the clients module).
func listDeals(ctx context.Context, q db.Querier, f ListFilter, clientMatches []uuid.UUID) ([]Deal, int, error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if s := strings.TrimSpace(f.Query); s != "" {
		cond := `(d.title || ' ' || d.reference) ILIKE ` + arg("%"+escapeLike(s)+"%")
		if len(clientMatches) > 0 {
			cond += ` OR d.client_id = ANY(` + arg(clientMatches) + `)`
		}
		where = append(where, "("+cond+")")
	}
	switch f.Stage {
	case "":
	case "open":
		where = append(where, `d.stage NOT IN ('won','lost')`)
	case "closed":
		where = append(where, `d.stage IN ('won','lost')`)
	default:
		where = append(where, `d.stage = `+arg(f.Stage))
	}
	if f.OwnerID != nil {
		where = append(where, `d.owner_id = `+arg(*f.OwnerID))
	}
	if f.ClientID != nil {
		where = append(where, `d.client_id = `+arg(*f.ClientID))
	}
	sql := strings.Replace(dealSelect, "d.version\n", "d.version, count(*) OVER ()\n", 1)
	if len(where) > 0 {
		sql += ` WHERE ` + strings.Join(where, " AND ")
	}
	// Open deals first, in pipeline order, soonest close first; then the most recently closed.
	sql += ` ORDER BY (d.stage IN ('won','lost')),
	         array_position(ARRAY['negotiation','proposal','qualified','lead'], d.stage),
	         d.expected_close NULLS LAST, d.closed_at DESC NULLS LAST, d.created_at DESC
	         LIMIT ` + arg(f.Page.Limit) + ` OFFSET ` + arg(f.Page.Offset)

	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Deal{}
	total := 0
	for rows.Next() {
		d, err := scanDeal(rows, &total)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	return out, total, rows.Err()
}

type dealRow struct {
	Reference     string
	ClientID      uuid.UUID
	Title         string
	Description   *string
	Stage         Stage
	Value         *string
	Currency      string
	Probability   int
	ExpectedClose *Date
	Source        *string
	NextStep      *string
	NextStepDate  *Date
	OwnerID       uuid.UUID
	CreatedBy     uuid.UUID
}

func insertDeal(ctx context.Context, q db.Querier, r dealRow) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO deals (reference, client_id, title, description, stage, value, currency, probability,
		                   expected_close, source, next_step, next_step_date, owner_id, created_by)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8,$9::date,$10,$11,$12::date,$13,$14)
		RETURNING id`,
		r.Reference, r.ClientID, r.Title, r.Description, string(r.Stage), r.Value, r.Currency, r.Probability,
		dateArg(r.ExpectedClose), r.Source, r.NextStep, dateArg(r.NextStepDate), r.OwnerID, r.CreatedBy,
	).Scan(&id)
	return id, err
}

// updateDeal saves every editable column and bumps the version.
func updateDeal(ctx context.Context, q db.Querier, d Deal) error {
	_, err := q.Exec(ctx, `
		UPDATE deals SET title=$2, description=$3, stage=$4, value=$5::numeric, currency=$6, probability=$7,
		       expected_close=$8::date, source=$9, next_step=$10, next_step_date=$11::date, lost_reason=$12,
		       closed_at=$13, stage_changed_at=$14, owner_id=$15, updated_at=now(), version=version+1
		WHERE id=$1`,
		d.ID, d.Title, d.Description, string(d.Stage), d.Value, d.Currency, d.Probability,
		dateArg(d.ExpectedClose), d.Source, d.NextStep, dateArg(d.NextStepDate), d.LostReason,
		d.ClosedAt, d.StageChangedAt, d.Owner.ID)
	return err
}

func insertNote(ctx context.Context, q db.Querier, dealID, by uuid.UUID, in NoteInput) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx,
		`INSERT INTO deal_notes (deal_id, kind, body, created_by) VALUES ($1,$2,$3,$4) RETURNING id`,
		dealID, in.Kind, in.Body, by).Scan(&id)
	return id, err
}

func listNotes(ctx context.Context, q db.Querier, dealID uuid.UUID) ([]Note, error) {
	rows, err := q.Query(ctx, `
		SELECT n.id, n.kind, n.body, u.id, u.full_name, u.position, n.created_at
		FROM deal_notes n JOIN users u ON u.id = n.created_by
		WHERE n.deal_id = $1 ORDER BY n.created_at DESC LIMIT 200`, dealID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Note{}
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Kind, &n.Body, &n.CreatedBy.ID, &n.CreatedBy.Name, &n.CreatedBy.Position, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func activeUserExists(ctx context.Context, q db.Querier, id uuid.UUID) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND active)`, id).Scan(&ok)
	return ok, err
}

func dateArg(d *Date) any {
	if d == nil {
		return nil
	}
	return string(*d)
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
