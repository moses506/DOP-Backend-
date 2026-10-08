package deals

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

	"dos-bpas/internal/clients"
	"dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/notify"
)

// Follow-up reminders.
//
// Every open deal should have a next step with a date. The reminder job runs
// every few minutes and:
//   - tells the owner (in the app) when a follow-up is due today, and when it
//     becomes overdue;
//   - tells the owner's manager (users.manager_id) when a follow-up is still
//     undone EscalateAfter days after its date — or the sales managers when the
//     owner has no active manager — and the sales managers straight away if the
//     owner's account is deactivated (nobody else would act on it);
//   - from DigestHour each morning, emails each person one summary of what they
//     need to follow up, instead of one email per deal.
//
// Each reminder is stored with a key, so it is created once however often the
// job runs, and each summary email is claimed per person per day.
const (
	EscalateAfter = 3 // days overdue before managers are told
	DigestHour    = 7 // local hour from which the morning summary emails go out
)

const (
	KindFollowupDue       = "followup_due"
	KindFollowupOverdue   = "followup_overdue"
	KindFollowupEscalated = "followup_escalated"
	KindDealAssigned      = "deal_assigned"
	KindDealNote          = "deal_note"
)

type Reminders struct {
	pool    *pgxpool.Pool
	clients clients.API
	mail    *mail.Service
	appURL  string
	loc     *time.Location
	log     *slog.Logger
}

func NewReminders(pool *pgxpool.Pool, cl clients.API, m *mail.Service, appURL string, loc *time.Location, log *slog.Logger) *Reminders {
	if loc == nil {
		loc, _ = time.LoadLocation("Africa/Lusaka")
	}
	return &Reminders{pool: pool, clients: cl, mail: m, appURL: strings.TrimRight(appURL, "/"), loc: loc, log: log}
}

// RunResult says what one run did, for logs and tests.
type RunResult struct {
	Notifications int
	Emails        int
}

type followup struct {
	DealID      uuid.UUID
	Reference   string
	Title       string
	NextStep    string
	Due         time.Time
	DaysOverdue int
	ClientID    uuid.UUID
	ClientName  string
	Owner       recipient
	Manager     *recipient // the owner's manager, when active
}

type recipient struct {
	ID     uuid.UUID
	Name   string
	Email  string
	Active bool
}

// Run checks every open deal's follow-up once, as at now.
func (r *Reminders) Run(ctx context.Context, now time.Time) (RunResult, error) {
	var res RunResult
	local := now.In(r.loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)

	items, err := r.dueFollowups(ctx, today)
	if err != nil {
		return res, err
	}
	managers, err := r.managers(ctx)
	if err != nil {
		return res, err
	}

	escalated := map[uuid.UUID][]followup{} // manager -> follow-ups to chase
	for _, f := range items {
		link := "/deals/" + f.DealID.String()
		subject := f.Title + " · " + f.ClientName
		due := f.Due.Format("2006-01-02")

		if f.Owner.Active {
			n := notify.Notification{UserID: f.Owner.ID, Link: link, EntityType: entityType, EntityID: &f.DealID}
			if f.DaysOverdue == 0 {
				n.Kind, n.Title, n.Body = KindFollowupDue, "Follow-up due today: "+f.NextStep, subject
				n.DedupeKey = fmt.Sprintf("%s:%s:%s", KindFollowupDue, f.DealID, due)
			} else {
				n.Kind, n.Title = KindFollowupOverdue, "Follow-up overdue: "+f.NextStep
				n.Body = subject + " — was due " + f.Due.Format("2 Jan")
				n.DedupeKey = fmt.Sprintf("%s:%s:%s", KindFollowupOverdue, f.DealID, due)
			}
			if ok, err := notify.Create(ctx, r.pool, n); err != nil {
				return res, err
			} else if ok {
				res.Notifications++
			}
		}

		if f.DaysOverdue >= EscalateAfter || !f.Owner.Active {
			why := fmt.Sprintf("%s's follow-up is %d days overdue", firstName(f.Owner.Name), f.DaysOverdue)
			if !f.Owner.Active {
				why = f.Owner.Name + " no longer has access — this deal needs a new owner"
			}
			to := managers
			if f.Owner.Active && f.Manager != nil {
				to = []recipient{*f.Manager}
			}
			for _, m := range to {
				if m.ID == f.Owner.ID {
					continue
				}
				escalated[m.ID] = append(escalated[m.ID], f)
				ok, err := notify.Create(ctx, r.pool, notify.Notification{
					UserID: m.ID, Kind: KindFollowupEscalated, Link: link, EntityType: entityType, EntityID: &f.DealID,
					Title:     "Needs attention: " + f.Title,
					Body:      why + " (" + f.NextStep + ") · " + f.ClientName,
					DedupeKey: fmt.Sprintf("%s:%s:%s:%s", KindFollowupEscalated, f.DealID, due, m.ID),
				})
				if err != nil {
					return res, err
				}
				if ok {
					res.Notifications++
				}
			}
		}
	}

	// Morning summary emails.
	if local.Hour() < DigestHour {
		return res, nil
	}
	byOwner := map[uuid.UUID][]followup{}
	for _, f := range items {
		if f.Owner.Active {
			byOwner[f.Owner.ID] = append(byOwner[f.Owner.ID], f)
		}
	}
	for _, fs := range byOwner {
		sent, err := r.sendDigest(ctx, today, fs[0].Owner, fs, false)
		if err != nil {
			return res, err
		}
		res.Emails += sent
	}
	recipients := map[uuid.UUID]recipient{}
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
			sent, err := r.sendDigest(ctx, today, m, fs, true)
			if err != nil {
				return res, err
			}
			res.Emails += sent
		}
	}
	return res, nil
}

