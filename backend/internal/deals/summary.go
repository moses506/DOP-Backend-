package deals

import (
	"context"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

// Money is a set of amounts by currency, as decimal text: {"ZMW": "1250000.00"}.
type Money map[string]string

type StageTotal struct {
	Stage    Stage `json:"stage"`
	Count    int   `json:"count"`
	Value    Money `json:"value"`
	Weighted Money `json:"weighted"` // value × probability
}

type Summary struct {
	Open              []StageTotal `json:"open"` // one per open stage, in pipeline order
	OpenCount         int          `json:"open_count"`
	OpenValue         Money        `json:"open_value"`
	Weighted          Money        `json:"weighted"`
	WonThisMonth      int          `json:"won_this_month"`
	WonValueThisMonth Money        `json:"won_value_this_month"`
	LostThisMonth     int          `json:"lost_this_month"`
	Won90             int          `json:"won_90_days"`
	Lost90            int          `json:"lost_90_days"`
	ClosingThisMonth  int          `json:"closing_this_month"`
	OverdueNextSteps  int          `json:"overdue_next_steps"` // open deals whose follow-up date has passed
	PastExpectedClose int          `json:"past_expected_close"`
	Quiet30           int          `json:"quiet_30_days"` // open deals with no activity for 30 days
	MyOverdue         int          `json:"my_overdue"`    // the caller's own overdue follow-ups, for the sidebar
}

// Summary returns pipeline figures, for everyone's deals or one owner's.
func (s *Service) Summary(ctx context.Context, me uuid.UUID, owner *uuid.UUID) (Summary, error) {
	out := Summary{OpenValue: Money{}, Weighted: Money{}, WonValueThisMonth: Money{}}
	byStage := map[Stage]*StageTotal{}
	for _, st := range OpenStages {
		out.Open = append(out.Open, StageTotal{Stage: st, Value: Money{}, Weighted: Money{}})
	}
	for i := range out.Open {
		byStage[out.Open[i].Stage] = &out.Open[i]
	}

	// $1 owner filter (NULL = everyone), $2 business timezone, $3 the caller.
	const ownerCond = `($1::uuid IS NULL OR owner_id = $1)`
	const today = `(now() AT TIME ZONE $2)::date`
	const monthStart = `(date_trunc('month', now() AT TIME ZONE $2) AT TIME ZONE $2)`
	args := []any{owner, s.tz}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		rows, err := s.pool.Query(gctx, `
			SELECT stage, currency, count(*), coalesce(sum(value),0)::text,
			       coalesce(round(sum(value * probability / 100.0), 2),0)::text
			FROM deals WHERE stage NOT IN ('won','lost') AND `+ownerCond+` AND $2::text IS NOT NULL
			GROUP BY stage, currency`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var st Stage
			var cur, val, wt string
			var n int
			if err := rows.Scan(&st, &cur, &n, &val, &wt); err != nil {
				return err
			}
			if t := byStage[st]; t != nil {
				t.Count += n
				t.Value[cur], t.Weighted[cur] = val, wt
			}
		}
		return rows.Err()
	})
	g.Go(func() error {
		rows, err := s.pool.Query(gctx, `
			SELECT currency, count(*), coalesce(sum(value),0)::text, coalesce(round(sum(value * probability / 100.0), 2),0)::text
			FROM deals WHERE stage NOT IN ('won','lost') AND `+ownerCond+` AND $2::text IS NOT NULL
			GROUP BY currency`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cur, val, wt string
			var n int
			if err := rows.Scan(&cur, &n, &val, &wt); err != nil {
				return err
			}
			out.OpenCount += n
			out.OpenValue[cur], out.Weighted[cur] = val, wt
		}
		return rows.Err()
	})
	g.Go(func() error {
		rows, err := s.pool.Query(gctx, `
			SELECT currency, count(*), coalesce(sum(value),0)::text
			FROM deals WHERE stage = 'won' AND closed_at >= `+monthStart+` AND `+ownerCond+`
			GROUP BY currency`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var cur, val string
			var n int
			if err := rows.Scan(&cur, &n, &val); err != nil {
				return err
			}
			out.WonThisMonth += n
			out.WonValueThisMonth[cur] = val
		}
		return rows.Err()
	})
	g.Go(func() error {
		return s.pool.QueryRow(gctx, `
			SELECT
			  count(*) FILTER (WHERE stage = 'lost' AND closed_at >= `+monthStart+`),
			  count(*) FILTER (WHERE stage = 'won'  AND closed_at >= now() - interval '90 days'),
			  count(*) FILTER (WHERE stage = 'lost' AND closed_at >= now() - interval '90 days'),
			  count(*) FILTER (WHERE stage NOT IN ('won','lost') AND expected_close >= date_trunc('month', `+today+`)
			                   AND expected_close < date_trunc('month', `+today+`) + interval '1 month'),
			  count(*) FILTER (WHERE stage NOT IN ('won','lost') AND next_step_date < `+today+`),
			  count(*) FILTER (WHERE stage NOT IN ('won','lost') AND expected_close < `+today+`),
			  count(*) FILTER (WHERE stage NOT IN ('won','lost') AND updated_at < now() - interval '30 days')
			FROM deals WHERE `+ownerCond, args...).
			Scan(&out.LostThisMonth, &out.Won90, &out.Lost90, &out.ClosingThisMonth,
				&out.OverdueNextSteps, &out.PastExpectedClose, &out.Quiet30)
	})
	g.Go(func() error {
		return s.pool.QueryRow(gctx, `
			SELECT count(*) FROM deals
			WHERE owner_id = $2 AND stage NOT IN ('won','lost') AND next_step_date < (now() AT TIME ZONE $1)::date`,
			s.tz, me).Scan(&out.MyOverdue)
	})
	if err := g.Wait(); err != nil {
		return Summary{}, err
	}
	return out, nil
}
