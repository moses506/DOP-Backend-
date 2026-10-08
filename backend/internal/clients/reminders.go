package clients

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/notify"
)

// Reminders for the client register, run every few minutes:
//
//   - Follow-ups from the contact log: the person responsible is told (in the
//     app) when one is due today and when it becomes overdue. Their own
//     manager (users.manager_id) is told when one is still undone
//     EscalateAfter days after its date; the sales managers are told instead
//     when the owner has no active manager, and straight away when the owner
//     no longer has access. From DigestHour each morning, each person gets one
//     email listing their client follow-ups.
//   - Team activity: from DigestHour on working days, each manager gets an
//     email of what their people logged on the previous working day (Friday
//     to Sunday on a Monday): visits, calls and meetings, and who checked in.
//   - Equipment cover: the client's owner is told when a warranty or contract
//     ends within NoticeDaysBefore days, and again when it has ended, as these
//     are renewal opportunities.
//
// Each notice is stored with a key so it is created once, however often the
// job runs, and each email is claimed per person per day.
const (
	EscalateAfter    = 3  // days overdue before managers are told
	DigestHour       = 7  // local hour from which morning emails go out
	NoticeDaysBefore = 30 // days before cover ends that the owner is told
)

const (
	KindFollowUpDue       = "client_followup_due"
	KindFollowUpOverdue   = "client_followup_overdue"
	KindFollowUpEscalated = "client_followup_escalated"
	KindFollowUpAssigned  = "client_followup_assigned"
	KindCoverEnding       = "equipment_cover_ending"
	KindCoverEnded        = "equipment_cover_ended"
)

type Reminders struct {
	pool   *pgxpool.Pool
	mail   *mail.Service
	appURL string
	loc    *time.Location
	log    *slog.Logger
}

