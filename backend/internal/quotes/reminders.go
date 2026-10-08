package quotes

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"dos-bpas/internal/platform/audit"
	"dos-bpas/internal/platform/notify"
)

// Reminders keep sent quotations from going quiet:
//   - ExpiryNoticeDays before a sent quotation's validity ends, its preparer is
//     reminded to follow up;
//   - once the validity date has passed without a decision, the quotation is
//     marked expired and the preparer is told, so they can revise or close it.
//
// Each notice is stored with a key, so it is created once however often the job runs.
const ExpiryNoticeDays = 3

type Reminders struct{ svc *Service }

func NewReminders(s *Service) *Reminders { return &Reminders{svc: s} }

type RunResult struct {
	Expired       int
	Notifications int
}

func (r *Reminders) Run(ctx context.Context, now time.Time) (RunResult, error) {
	var res RunResult
	s := r.svc
	local := now.In(s.loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC).Format("2006-01-02")

	// Expire.
	rows, err := s.pool.Query(ctx, `
		UPDATE quotes SET status = 'expired', updated_at = now(), version = version + 1
		WHERE status = 'sent' AND valid_until < $1::date
		RETURNING id, reference, revision, title, owner_id, to_char(valid_until, 'YYYY-MM-DD')`, today)
	if err != nil {
		return res, err
	}
	type item struct {
		id      uuid.UUID
		ref     string
		rev     int
		title   string
		owner   uuid.UUID
		validTo string
	}
	var expired []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.ref, &it.rev, &it.title, &it.owner, &it.validTo); err != nil {
			rows.Close()
			return res, err
		}
		expired = append(expired, it)
	}
	rows.Close()
	for _, it := range expired {
		res.Expired++
		num := number(it.ref, it.rev)
		if err := audit.Record(ctx, s.pool, audit.Entry{EntityType: entityType, EntityID: it.id, Action: "expired",
			Summary: "Expired — valid until " + longDate(it.validTo) + " with no decision from the client"}); err != nil {
			return res, err
		}
		id := it.id
		ok, err := notify.Create(ctx, s.pool, notify.Notification{UserID: it.owner, Kind: KindExpired,
			Title: "Quotation expired: " + num, Body: it.title + " — revise it with new dates, or record the client's decision.",
			Link: "/quotations/" + it.id.String(), EntityType: entityType, EntityID: &id,
			DedupeKey: fmt.Sprintf("%s:%s", KindExpired, it.id)})
		if err != nil {
			return res, err
		}
		if ok {
			res.Notifications++
		}
	}

	// Expiring soon.
	rows, err = s.pool.Query(ctx, `
		SELECT q.id, q.reference, q.revision, q.title, q.owner_id, to_char(q.valid_until, 'YYYY-MM-DD')
		FROM quotes q JOIN users u ON u.id = q.owner_id
		WHERE q.status = 'sent' AND u.active AND q.valid_until BETWEEN $1::date AND $1::date + $2::int`, today, ExpiryNoticeDays)
	if err != nil {
		return res, err
	}
	var soon []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.ref, &it.rev, &it.title, &it.owner, &it.validTo); err != nil {
			rows.Close()
			return res, err
		}
		soon = append(soon, it)
	}
	rows.Close()
	for _, it := range soon {
		id := it.id
		ok, err := notify.Create(ctx, s.pool, notify.Notification{UserID: it.owner, Kind: KindExpiring,
			Title: "Follow up: " + number(it.ref, it.rev) + " expires " + longDate(it.validTo),
			Body:  it.title + " — no decision from the client yet.",
			Link:  "/quotations/" + it.id.String(), EntityType: entityType, EntityID: &id,
			DedupeKey: fmt.Sprintf("%s:%s:%s", KindExpiring, it.id, it.validTo)})
		if err != nil {
			return res, err
		}
		if ok {
			res.Notifications++
		}
	}
	return res, nil
}

func (r *Reminders) Start(ctx context.Context, interval time.Duration) {
	go func() {
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			res, err := r.Run(ctx, time.Now())
			if err != nil && ctx.Err() == nil {
				r.svc.log.Error("quotation reminders failed", "err", err)
			} else if res.Expired+res.Notifications > 0 {
				r.svc.log.Info("quotation reminders", "expired", res.Expired, "notifications", res.Notifications)
			}
			timer.Reset(interval)
		}
	}()
}
