package clients

import (
	"context"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

// Summary is the client portfolio at a glance, for the dashboard.
type Summary struct {
	Active        int `json:"active"`
	Inactive      int `json:"inactive"`
	Mine          int `json:"mine"` // active clients the signed-in user is responsible for
	NewThisMonth  int `json:"new_this_month"`
	NewLastMonth  int `json:"new_last_month"`
	WithContact   int `json:"with_contact"` // active clients with at least one active contact
	WithoutSector int `json:"without_sector"`

	// Contact log and equipment.
	FollowUpsDue     int `json:"follow_ups_due"`     // the signed-in user's, due today or earlier
	FollowUpsOverdue int `json:"follow_ups_overdue"` // …of which overdue
	NotContacted     int `json:"not_contacted"`      // active clients not contacted in ColdAfterDays days, or never
	NotContactedMine int `json:"not_contacted_mine"`
	CoverEndingSoon  int `json:"cover_ending_soon"` // equipment whose warranty or contract ends within ExpiringWithinDays days

	ByMonth  []MonthCount  `json:"by_month"`  // registrations, last 12 months, oldest first
	BySector []NamedCount  `json:"by_sector"` // active clients, largest first; tail folded into "Other"
	ByOwner  []OwnerCount  `json:"by_owner"`  // active clients per responsible person
	Recent   []RecentEntry `json:"recent"`    // newest registrations
	Activity []ActivityRow `json:"activity"`  // latest changes to any client
}

type MonthCount struct {
	Month string `json:"month"` // YYYY-MM
	Count int    `json:"count"`
}

type NamedCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type OwnerCount struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Position string    `json:"position"`
	Count    int       `json:"count"`
}

type RecentEntry struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	LegalName string    `json:"legal_name"`
	Sector    *string   `json:"sector"`
	OwnerName string    `json:"owner_name"`
	CreatedAt time.Time `json:"created_at"`
}

type ActivityRow struct {
	ClientID   uuid.UUID `json:"client_id"`
	Action     string    `json:"action"`
	Summary    string    `json:"summary"`
	ActorName  string    `json:"actor_name"`
	OccurredAt time.Time `json:"occurred_at"`
}

const topSectors = 7

