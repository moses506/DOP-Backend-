package clients_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"dos-bpas/internal/clients"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/notify"
)

func reportsTo(t *testing.T, pool *pgxpool.Pool, who, manager context.Context) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE users SET manager_id = $2 WHERE id = $1`, me(who).UserID, me(manager).UserID); err != nil {
		t.Fatal(err)
	}
}

func TestVisitCheckIn(t *testing.T) {
	_, svc, us := setup(t)
	alice := actor(t, us, "alice@dos.test", auth.RoleSalesExecutive)
	c, err := svc.Create(alice, clients.CreateInput{LegalName: "Mopani Copper Mines"})
	if err != nil {
		t.Fatal(err)
	}
	here := &clients.CheckInInput{Lat: -12.9587, Lng: 28.6366, AccuracyM: 18.4}

	// Only visits and meetings, only around now, only real positions.
	bad := []clients.InteractionInput{
		{Kind: "call", Summary: "x", CheckIn: here},
		{Kind: "visit", Summary: "x", CheckIn: &clients.CheckInInput{Lat: 0, Lng: 0}},
		{Kind: "visit", Summary: "x", CheckIn: &clients.CheckInInput{Lat: 95, Lng: 28}},
		{Kind: "visit", Summary: "x", CheckIn: &clients.CheckInInput{Lat: -12, Lng: 28, AccuracyM: 50000}},
	}
	earlier := time.Now().Add(-5 * time.Hour)
	bad = append(bad, clients.InteractionInput{Kind: "visit", Summary: "x", OccurredAt: &earlier, CheckIn: here})
	for i, in := range bad {
		if _, err := svc.LogInteraction(alice, c.ID, in); code(err) != "validation_failed" {
			t.Fatalf("bad check-in %d accepted: %v", i, err)
		}
	}

	it, err := svc.LogInteraction(alice, c.ID, clients.InteractionInput{Kind: "visit", Summary: "Site survey at Kitwe", CheckIn: here})
	if err != nil {
		t.Fatal(err)
	}
	if it.CheckIn == nil || it.CheckIn.Lat != -12.9587 || it.CheckIn.Lng != 28.6366 || it.CheckIn.AccuracyM != 18 {
		t.Fatalf("check-in = %+v", it.CheckIn)
	}
	plain, err := svc.LogInteraction(alice, c.ID, clients.InteractionInput{Kind: "visit", Summary: "Earlier visit", OccurredAt: &earlier})
	if err != nil || plain.CheckIn != nil {
		t.Fatalf("plain visit = %+v, %v", plain.CheckIn, err)
	}
	items, err := svc.Interactions(alice, c.ID, 10)
	if err != nil || len(items) != 2 || items[0].CheckIn == nil || items[1].CheckIn != nil {
		t.Fatalf("listed = %+v, %v", items, err)
	}
}

func TestTeamActivity(t *testing.T) {
	pool, svc, us := setup(t)
	boss := actor(t, us, "boss@dos.test", auth.RoleSalesManager)
	lead := actor(t, us, "lead@dos.test", auth.RoleSalesExecutive)
	exec := actor(t, us, "exec@dos.test", auth.RoleSalesExecutive)
	other := actor(t, us, "other@dos.test", auth.RoleSalesExecutive)
	reportsTo(t, pool, lead, boss)
	reportsTo(t, pool, exec, lead) // two levels down from boss

	c, err := svc.Create(other, clients.CreateInput{LegalName: "Zambia Sugar"})
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []context.Context{lead, exec, exec, other} {
		if _, err := svc.LogInteraction(who, c.ID, clients.InteractionInput{Kind: "visit", Summary: "Visit",
			CheckIn: &clients.CheckInInput{Lat: -15.4, Lng: 28.3, AccuracyM: 10}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.LogInteraction(exec, c.ID, clients.InteractionInput{Kind: "call", Summary: "Call"}); err != nil {
		t.Fatal(err)
	}
	today := time.Now().In(mustLoc()).Truncate(0)
	d := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	f := clients.TeamFilter{Scope: clients.ScopeTeam, From: d, To: d, Limit: 50}

	ids := func(ctx context.Context, f clients.TeamFilter) map[uuid.UUID]int {
		items, _, err := svc.TeamLog(ctx, me(ctx), f)
		if err != nil {
			t.Fatal(err)
		}
		out := map[uuid.UUID]int{}
		for _, it := range items {
			out[it.CreatedBy.ID]++
			if it.Client == nil || it.Client.LegalName != "Zambia Sugar" {
				t.Fatalf("feed entry without client: %+v", it)
			}
		}
		return out
	}
	// The boss sees the whole tree below them; the lead sees themselves and exec; exec only themselves.
	if got := ids(boss, f); len(got) != 2 || got[me(lead).UserID] != 1 || got[me(exec).UserID] != 3 {
		t.Fatalf("boss team = %v", got)
	}
	if got := ids(lead, f); len(got) != 2 {
		t.Fatalf("lead team = %v", got)
	}
	if got := ids(exec, f); len(got) != 1 || got[me(exec).UserID] != 3 {
		t.Fatalf("exec team = %v", got)
	}
	// Managers can widen to everyone; executives can't, nor look at someone outside their team.
	all := f
	all.Scope = clients.ScopeAll
	if got := ids(boss, all); len(got) != 3 {
		t.Fatalf("boss all = %v", got)
	}
	if _, _, err := svc.TeamLog(exec, me(exec), all); code(err) != "forbidden" {
		t.Fatalf("exec all err = %v", err)
	}
	oid := me(other).UserID
	f2 := f
	f2.UserID = &oid
	if _, _, err := svc.TeamLog(lead, me(lead), f2); code(err) != "forbidden" {
		t.Fatalf("outside person err = %v", err)
	}
	// Filter by kind.
	calls := f
	calls.Kind = "call"
	if got := ids(boss, calls); len(got) != 1 || got[me(exec).UserID] != 1 {
		t.Fatalf("calls = %v", got)
	}

	sum, err := svc.TeamSummary(boss, me(boss), f)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uuid.UUID]clients.MemberSummary{}
	for _, s := range sum {
		byID[s.Person.ID] = s
	}
	if len(sum) != 3 { // boss, lead, exec
		t.Fatalf("summary people = %d", len(sum))
	}
	e := byID[me(exec).UserID]
	if e.Total != 3 || e.Counts["visit"] != 2 || e.Counts["call"] != 1 || e.CheckedIn != 2 || e.Clients != 1 || e.LastActivity == nil {
		t.Fatalf("exec summary = %+v", e)
	}

	// Follow-ups waiting: exec's open follow-up shows for exec and the boss, not for someone outside.
	soon := time.Now().In(mustLoc()).AddDate(0, 0, 2).Format("2006-01-02")
	if _, err := svc.LogInteraction(exec, c.ID, clients.InteractionInput{Kind: "call", Summary: "Call back", FollowUpOn: &soon}); err != nil {
		t.Fatal(err)
	}
	far := time.Now().In(mustLoc()).AddDate(0, 0, 20).Format("2006-01-02")
	if _, err := svc.LogInteraction(exec, c.ID, clients.InteractionInput{Kind: "call", Summary: "Much later", FollowUpOn: &far}); err != nil {
		t.Fatal(err)
	}
	waiting, err := svc.TeamFollowUps(boss, me(boss), f)
	if err != nil || len(waiting) != 1 || waiting[0].FollowUp.On != soon || waiting[0].Client == nil {
		t.Fatalf("boss waiting = %+v, %v", waiting, err)
	}
	if w, _ := svc.TeamFollowUps(other, me(other), f); len(w) != 0 {
		t.Fatalf("outsider sees %d follow-ups", len(w))
	}

	// People list says who reports directly.
	people, err := svc.TeamPeople(boss, me(boss), clients.ScopeTeam)
	if err != nil || len(people) != 3 || !people[0].IsMe {
		t.Fatalf("people = %+v, %v", people, err)
	}
	for _, p := range people {
		if p.ID == me(lead).UserID && !p.Direct || p.ID == me(exec).UserID && p.Direct {
			t.Fatalf("direct flags wrong: %+v", p)
		}
	}
}

func mustLoc() *time.Location {
	l, _ := time.LoadLocation("Africa/Lusaka")
	return l
}

func TestManagerEscalationAndTeamEmail(t *testing.T) {
	pool, svc, us := setup(t)
	salesMgr := actor(t, us, "salesmgr@dos.test", auth.RoleSalesManager)
	lead := actor(t, us, "lead@dos.test", auth.RoleSalesExecutive)
	exec := actor(t, us, "exec@dos.test", auth.RoleSalesExecutive)
	loner := actor(t, us, "loner@dos.test", auth.RoleSalesExecutive)
	reportsTo(t, pool, exec, lead)
	notes := notify.New(pool)
	ctx := context.Background()
	loc := mustLoc()
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, loc) // a Tuesday

	c, err := svc.Create(exec, clients.CreateInput{LegalName: "Lafarge Zambia"})
	if err != nil {
		t.Fatal(err)
	}
	late := func(who context.Context) {
		it, err := svc.LogInteraction(who, c.ID, clients.InteractionInput{Kind: "call", Summary: "Chase PO", FollowUpOn: day(1)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE client_interactions SET follow_up_on = $2, occurred_at = $3 WHERE id = $1`,
			it.ID, now.AddDate(0, 0, -5).Format("2006-01-02"), now.AddDate(0, 0, -1)); err != nil {
			t.Fatal(err)
		}
	}
	late(exec)  // escalates to exec's own manager, the lead
	late(loner) // no manager: escalates to the sales managers

	// Yesterday exec also visited and checked in.
	v, err := svc.LogInteraction(exec, c.ID, clients.InteractionInput{Kind: "visit", Summary: "Met the plant manager",
		CheckIn: &clients.CheckInInput{Lat: -15.4, Lng: 28.3, AccuracyM: 12}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE client_interactions SET occurred_at = $2 WHERE id = $1`, v.ID, now.AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := clients.NewReminders(pool, mail.New(mail.Config{}, pool, log), "http://app.test", loc, log)
	if _, err := r.Run(ctx, now); err != nil {
		t.Fatal(err)
	}
	if n := myNotices(t, notes, lead); n[clients.KindFollowUpEscalated] != 1 {
		t.Fatalf("lead notices = %v", n)
	}
	if n := myNotices(t, notes, salesMgr); n[clients.KindFollowUpEscalated] != 1 {
		t.Fatalf("sales manager notices = %v (should only get loner's)", n)
	}

	mails := map[string][]string{} // to -> subjects
	rows, _ := pool.Query(ctx, `SELECT recipient, subject, kind FROM email_log ORDER BY subject`)
	for rows.Next() {
		var to, subj, kind string
		_ = rows.Scan(&to, &subj, &kind)
		mails[to] = append(mails[to], kind+": "+subj)
	}
	rows.Close()
	has := func(to, want string) {
		t.Helper()
		for _, s := range mails[to] {
			if strings.Contains(s, want) {
				return
			}
		}
		t.Fatalf("%s emails = %q, want one containing %q", to, mails[to], want)
	}
	has("lead@dos.test", "team_activity_digest: Team activity for Monday 19 October: 2 client contacts, 1 visit (1 checked in)")
	has("lead@dos.test", "Client follow-ups needing attention (1)")
	// The sales manager hears about the lead and the loner (no manager), not exec (the lead has them).
	has("salesmgr@dos.test", "team_activity_digest: Team activity for Monday 19 October: 1 client contact")
	has("salesmgr@dos.test", "Client follow-ups needing attention (1)")

	// Mondays cover Friday to Sunday; weekends send nothing.
	r2 := clients.NewReminders(pool, mail.New(mail.Config{}, pool, log), "http://app.test", loc, log)
	if _, err := r2.Run(ctx, time.Date(2026, 10, 24, 9, 0, 0, 0, loc)); err != nil {
		t.Fatal(err)
	}
	var sat int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM notification_digests WHERE kind = 'team_activity' AND day = '2026-10-24'`).Scan(&sat)
	if sat != 0 {
		t.Fatalf("weekend team emails = %d", sat)
	}
	if _, err := r2.Run(ctx, time.Date(2026, 10, 26, 9, 0, 0, 0, loc)); err != nil {
		t.Fatal(err)
	}
	var subj string
	if err := pool.QueryRow(ctx, `SELECT subject FROM email_log WHERE kind = 'team_activity_digest' AND recipient = 'lead@dos.test' ORDER BY created_at DESC LIMIT 1`).Scan(&subj); err != nil ||
		!strings.Contains(subj, "Fri 23 Oct – Sun 25 Oct") {
		t.Fatalf("monday subject = %q, %v", subj, err)
	}
}
