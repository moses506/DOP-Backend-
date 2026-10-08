package deals_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"dos-bpas/internal/deals"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/notify"
)

func notifications(t *testing.T, e env, ctx context.Context) []notify.Notification {
	t.Helper()
	items, _, err := notify.New(e.pool).List(context.Background(), principal(ctx).UserID, 50)
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func kinds(ns []notify.Notification) map[string]int {
	out := map[string]int{}
	for _, n := range ns {
		out[n.Kind]++
	}
	return out
}

func TestFollowupReminders(t *testing.T) {
	e := setup(t)
	alice := e.actor(t, "alice@dos.test", auth.RoleSalesExecutive)
	manager := e.actor(t, "manager@dos.test", auth.RoleSalesManager)
	cl := e.client(t, alice, "Konkola Copper Mines")

	loc, _ := time.LoadLocation("Africa/Lusaka")
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, loc) // 08:00, after the morning summary hour
	day := func(offset int) *deals.Date {
		d := deals.Date(now.AddDate(0, 0, offset).Format("2006-01-02"))
		return &d
	}
	mk := func(title string, due *deals.Date) {
		if _, err := e.deals.Create(alice, deals.CreateInput{ClientID: cl.ID, Title: title, NextStep: str("Call buyer"), NextStepDate: due}); err != nil {
			t.Fatal(err)
		}
	}
	mk("Due today", day(0))
	mk("Overdue one day", day(-1))
	mk("Overdue five days", day(-5))
	mk("Next week", day(7))

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := deals.NewReminders(e.pool, e.clients, mail.New(mail.Config{}, e.pool, log), "http://app.test", loc, log)
	res, err := r.Run(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}

	// The owner hears about everything due or overdue; the manager only about the one 3+ days late.
	got := kinds(notifications(t, e, alice))
	if got[deals.KindFollowupDue] != 1 || got[deals.KindFollowupOverdue] != 2 {
		t.Fatalf("owner notifications = %v", got)
	}
	got = kinds(notifications(t, e, manager))
	if got[deals.KindFollowupEscalated] != 1 || len(got) != 1 {
		t.Fatalf("manager notifications = %v", got)
	}
	if res.Notifications != 4 || res.Emails != 2 { // owner summary + manager summary
		t.Fatalf("first run = %+v", res)
	}

	// Running again the same day repeats nothing.
	res, err = r.Run(context.Background(), now.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if res.Notifications != 0 || res.Emails != 0 {
		t.Fatalf("second run should be silent, got %+v", res)
	}

	// Before the summary hour, reminders appear in the app but no email is sent.
	early := time.Date(2026, 10, 21, 5, 0, 0, 0, loc)
	res, err = r.Run(context.Background(), early)
	if err != nil {
		t.Fatal(err)
	}
	if res.Emails != 0 {
		t.Fatalf("emails before %d:00: %+v", deals.DigestHour, res)
	}
}

func TestFollowupEscalatesWhenOwnerLeaves(t *testing.T) {
	e := setup(t)
	admin := e.actor(t, "admin@dos.test", auth.RoleAdmin)
	bob := e.actor(t, "bob@dos.test", auth.RoleSalesExecutive)
	cl := e.client(t, bob, "Mopani")
	today := deals.Date(time.Now().In(time.UTC).Format("2006-01-02"))
	if _, err := e.deals.Create(bob, deals.CreateInput{ClientID: cl.ID, Title: "Printers", NextStep: str("Visit"), NextStepDate: &today}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE users SET active = false WHERE id = $1`, principal(bob).UserID); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := deals.NewReminders(e.pool, e.clients, mail.New(mail.Config{}, e.pool, log), "", time.UTC, log)
	if _, err := r.Run(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	// With no sales manager, the administrator is told straight away.
	if got := kinds(notifications(t, e, admin)); got[deals.KindFollowupEscalated] != 1 {
		t.Fatalf("admin notifications = %v", got)
	}
}

func TestColleagueActivityNotifiesOwner(t *testing.T) {
	e := setup(t)
	alice := e.actor(t, "alice@dos.test", auth.RoleSalesExecutive)
	bob := e.actor(t, "bob@dos.test", auth.RoleSalesExecutive)
	manager := e.actor(t, "manager@dos.test", auth.RoleSalesManager)
	cl := e.client(t, alice, "Zesco")
	d, err := e.deals.Create(alice, deals.CreateInput{ClientID: cl.ID, Title: "Meters"})
	if err != nil {
		t.Fatal(err)
	}
	// Alice's own note doesn't notify her; Bob's does.
	if _, err := e.deals.AddNote(alice, d.ID, deals.NoteInput{Kind: "note", Body: "mine"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deals.AddNote(bob, d.ID, deals.NoteInput{Kind: "call", Body: "Client called the office"}); err != nil {
		t.Fatal(err)
	}
	if got := kinds(notifications(t, e, alice)); got[deals.KindDealNote] != 1 {
		t.Fatalf("alice notifications = %v", got)
	}
	// Handing the deal to Bob tells Bob.
	bobID := principal(bob).UserID
	if _, err := e.deals.Update(manager, d.ID, deals.UpdateInput{Version: d.Version, OwnerID: &bobID, Reason: "Alice on leave"}); err != nil {
		t.Fatal(err)
	}
	if got := kinds(notifications(t, e, bob)); got[deals.KindDealAssigned] != 1 {
		t.Fatalf("bob notifications = %v", got)
	}
}