func NewReminders(pool *pgxpool.Pool, m *mail.Service, appURL string, loc *time.Location, log *slog.Logger) *Reminders {
	if loc == nil {
		loc, _ = time.LoadLocation("Africa/Lusaka")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Reminders{pool: pool, mail: m, appURL: strings.TrimRight(appURL, "/"), loc: loc, log: log}
}

type RunResult struct {
	Notifications int
	Emails        int
}

type person struct {
	ID     uuid.UUID
	Name   string
	Email  string
	Active bool
}

type dueFollowUp struct {
	ID          uuid.UUID
	ClientID    uuid.UUID
	ClientRef   string
	ClientName  string
	What        string
	Due         time.Time
	DaysOverdue int
	Owner       person
	Manager     *person // the owner's manager, when active
}

// Run checks every open follow-up and every warranty and contract once, as at now.
func (r *Reminders) Run(ctx context.Context, now time.Time) (RunResult, error) {
	var res RunResult
	local := now.In(r.loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)

	items, err := r.dueFollowUps(ctx, today)
	if err != nil {
		return res, err
	}
	managers, err := r.managers(ctx)
	if err != nil {
		return res, err
	}

	add := func(n notify.Notification) error {
		ok, err := notify.Create(ctx, r.pool, n)
		if ok {
			res.Notifications++
		}
		return err
	}

	escalated := map[uuid.UUID][]dueFollowUp{}
	for _, f := range items {
		link := "/clients/" + f.ClientID.String()
		due := f.Due.Format("2006-01-02")
		cid := f.ClientID
		if f.Owner.Active {
			n := notify.Notification{UserID: f.Owner.ID, Link: link, EntityType: entityType, EntityID: &cid}
			if f.DaysOverdue == 0 {
				n.Kind, n.Title, n.Body = KindFollowUpDue, "Follow up today: "+f.ClientName, f.What
				n.DedupeKey = fmt.Sprintf("%s:%s:%s", KindFollowUpDue, f.ID, due)
			} else {
				n.Kind, n.Title = KindFollowUpOverdue, "Overdue follow-up: "+f.ClientName
				n.Body = f.What + " — was due " + f.Due.Format("2 Jan")
				n.DedupeKey = fmt.Sprintf("%s:%s:%s", KindFollowUpOverdue, f.ID, due)
			}
			if err := add(n); err != nil {
				return res, err
			}
		}
		if f.DaysOverdue >= EscalateAfter || !f.Owner.Active {
			why := fmt.Sprintf("%s's follow-up is %d days overdue", firstName(f.Owner.Name), f.DaysOverdue)
			if !f.Owner.Active {
				why = f.Owner.Name + " no longer has access — this follow-up needs someone else"
			}
			to := managers
			if f.Owner.Active && f.Manager != nil {
				to = []person{*f.Manager}
			}
			for _, m := range to {
				if m.ID == f.Owner.ID {
					continue
				}
				escalated[m.ID] = append(escalated[m.ID], f)
				if err := add(notify.Notification{
					UserID: m.ID, Kind: KindFollowUpEscalated, Link: link, EntityType: entityType, EntityID: &cid,
					Title:     "Needs attention: " + f.ClientName,
					Body:      why + " (" + f.What + ")",
					DedupeKey: fmt.Sprintf("%s:%s:%s:%s", KindFollowUpEscalated, f.ID, due, m.ID),
				}); err != nil {
					return res, err
				}
			}
		}
	}

	if err := r.coverNotices(ctx, today, managers, add); err != nil {
		return res, err
	}

	// Morning emails.
	if local.Hour() < DigestHour || r.mail == nil {
		return res, nil
	}
	byOwner := map[uuid.UUID][]dueFollowUp{}
	for _, f := range items {
		if f.Owner.Active {
			byOwner[f.Owner.ID] = append(byOwner[f.Owner.ID], f)
		}
	}
	for _, fs := range byOwner {
		n, err := r.sendDigest(ctx, today, fs[0].Owner, fs, false)
		if err != nil {
			return res, err
		}
		res.Emails += n
	}
	recipients := map[uuid.UUID]person{}
	for _, m := range managers {
		recipients[m.ID] = m
	}
	for _, f := range items {
		if f.Manager != nil {
			recipients[f.Manager.ID] = *f.Manager
		}
	}
	for id, m := range recipients {
		if fs := escalated[id]; len(fs) > 0 {
			n, err := r.sendDigest(ctx, today, m, fs, true)
			if err != nil {
				return res, err
			}
			res.Emails += n
		}
	}
	n, err := r.teamDigests(ctx, today)
	if err != nil {
		return res, err
	}
	res.Emails += n
	return res, nil
}

// Start runs the reminders shortly after start-up and then every interval.
func (r *Reminders) Start(ctx context.Context, interval time.Duration) {
	go func() {
		timer := time.NewTimer(25 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			res, err := r.Run(ctx, time.Now())
			if err != nil && ctx.Err() == nil {
				r.log.Error("client reminders failed", "err", err)
			} else if res.Notifications+res.Emails > 0 {
				r.log.Info("client reminders sent", "notifications", res.Notifications, "emails", res.Emails)
			}
			timer.Reset(interval)
		}
	}()
}

func (r *Reminders) dueFollowUps(ctx context.Context, today time.Time) ([]dueFollowUp, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT i.id, c.id, c.reference, c.legal_name, coalesce(i.follow_up_note, i.summary), i.follow_up_on,
		       u.id, u.full_name, u.email, u.active,
		       m.id, m.full_name, m.email
		FROM client_interactions i
		JOIN clients c ON c.id = i.client_id
		JOIN users u ON u.id = i.follow_up_owner_id
		LEFT JOIN users m ON m.id = u.manager_id AND m.active AND m.id <> u.id
		WHERE i.follow_up_on IS NOT NULL AND i.follow_up_done_at IS NULL AND i.follow_up_on <= $1::date
		  AND c.status = 'active'
		ORDER BY i.follow_up_on, c.legal_name`, today.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dueFollowUp
	for rows.Next() {
		var f dueFollowUp
		var mID *uuid.UUID
		var mName, mEmail *string
		if err := rows.Scan(&f.ID, &f.ClientID, &f.ClientRef, &f.ClientName, &f.What, &f.Due,
			&f.Owner.ID, &f.Owner.Name, &f.Owner.Email, &f.Owner.Active, &mID, &mName, &mEmail); err != nil {
			return nil, err
		}
		if mID != nil {
			f.Manager = &person{ID: *mID, Name: *mName, Email: *mEmail, Active: true}
		}
		f.What = shortText(f.What, 140)
		f.Due = time.Date(f.Due.Year(), f.Due.Month(), f.Due.Day(), 0, 0, 0, 0, time.UTC)
		f.DaysOverdue = int(today.Sub(f.Due).Hours() / 24)
		out = append(out, f)
	}
	return out, rows.Err()
}

// coverNotices tells each client's owner about warranties and contracts ending
// soon or just ended (within the last week, so old ones don't flood in).
func (r *Reminders) coverNotices(ctx context.Context, today time.Time, managers []person, add func(notify.Notification) error) error {
	rows, err := r.pool.Query(ctx, `
		SELECT e.id, e.make, e.model, e.serial_no, x.what, x.ends, c.id, c.legal_name, o.id, o.active
		FROM client_equipment e
		JOIN clients c ON c.id = e.client_id
		JOIN users o ON o.id = c.owner_id
		CROSS JOIN LATERAL (VALUES ('warranty', e.warranty_until), ('contract', CASE WHEN e.contract <> 'none' THEN e.contract_until END)) x(what, ends)
		WHERE e.status = 'in_use' AND c.status = 'active' AND x.ends IS NOT NULL
		  AND x.ends BETWEEN $1::date - 7 AND $1::date + $2::int
		ORDER BY x.ends`, today.Format("2006-01-02"), NoticeDaysBefore)
	if err != nil {
		return err
	}
	defer rows.Close()
	type notice struct {
		n      notify.Notification
		active bool
	}
	var list []notice
	for rows.Next() {
		var eid, cid, owner uuid.UUID
		var mk, model, what, client string
		var serial *string
		var ends time.Time
		var active bool
		if err := rows.Scan(&eid, &mk, &model, &serial, &what, &ends, &cid, &client, &owner, &active); err != nil {
			return err
		}
		ends = time.Date(ends.Year(), ends.Month(), ends.Day(), 0, 0, 0, 0, time.UTC)
		thing := mk + " " + model
		if serial != nil {
			thing += " · S/N " + *serial
		}
		label := map[string]string{"warranty": "Warranty", "contract": "Contract"}[what]
		n := notify.Notification{UserID: owner, Link: "/clients/" + cid.String() + "#equipment", EntityType: entityType, EntityID: &cid}
		if ends.Before(today) {
			n.Kind, n.Title = KindCoverEnded, label+" ended: "+client
			n.Body = thing + " — ended " + ends.Format("2 Jan") + ". A chance to offer a renewal or service contract."
		} else {
			days := int(ends.Sub(today).Hours() / 24)
			n.Kind, n.Title = KindCoverEnding, label+" ending: "+client
			n.Body = thing + " — ends " + ends.Format("2 Jan 2006") + " (" + plural(days, "day") + "). A chance to offer a renewal."
		}
		n.DedupeKey = fmt.Sprintf("%s:%s:%s:%s", n.Kind, eid, what, ends.Format("2006-01-02"))
		list = append(list, notice{n, active})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, x := range list {
		if x.active {
			if err := add(x.n); err != nil {
				return err
			}
			continue
		}
		// Nobody active owns the client: tell the managers instead.
		for _, m := range managers {
			n := x.n
			n.UserID = m.ID
			n.DedupeKey += ":" + m.ID.String()
			if err := add(n); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Reminders) managers(ctx context.Context) ([]person, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, full_name, email, active FROM users
		WHERE active AND (role = 'sales_manager'
		      OR (role = 'admin' AND NOT EXISTS (SELECT 1 FROM users WHERE active AND role = 'sales_manager')))
		ORDER BY full_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []person
	for rows.Next() {
		var p person
		if err := rows.Scan(&p.ID, &p.Name, &p.Email, &p.Active); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Reminders) sendDigest(ctx context.Context, today time.Time, to person, fs []dueFollowUp, team bool) (int, error) {
	kind := "client_followups"
	if team {
		kind = "team_client_followups"
	}
	claimed, err := notify.ClaimDigest(ctx, r.pool, to.ID, today, kind)
	if err != nil || !claimed {
		return 0, err
	}
	sort.SliceStable(fs, func(i, j int) bool { return fs[i].DaysOverdue > fs[j].DaysOverdue })

	dueToday, overdue := 0, 0
	for _, f := range fs {
		if f.DaysOverdue == 0 {
			dueToday++
		} else {
			overdue++
		}
	}
	const maxRows = 15
	var rows [][2]string
	for i, f := range fs {
		if i == maxRows {
			rows = append(rows, [2]string{"…and " + strconv.Itoa(len(fs)-maxRows) + " more", "See them all in DOS BPA"})
			break
		}
		when := "Due today"
		if f.DaysOverdue > 0 {
			when = plural(f.DaysOverdue, "day") + " overdue"
		}
		if team {
			when += " · " + f.Owner.Name
		}
		rows = append(rows, [2]string{f.ClientName, when + " — " + f.What})
	}

	var c mail.Content
	var subject string
	if team {
		subject = "Client follow-ups needing attention (" + strconv.Itoa(len(fs)) + ")"
		c = mail.Content{
			Preheader: plural(len(fs), "client follow-up") + " in the team need attention.",
			Heading:   "Client follow-ups needing attention",
			Paragraphs: []string{"Hi " + firstName(to.Name) + ". These client follow-ups are " + strconv.Itoa(EscalateAfter) +
				" or more days overdue, or belong to someone who no longer has access. Those of people who report to you come to you; the rest go to the sales managers."},
			ButtonText: "Open the clients",
			ButtonURL:  r.appURL + "/clients",
		}
	} else {
		parts := []string{}
		if dueToday > 0 {
			parts = append(parts, plural(dueToday, "client follow-up")+" due today")
		}
		if overdue > 0 {
			parts = append(parts, plural(overdue, "overdue client follow-up"))
		}
		summary := strings.Join(parts, " and ")
		subject = "Your client follow-ups: " + summary
		c = mail.Content{
			Preheader:  "You have " + summary + ".",
			Heading:    "Your client follow-ups for today",
			Paragraphs: []string{"Hi " + firstName(to.Name) + ". You have " + summary + ". When you've spoken to the client, log it in DOS BPA and tick the follow-up off."},
			ButtonText: "Open my follow-ups",
			ButtonURL:  r.appURL + "/clients?attention=followups",
		}
		if overdue > 0 {
			c.Footnote = "Follow-ups " + strconv.Itoa(EscalateAfter) + " or more days overdue are also shown to your manager."
			if fs[0].Manager == nil {
				c.Footnote = "Follow-ups " + strconv.Itoa(EscalateAfter) + " or more days overdue are also shown to the sales managers."
			}
		}
	}
	c.Details = rows
	html, text := mail.Render(c)
	if err := r.mail.Send(ctx, mail.Message{To: to.Email, ToName: to.Name, Kind: "client_followup_digest", Subject: subject, HTML: html, Text: text}); err != nil {
		r.log.Warn("client follow-up email not sent", "to", to.Email, "err", err)
		return 0, nil
	}
	return 1, nil
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func firstName(full string) string {
	if f := strings.Fields(full); len(f) > 0 {
		return f[0]
	}
	return full
}
