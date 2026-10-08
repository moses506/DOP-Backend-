package clients

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/notify"
)

// The manager's morning email: what their people logged on the previous
// working day. A manager's people are those who report to them directly;
// sales managers also get the sales executives who have no manager set, so
// nobody's work goes unseen.

const KindTeamDigest = "team_activity"

type teamDay struct {
	Person       person
	Role         string
	Counts       map[string]int
	CheckedIn    int
	Clients      int
	Overdue      int
	LastEver     *time.Time
	Visits       []teamVisit
	ManagerID    *uuid.UUID
	IsUnassigned bool
}

type teamVisit struct {
	Kind      string
	Client    string
	Summary   string
	CheckedIn bool
	At        time.Time
}

// digestPeriod is the previous working day, or Friday to Sunday on a Monday.
// ok is false at weekends, when no email goes out.
func digestPeriod(today time.Time) (from, to time.Time, ok bool) {
	switch today.Weekday() {
	case time.Saturday, time.Sunday:
		return time.Time{}, time.Time{}, false
	case time.Monday:
		return today.AddDate(0, 0, -3), today.AddDate(0, 0, -1), true
	default:
		d := today.AddDate(0, 0, -1)
		return d, d, true
	}
}

func (r *Reminders) teamDigests(ctx context.Context, today time.Time) (int, error) {
	from, to, ok := digestPeriod(today)
	if !ok || r.mail == nil {
		return 0, nil
	}
	a := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, r.loc)
	b := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, r.loc).AddDate(0, 0, 1)

	// Everyone who might appear: active sales staff, and anyone who logged something.
	rows, err := r.pool.Query(ctx, `
		SELECT u.id, u.full_name, u.email, u.role, u.manager_id,
		       (SELECT count(*) FROM client_interactions
		         WHERE follow_up_owner_id = u.id AND follow_up_done_at IS NULL AND follow_up_on < $3::date),
		       (SELECT max(occurred_at) FROM client_interactions WHERE created_by = u.id)
		FROM users u
		WHERE u.active AND (u.role IN ('sales_executive', 'sales_manager')
		      OR EXISTS (SELECT 1 FROM client_interactions i WHERE i.created_by = u.id AND i.occurred_at >= $1 AND i.occurred_at < $2))`,
		a, b, today.Format("2006-01-02"))
	if err != nil {
		return 0, err
	}
	people := map[uuid.UUID]*teamDay{}
	for rows.Next() {
		d := &teamDay{Counts: map[string]int{}}
		if err := rows.Scan(&d.Person.ID, &d.Person.Name, &d.Person.Email, &d.Role, &d.ManagerID, &d.Overdue, &d.LastEver); err != nil {
			rows.Close()
			return 0, err
		}
		d.Person.Active = true
		people[d.Person.ID] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	rows, err = r.pool.Query(ctx, `
		SELECT i.created_by, i.kind, c.legal_name, i.summary, i.checked_in_at IS NOT NULL, i.occurred_at, i.client_id
		FROM client_interactions i JOIN clients c ON c.id = i.client_id
		WHERE i.occurred_at >= $1 AND i.occurred_at < $2
		ORDER BY i.occurred_at`, a, b)
	if err != nil {
		return 0, err
	}
	seen := map[uuid.UUID]map[uuid.UUID]bool{}
	for rows.Next() {
		var by, cid uuid.UUID
		var v teamVisit
		if err := rows.Scan(&by, &v.Kind, &v.Client, &v.Summary, &v.CheckedIn, &v.At, &cid); err != nil {
			rows.Close()
			return 0, err
		}
		d := people[by]
		if d == nil {
			continue
		}
		d.Counts[v.Kind]++
		if v.CheckedIn {
			d.CheckedIn++
		}
		if v.Kind != "note" {
			if seen[by] == nil {
				seen[by] = map[uuid.UUID]bool{}
			}
			if !seen[by][cid] {
				seen[by][cid] = true
				d.Clients++
			}
		}
		if v.Kind == "visit" || v.Kind == "meeting" {
			d.Visits = append(d.Visits, v)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// Who manages whom.
	rows, err = r.pool.Query(ctx, `SELECT id, full_name, email, role FROM users WHERE active`)
	if err != nil {
		return 0, err
	}
	type mgr struct {
		p    person
		role string
	}
	var all []mgr
	for rows.Next() {
		var m mgr
		if err := rows.Scan(&m.p.ID, &m.p.Name, &m.p.Email, &m.role); err != nil {
			rows.Close()
			return 0, err
		}
		m.p.Active = true
		all = append(all, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	sent := 0
	for _, m := range all {
		var team []*teamDay
		for _, d := range people {
			if d.Person.ID == m.p.ID {
				continue
			}
			direct := d.ManagerID != nil && *d.ManagerID == m.p.ID
			unassigned := d.ManagerID == nil && d.Role == "sales_executive" && m.role == "sales_manager"
			if direct || unassigned {
				dd := *d
				dd.IsUnassigned = unassigned && !direct
				team = append(team, &dd)
			}
		}
		if len(team) == 0 {
			continue
		}
		n, err := r.sendTeamDigest(ctx, today, from, to, m.p, team)
		if err != nil {
			return sent, err
		}
		sent += n
	}
	return sent, nil
}

func (d *teamDay) contacts() int {
	n := 0
	for k, v := range d.Counts {
		if k != "note" {
			n += v
		}
	}
	return n
}

func (d *teamDay) line() string {
	if d.contacts() == 0 && d.Counts["note"] == 0 {
		s := "Nothing logged"
		if d.LastEver != nil {
			s += " · last entry " + d.LastEver.Format("2 Jan")
		} else {
			s += " · never logged a contact"
		}
		if d.Overdue > 0 {
			s += " · " + plural(d.Overdue, "overdue follow-up")
		}
		return s
	}
	var parts []string
	for _, k := range []string{"visit", "meeting", "call", "email", "message", "note"} {
		if n := d.Counts[k]; n > 0 {
			parts = append(parts, plural(n, k))
		}
	}
	s := strings.Join(parts, ", ")
	if v := d.Counts["visit"] + d.Counts["meeting"]; v > 0 {
		s += " (" + strconv.Itoa(d.CheckedIn) + " of " + strconv.Itoa(v) + " checked in)"
	}
	if d.Clients > 0 {
		s += " · " + plural(d.Clients, "client")
	}
	if d.Overdue > 0 {
		s += " · " + plural(d.Overdue, "overdue follow-up")
	}
	return s
}

func periodLabel(from, to time.Time) string {
	if from.Equal(to) {
		return from.Format("Monday 2 January")
	}
	return from.Format("Mon 2 Jan") + " – " + to.Format("Mon 2 Jan")
}

func (r *Reminders) sendTeamDigest(ctx context.Context, today, from, to time.Time, m person, team []*teamDay) (int, error) {
	claimed, err := notify.ClaimDigest(ctx, r.pool, m.ID, today, KindTeamDigest)
	if err != nil || !claimed {
		return 0, err
	}
	// Busiest first; people with nothing logged last.
	sort.SliceStable(team, func(i, j int) bool {
		if a, b := team[i].contacts(), team[j].contacts(); a != b {
			return a > b
		}
		return team[i].Person.Name < team[j].Person.Name
	})

	total, visits, checked, quiet := 0, 0, 0, 0
	for _, d := range team {
		total += d.contacts()
		visits += d.Counts["visit"] + d.Counts["meeting"]
		checked += d.CheckedIn
		if d.contacts() == 0 {
			quiet++
		}
	}

	var rows [][2]string
	for _, d := range team {
		name := d.Person.Name
		if d.IsUnassigned {
			name += " (no manager set)"
		}
		rows = append(rows, [2]string{name, d.line()})
	}
	// Then the visits themselves, so the manager can see where people went.
	const maxVisits = 12
	shown := 0
	for _, d := range team {
		for _, v := range d.Visits {
			if shown == maxVisits {
				break
			}
			tag := "Visit"
			if v.Kind == "meeting" {
				tag = "Meeting"
			}
			if v.CheckedIn {
				tag += ", checked in"
			}
			when := v.At.In(r.loc).Format("Mon 15:04")
			rows = append(rows, [2]string{firstName(d.Person.Name) + " · " + v.Client, tag + " · " + when + " — " + shortText(v.Summary, 110)})
			shown++
		}
	}
	if visits > shown {
		rows = append(rows, [2]string{"…and " + plural(visits-shown, "more visit"), "See them all in DOS BPA"})
	}

	label := periodLabel(from, to)
	summary := plural(total, "client contact")
	if visits > 0 {
		summary += ", " + plural(visits, "visit") + " (" + strconv.Itoa(checked) + " checked in)"
	}
	para := []string{"Hi " + firstName(m.Name) + ". Here's what your team logged on " + label + ": " + summary + "."}
	if quiet > 0 {
		who := "1 person"
		if quiet != 1 {
			who = strconv.Itoa(quiet) + " people"
		}
		para = append(para, who+" logged nothing with clients.")
	}
	q := "from=" + from.Format("2006-01-02") + "&to=" + to.Format("2006-01-02")
	c := mail.Content{
		Preheader:  label + ": " + summary + ".",
		Heading:    "Team activity · " + label,
		Paragraphs: para,
		ButtonText: "Open team activity",
		ButtonURL:  r.appURL + "/activity?" + q,
		Details:    rows,
		Footnote:   "You get this each working morning for the people who report to you. Who reports to whom is set on the Staff page.",
	}
	html, text := mail.Render(c)
	subject := "Team activity for " + label + ": " + summary
	if err := r.mail.Send(ctx, mail.Message{To: m.Email, ToName: m.Name, Kind: "team_activity_digest", Subject: subject, HTML: html, Text: text}); err != nil {
		r.log.Warn("team activity email not sent", "to", m.Email, "err", err)
		return 0, nil
	}
	return 1, nil
}
