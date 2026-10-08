package quotes

import (
	"context"

	"dos-bpas/internal/platform/auth"
)

// Summary is the quotation pipeline at a glance, for the list page and dashboard.
type Summary struct {
	Draft           int `json:"draft"`
	PendingApproval int `json:"pending_approval"`
	Ready           int `json:"ready"`
	Sent            int `json:"sent"`
	ExpiringSoon    int `json:"expiring_soon"` // sent, validity ends within 7 days
	// Values by currency, as decimals in strings.
	SentValue         map[string]string `json:"sent_value"`
	AcceptedThisMonth int               `json:"accepted_this_month"`
	AcceptedValue     map[string]string `json:"accepted_value"`
	// WinRate is accepted ÷ (accepted + declined + expired) over the last 90 days, in percent; -1 when there's nothing to measure.
	WinRate int `json:"win_rate"`
	// Mine counts the signed-in person's open quotations; AwaitingMe the approvals waiting for them.
	Mine       int `json:"mine"`
	AwaitingMe int `json:"awaiting_me"`
}

func (s *Service) Summary(ctx context.Context) (Summary, error) {
	p, _ := auth.FromContext(ctx)
	out := Summary{SentValue: map[string]string{}, AcceptedValue: map[string]string{}, WinRate: -1}
	tz := s.loc.String()
	var won, decided int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'draft'),
		       count(*) FILTER (WHERE status = 'pending_approval'),
		       count(*) FILTER (WHERE status = 'ready'),
		       count(*) FILTER (WHERE status = 'sent'),
		       count(*) FILTER (WHERE status = 'sent' AND valid_until <= (now() AT TIME ZONE $1)::date + 7),
		       count(*) FILTER (WHERE status = 'accepted' AND decided_at >= date_trunc('month', now() AT TIME ZONE $1) AT TIME ZONE $1),
		       count(*) FILTER (WHERE status = 'accepted' AND decided_at > now() - interval '90 days'),
		       count(*) FILTER (WHERE status IN ('accepted','declined','expired') AND updated_at > now() - interval '90 days'),
		       count(*) FILTER (WHERE owner_id = $2 AND status IN ('draft','pending_approval','ready','sent'))
		FROM quotes`, tz, p.UserID).
		Scan(&out.Draft, &out.PendingApproval, &out.Ready, &out.Sent, &out.ExpiringSoon, &out.AcceptedThisMonth, &won, &decided, &out.Mine)
	if err != nil {
		return out, err
	}
	if decided > 0 {
		out.WinRate = (won*100 + decided/2) / decided
	}
	if p.Can(PermApprove) {
		out.AwaitingMe = out.PendingApproval
	}
	rows, err := s.pool.Query(ctx, `
		SELECT 'sent', currency, sum(total)::text FROM quotes WHERE status = 'sent' GROUP BY currency
		UNION ALL
		SELECT 'accepted', currency, sum(total)::text FROM quotes
		WHERE status = 'accepted' AND decided_at >= date_trunc('month', now() AT TIME ZONE $1) AT TIME ZONE $1 GROUP BY currency`, tz)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, cur, v string
		if err := rows.Scan(&kind, &cur, &v); err != nil {
			return out, err
		}
		if kind == "sent" {
			out.SentValue[cur] = v
		} else {
			out.AcceptedValue[cur] = v
		}
	}
	return out, rows.Err()
}