// Start runs the reminders shortly after start-up and then every interval.
func (r *Reminders) Start(ctx context.Context, interval time.Duration) {
	go func() {
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			res, err := r.Run(ctx, time.Now())
			if err != nil && ctx.Err() == nil {
				r.log.Error("follow-up reminders failed", "err", err)
			} else if res.Notifications+res.Emails > 0 {
				r.log.Info("follow-up reminders sent", "notifications", res.Notifications, "emails", res.Emails)
			}
			timer.Reset(interval)
		}
	}()
}

func (r *Reminders) dueFollowups(ctx context.Context, today time.Time) ([]followup, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT d.id, d.reference, d.title, coalesce(d.next_step, 'Follow up'), d.next_step_date, d.client_id,
		       o.id, o.full_name, o.email, o.active, mg.id, mg.full_name, mg.email
		FROM deals d JOIN users o ON o.id = d.owner_id
		LEFT JOIN users mg ON mg.id = o.manager_id AND mg.active AND mg.id <> o.id
		WHERE d.stage NOT IN ('won','lost') AND d.next_step_date IS NOT NULL AND d.next_step_date <= $1::date
		ORDER BY d.next_step_date, d.reference`, today.Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []followup
	var ids []uuid.UUID
	for rows.Next() {
		var f followup
		var mID *uuid.UUID
		var mName, mEmail *string
		if err := rows.Scan(&f.DealID, &f.Reference, &f.Title, &f.NextStep, &f.Due, &f.ClientID,
			&f.Owner.ID, &f.Owner.Name, &f.Owner.Email, &f.Owner.Active, &mID, &mName, &mEmail); err != nil {
			return nil, err
		}
		if mID != nil {
			f.Manager = &recipient{ID: *mID, Name: *mName, Email: *mEmail, Active: true}
		}
		f.Due = time.Date(f.Due.Year(), f.Due.Month(), f.Due.Day(), 0, 0, 0, 0, time.UTC)
		f.DaysOverdue = int(today.Sub(f.Due).Hours() / 24)
		out = append(out, f)
		ids = append(ids, f.ClientID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	refs, err := r.clients.Lookup(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].ClientName = refs[out[i].ClientID].LegalName
	}
	return out, nil
}

// managers are the people told about neglected follow-ups: the active sales
// managers, or the administrators if there is no sales manager yet.
func (r *Reminders) managers(ctx context.Context) ([]recipient, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, full_name, email, active FROM users
		WHERE active AND (role = 'sales_manager'
		      OR (role = 'admin' AND NOT EXISTS (SELECT 1 FROM users WHERE active AND role = 'sales_manager')))
		ORDER BY full_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recipient
	for rows.Next() {
		var m recipient
		if err := rows.Scan(&m.ID, &m.Name, &m.Email, &m.Active); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// sendDigest emails one person their follow-ups, at most once a day.
func (r *Reminders) sendDigest(ctx context.Context, today time.Time, to recipient, fs []followup, team bool) (int, error) {
	kind := "followups"
	if team {
		kind = "team_followups"
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
		when := "due today"
		if f.DaysOverdue > 0 {
			when = plural(f.DaysOverdue, "day") + " overdue"
		}
		value := f.NextStep + " — " + when
		if team {
			value = firstName(f.Owner.Name) + ": " + value
		}
		rows = append(rows, [2]string{f.Title + " · " + f.ClientName, value})
	}

	var c mail.Content
	var subject string
	if team {
		subject = "Team follow-ups needing attention (" + strconv.Itoa(len(fs)) + ")"
		c = mail.Content{
			Preheader: plural(len(fs), "follow-up") + " in the team are overdue.",
			Heading:   "Team follow-ups needing attention",
			Paragraphs: []string{
				"Hi " + firstName(to.Name) + ". These follow-ups are " + strconv.Itoa(EscalateAfter) +
					" or more days overdue, or belong to someone who no longer has access. A quick word with the owner usually gets them moving.",
			},
			ButtonText: "Open the deals",
			ButtonURL:  r.appURL + "/deals?view=list",
		}
	} else {
		parts := []string{}
		if dueToday > 0 {
			parts = append(parts, plural(dueToday, "follow-up")+" due today")
		}
		if overdue > 0 {
			parts = append(parts, plural(overdue, "overdue follow-up"))
		}
		summary := strings.Join(parts, " and ")
		subject = "Your follow-ups: " + summary
		c = mail.Content{
			Preheader: "You have " + summary + ".",
			Heading:   "Your follow-ups for today",
			Paragraphs: []string{
				"Hi " + firstName(to.Name) + ". You have " + summary + ". Once you've done one, update the deal's next step so the team can see where it stands.",
			},
			ButtonText: "Open my deals",
			ButtonURL:  r.appURL + "/deals?mine=1&view=list",
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
	if err := r.mail.Send(ctx, mail.Message{To: to.Email, ToName: to.Name, Kind: "followup_digest", Subject: subject, HTML: html, Text: text}); err != nil {
		// The failure is logged in email_log; the in-app reminders still stand.
		r.log.Warn("follow-up summary email not sent", "to", to.Email, "err", err)
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
