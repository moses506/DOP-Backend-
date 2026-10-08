package orders

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"dos-bpas/internal/platform/notify"
)

// Reminders keep promised delivery dates from slipping by unnoticed:
//
//   - the order's owner is told DueSoonDays before the promised date if
//     anything is still to deliver, and again when the date is missed;
//   - EscalateAfter days after a missed date, the owner's manager is told
//     (or the sales managers, when the owner has no active manager).
//
// Orders on hold are skipped: someone has already decided to wait. Each
// notice has a key, so it is sent once however often the job runs.
const (
	DueSoonDays   = 2
	EscalateAfter = 3
)

type Reminders struct{ svc *Service }

func NewReminders(s *Service) *Reminders { return &Reminders{svc: s} }

type lateOrder struct {
	id               uuid.UUID
	ref, client, due string
	daysLate         int
	ownerID          uuid.UUID
	ownerName        string
	ownerActive      bool
	managerID        *uuid.UUID
	deliveredPct     int
}

// Run checks every live order once, as at now. It returns how many notices it created.
func (r *Reminders) Run(ctx context.Context, now time.Time) (int, error) {
	s := r.svc
	local := now.In(s.loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	horizon := today.AddDate(0, 0, DueSoonDays)
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.reference, cl.legal_name, o.delivery_due, ow.id, ow.full_name, ow.active, mg.id,
		       coalesce((SELECT floor(coalesce(sum(l.line_net * l.qty_delivered / l.quantity) * 100 / nullif(sum(l.line_net), 0),
		                 sum(l.qty_delivered) * 100 / nullif(sum(l.quantity), 0)))::int FROM customer_order_lines l WHERE l.order_id = o.id), 0)
		FROM customer_orders o
		JOIN clients cl ON cl.id = o.client_id
		JOIN users ow ON ow.id = o.owner_id
		LEFT JOIN users mg ON mg.id = ow.manager_id AND mg.active AND mg.id <> ow.id
		WHERE o.status IN ('open','in_progress','ready','part_delivered') AND o.delivery_due <= $1::date
		ORDER BY o.delivery_due`, horizon.Format("2006-01-02"))
	if err != nil {
		return 0, err
	}
	var list []lateOrder
	for rows.Next() {
		var l lateOrder
		var due time.Time
		if err := rows.Scan(&l.id, &l.ref, &l.client, &due, &l.ownerID, &l.ownerName, &l.ownerActive, &l.managerID, &l.deliveredPct); err != nil {
			rows.Close()
			return 0, err
		}
		due = time.Date(due.Year(), due.Month(), due.Day(), 0, 0, 0, 0, time.UTC)
		l.due = due.Format("2006-01-02")
		l.daysLate = int(today.Sub(due).Hours() / 24)
		list = append(list, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var managers []uuid.UUID
	if mrows, err := s.pool.Query(ctx, `SELECT id FROM users WHERE active AND (role = 'sales_manager'
		OR (role = 'admin' AND NOT EXISTS (SELECT 1 FROM users WHERE active AND role = 'sales_manager')))`); err == nil {
		for mrows.Next() {
			var id uuid.UUID
			if mrows.Scan(&id) == nil {
				managers = append(managers, id)
			}
		}
		mrows.Close()
	}

	n := 0
	add := func(x notify.Notification) error {
		ok, err := notify.Create(ctx, s.pool, x)
		if ok {
			n++
		}
		return err
	}
	for _, l := range list {
		id := l.id
		link := "/orders/" + l.id.String()
		left := "nothing delivered yet"
		if l.deliveredPct > 0 {
			left = fmt.Sprintf("%d%% delivered so far", l.deliveredPct)
		}
		if l.ownerActive {
			x := notify.Notification{UserID: l.ownerID, Link: link, EntityType: entityType, EntityID: &id}
			if l.daysLate <= 0 {
				x.Kind, x.Title = KindDueSoon, "Delivery due "+whenDue(-l.daysLate)+": "+l.ref
				x.Body = l.client + " — " + left
				x.DedupeKey = fmt.Sprintf("%s:%s:%s", KindDueSoon, l.id, l.due)
			} else {
				x.Kind, x.Title = KindOverdue, "Delivery date missed: "+l.ref
				x.Body = l.client + " was promised " + dayLabel(l.due) + " — " + left + ". Deliver, or move the date and tell the client."
				x.DedupeKey = fmt.Sprintf("%s:%s:%s", KindOverdue, l.id, l.due)
			}
			if err := add(x); err != nil {
				return n, err
			}
		}
		if l.daysLate >= EscalateAfter || (l.daysLate > 0 && !l.ownerActive) {
			to := managers
			if l.ownerActive && l.managerID != nil {
				to = []uuid.UUID{*l.managerID}
			}
			for _, m := range to {
				if m == l.ownerID {
					continue
				}
				if err := add(notify.Notification{UserID: m, Kind: KindEscalated, Link: link, EntityType: entityType, EntityID: &id,
					Title:     fmt.Sprintf("Order %d days late: %s", l.daysLate, l.ref),
					Body:      l.client + " · " + l.ownerName + "'s order — " + left,
					DedupeKey: fmt.Sprintf("%s:%s:%s:%s", KindEscalated, l.id, l.due, m)}); err != nil {
					return n, err
				}
			}
		}
	}
	return n, nil
}

func whenDue(days int) string {
	switch days {
	case 0:
		return "today"
	case 1:
		return "tomorrow"
	default:
		return fmt.Sprintf("in %d days", days)
	}
}

// Start runs the reminders shortly after start-up and then every interval.
func (r *Reminders) Start(ctx context.Context, interval time.Duration) {
	go func() {
		timer := time.NewTimer(35 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			n, err := r.Run(ctx, time.Now())
			if err != nil && ctx.Err() == nil {
				r.svc.log.Error("order reminders failed", "err", err)
			} else if n > 0 {
				r.svc.log.Info("order reminders sent", "notifications", n)
			}
			timer.Reset(interval)
		}
	}()
}