// Summary gathers the dashboard figures. The queries are independent, so they run in parallel.
func (s *Service) Summary(ctx context.Context, me uuid.UUID) (Summary, error) {
	out := Summary{ByMonth: []MonthCount{}, BySector: []NamedCount{}, ByOwner: []OwnerCount{},
		Recent: []RecentEntry{}, Activity: []ActivityRow{}}
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		return s.pool.QueryRow(ctx, `
			WITH bounds AS (
			  SELECT date_trunc('month', now() AT TIME ZONE $2) AT TIME ZONE $2 AS this_month,
			         date_trunc('month', (now() AT TIME ZONE $2) - interval '1 month') AT TIME ZONE $2 AS last_month
			)
			SELECT
			  count(*) FILTER (WHERE status = 'active'),
			  count(*) FILTER (WHERE status = 'inactive'),
			  count(*) FILTER (WHERE status = 'active' AND owner_id = $1),
			  count(*) FILTER (WHERE created_at >= b.this_month),
			  count(*) FILTER (WHERE created_at >= b.last_month AND created_at < b.this_month),
			  count(*) FILTER (WHERE status = 'active' AND EXISTS (
			      SELECT 1 FROM client_contacts k WHERE k.client_id = c.id AND k.active)),
			  count(*) FILTER (WHERE status = 'active' AND coalesce(trim(sector), '') = '')
			FROM clients c, bounds b`, me, s.tz).
			Scan(&out.Active, &out.Inactive, &out.Mine, &out.NewThisMonth, &out.NewLastMonth,
				&out.WithContact, &out.WithoutSector)
	})

	g.Go(func() error {
		return s.pool.QueryRow(ctx, `
			SELECT
			  (SELECT count(*) FROM client_interactions i JOIN clients c ON c.id = i.client_id
			   WHERE i.follow_up_owner_id = $1 AND i.follow_up_done_at IS NULL AND c.status = 'active'
			     AND i.follow_up_on <= (now() AT TIME ZONE $2)::date),
			  (SELECT count(*) FROM client_interactions i JOIN clients c ON c.id = i.client_id
			   WHERE i.follow_up_owner_id = $1 AND i.follow_up_done_at IS NULL AND c.status = 'active'
			     AND i.follow_up_on < (now() AT TIME ZONE $2)::date),
			  (SELECT count(*) FROM clients WHERE status = 'active'
			     AND (last_contact_at IS NULL OR last_contact_at < now() - make_interval(days => $3))),
			  (SELECT count(*) FROM clients WHERE status = 'active' AND owner_id = $1
			     AND (last_contact_at IS NULL OR last_contact_at < now() - make_interval(days => $3))),
			  (SELECT count(*) FROM client_equipment e JOIN clients c ON c.id = e.client_id
			   WHERE e.status = 'in_use' AND c.status = 'active'
			     AND greatest(e.warranty_until, CASE WHEN e.contract <> 'none' THEN e.contract_until END)
			         BETWEEN (now() AT TIME ZONE $2)::date AND (now() AT TIME ZONE $2)::date + $4::int)`,
			me, s.tz, ColdAfterDays, ExpiringWithinDays).
			Scan(&out.FollowUpsDue, &out.FollowUpsOverdue, &out.NotContacted, &out.NotContactedMine, &out.CoverEndingSoon)
	})

	g.Go(func() error {
		rows, err := s.pool.Query(ctx, `
			WITH months AS (
			  SELECT generate_series(
			    date_trunc('month', now() AT TIME ZONE $1) - interval '11 months',
			    date_trunc('month', now() AT TIME ZONE $1), interval '1 month') AS m
			)
			SELECT to_char(m.m, 'YYYY-MM'), count(c.id)
			FROM months m
			LEFT JOIN clients c ON date_trunc('month', c.created_at AT TIME ZONE $1) = m.m
			GROUP BY m.m ORDER BY m.m`, s.tz)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var mc MonthCount
			if err := rows.Scan(&mc.Month, &mc.Count); err != nil {
				return err
			}
			out.ByMonth = append(out.ByMonth, mc)
		}
		return rows.Err()
	})

	g.Go(func() error {
		rows, err := s.pool.Query(ctx, `
			SELECT coalesce(nullif(trim(sector), ''), 'Not set') AS name, count(*) AS n
			FROM clients WHERE status = 'active'
			GROUP BY 1 ORDER BY n DESC, name`)
		if err != nil {
			return err
		}
		defer rows.Close()
		other := 0
		for rows.Next() {
			var nc NamedCount
			if err := rows.Scan(&nc.Name, &nc.Count); err != nil {
				return err
			}
			if len(out.BySector) < topSectors {
				out.BySector = append(out.BySector, nc)
			} else {
				other += nc.Count
			}
		}
		if other > 0 {
			out.BySector = append(out.BySector, NamedCount{Name: "Other", Count: other})
		}
		return rows.Err()
	})

	g.Go(func() error {
		rows, err := s.pool.Query(ctx, `
			SELECT u.id, u.full_name, u.position, count(*) AS n
			FROM clients c JOIN users u ON u.id = c.owner_id
			WHERE c.status = 'active'
			GROUP BY u.id ORDER BY n DESC, u.full_name LIMIT 8`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var oc OwnerCount
			if err := rows.Scan(&oc.ID, &oc.Name, &oc.Position, &oc.Count); err != nil {
				return err
			}
			out.ByOwner = append(out.ByOwner, oc)
		}
		return rows.Err()
	})

	g.Go(func() error {
		rows, err := s.pool.Query(ctx, `
			SELECT c.id, c.reference, c.legal_name, c.sector, u.full_name, c.created_at
			FROM clients c JOIN users u ON u.id = c.owner_id
			ORDER BY c.created_at DESC, c.reference DESC LIMIT 6`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r RecentEntry
			if err := rows.Scan(&r.ID, &r.Reference, &r.LegalName, &r.Sector, &r.OwnerName, &r.CreatedAt); err != nil {
				return err
			}
			out.Recent = append(out.Recent, r)
		}
		return rows.Err()
	})

	g.Go(func() error {
		rows, err := s.pool.Query(ctx, `
			SELECT entity_id, action, summary, actor_name, occurred_at
			FROM audit_log WHERE entity_type = $1
			ORDER BY occurred_at DESC, id DESC LIMIT 8`, entityType)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a ActivityRow
			if err := rows.Scan(&a.ClientID, &a.Action, &a.Summary, &a.ActorName, &a.OccurredAt); err != nil {
				return err
			}
			out.Activity = append(out.Activity, a)
		}
		return rows.Err()
	})

	if err := g.Wait(); err != nil {
		return Summary{}, err
	}
	return out, nil
}
