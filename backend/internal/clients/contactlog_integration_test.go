package clients_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"dos-bpas/internal/clients"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/notify"
)

func me(ctx context.Context) auth.Principal {
	p, _ := auth.FromContext(ctx)
	return p
}

func myNotices(t *testing.T, svc *notify.Service, ctx context.Context) map[string]int {
	t.Helper()
	items, _, err := svc.List(context.Background(), me(ctx).UserID, 100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, n := range items {
		out[n.Kind]++
	}
	return out
}

func day(offset int) *string {
	loc, _ := time.LoadLocation("Africa/Lusaka")
	s := time.Now().In(loc).AddDate(0, 0, offset).Format("2006-01-02")
	return &s
}

func TestContactLogAndFollowUps(t *testing.T) {
	pool, svc, us := setup(t)
	alice := actor(t, us, "alice@dos.test", auth.RoleSalesExecutive)
	bob := actor(t, us, "bob@dos.test", auth.RoleTechnical)
	carol := actor(t, us, "carol@dos.test", auth.RoleSalesExecutive)
	manager := actor(t, us, "manager@dos.test", auth.RoleSalesManager)
	notes := notify.New(pool)

	c, err := svc.Create(alice, clients.CreateInput{LegalName: "Zambeef Products PLC",
		Contacts: []clients.ContactInput{{Name: "Mutale Banda", IsPrimary: true}}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.Create(alice, clients.CreateInput{LegalName: "Other Co", Contacts: []clients.ContactInput{{Name: "Someone"}}})
	if err != nil {
		t.Fatal(err)
	}
	if c.ContactHealth != "never" || c.LastContactAt != nil {
		t.Fatalf("new client health = %q", c.ContactHealth)
	}

	// Mistakes are caught.
	future := time.Now().Add(2 * time.Hour)
	_, err = svc.LogInteraction(alice, c.ID, clients.InteractionInput{Kind: "fax", Summary: "", OccurredAt: &future, FollowUpOn: day(-1)})
	if code(err) != "validation_failed" {
		t.Fatalf("bad entry code = %q (%v)", code(err), err)
	}
	_, err = svc.LogInteraction(alice, c.ID, clients.InteractionInput{Kind: "call", Summary: "Hi", ContactID: &other.Contacts[0].ID})
	if code(err) != "validation_failed" {
		t.Fatalf("other client's contact accepted: %v", err)
	}

	// A call with a follow-up for a colleague: the client shows the contact, the colleague hears about it.
	contact := c.Contacts[0].ID
	bobID := me(bob).UserID
	call, err := svc.LogInteraction(alice, c.ID, clients.InteractionInput{
		Kind: "call", Summary: "Discussed replacing the 3 copiers at head office", Outcome: str("Wants a demo"),
		ContactID: &contact, FollowUpOn: day(2), FollowUpNote: str("Book the C3226i demo"), FollowUpOwnerID: &bobID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if call.Contact == nil || call.Contact.Name != "Mutale Banda" || call.FollowUp == nil || call.FollowUp.Owner.ID != bobID {
		t.Fatalf("call = %+v", call)
	}
	got, _ := svc.Get(alice, c.ID)
	if got.ContactHealth != "good" || got.LastContactAt == nil || got.OpenFollowUps != 1 || got.NextFollowUpOn == nil || *got.NextFollowUpOn != *day(2) {
		t.Fatalf("after call: health=%q open=%d next=%v", got.ContactHealth, got.OpenFollowUps, got.NextFollowUpOn)
	}
	if got.Version != c.Version {
		t.Fatal("logging a call must not change the client's version (it would break someone's open edit)")
	}
	if n := myNotices(t, notes, bob); n[clients.KindFollowUpAssigned] != 1 {
		t.Fatalf("bob notices = %v", n)
	}

	// An internal note doesn't count as contact.
	before := *got.LastContactAt
	old := time.Now().Add(-48 * time.Hour)
	if _, err := svc.LogInteraction(alice, other.ID, clients.InteractionInput{Kind: "note", Summary: "Internal: check credit"}); err != nil {
		t.Fatal(err)
	}
	o, _ := svc.Get(alice, other.ID)
	if o.LastContactAt != nil {
		t.Fatal("a note counted as contact")
	}
	// An older entry logged later doesn't move last contact backwards.
	if _, err := svc.LogInteraction(alice, c.ID, clients.InteractionInput{Kind: "visit", Summary: "Site visit last week", OccurredAt: &old}); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.Get(alice, c.ID)
	if !got.LastContactAt.Equal(before) {
		t.Fatalf("last contact moved back: %v -> %v", before, got.LastContactAt)
	}

	// Carol has nothing to do with it, so she can't touch the follow-up.
	if _, err := svc.ChangeFollowUp(carol, c.ID, call.ID, clients.FollowUpChange{Action: "done"}); code(err) != "forbidden" {
		t.Fatalf("carol err = %v", err)
	}
	// The manager can move it; the past isn't allowed.
	if _, err := svc.ChangeFollowUp(manager, c.ID, call.ID, clients.FollowUpChange{Action: "reschedule", On: day(-1)}); code(err) != "validation_failed" {
		t.Fatalf("past reschedule err = %v", err)
	}
	moved, err := svc.ChangeFollowUp(manager, c.ID, call.ID, clients.FollowUpChange{Action: "reschedule", On: day(0), Note: "Client asked for today"})
	if err != nil || moved.FollowUp.On != *day(0) {
		t.Fatalf("reschedule = %+v, %v", moved.FollowUp, err)
	}

	// Bob's follow-up is now due today, so it shows in his list.
	list, err := svc.List(bob, clients.ListFilter{Attention: "followups", Me: bobID, Page: httpx.Page{Limit: 25}})
	if err != nil || list.Total != 1 || list.Items[0].ID != c.ID {
		t.Fatalf("bob's follow-ups = %+v, %v", list, err)
	}
	mine, _ := svc.MyFollowUps(bob, bobID, 10)
	if len(mine) != 1 || mine[0].ClientName != "Zambeef Products PLC" {
		t.Fatalf("my follow-ups = %+v", mine)
	}
	sum, err := svc.Summary(context.Background(), bobID)
	if err != nil || sum.FollowUpsDue != 1 || sum.FollowUpsOverdue != 0 {
		t.Fatalf("summary = %+v, %v", sum, err)
	}

	// Bob does the demo and logs it, ticking the follow-up off in the same step.
	demo, err := svc.LogInteraction(bob, c.ID, clients.InteractionInput{Kind: "visit", Summary: "Demo done, sent specs", Completes: []uuid.UUID{call.ID}})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := svc.Interactions(alice, c.ID, 50)
	var closed *clients.Interaction
	for i := range items {
		if items[i].ID == call.ID {
			closed = &items[i]
		}
	}
	if closed == nil || closed.FollowUp.DoneAt == nil || closed.FollowUp.DoneBy == nil || *closed.FollowUp.DoneBy != "Test technical" {
		t.Fatalf("follow-up not closed by the demo: %+v", closed)
	}
	if items[0].ID != demo.ID {
		t.Fatal("newest entry should come first")
	}
	got, _ = svc.Get(alice, c.ID)
	if got.OpenFollowUps != 0 || got.NextFollowUpOn != nil {
		t.Fatalf("open follow-ups after done = %d", got.OpenFollowUps)
	}
	if _, err := svc.ChangeFollowUp(alice, c.ID, call.ID, clients.FollowUpChange{Action: "done"}); code(err) != "follow_up_closed" {
		t.Fatalf("closing twice err = %v", err)
	}

	// Handing over a follow-up tells the new person; the audit history records the changes.
	f2, err := svc.LogInteraction(alice, c.ID, clients.InteractionInput{Kind: "email", Summary: "Sent quotation", FollowUpOn: day(5)})
	if err != nil {
		t.Fatal(err)
	}
	carolID := me(carol).UserID
	if _, err := svc.ChangeFollowUp(alice, c.ID, f2.ID, clients.FollowUpChange{Action: "reassign", OwnerID: &carolID, Note: "I'm on leave"}); err != nil {
		t.Fatal(err)
	}
	if n := myNotices(t, notes, carol); n[clients.KindFollowUpAssigned] != 1 {
		t.Fatalf("carol notices = %v", n)
	}
	hist, _ := svc.Activity(alice, c.ID)
	actions := map[string]int{}
	for _, h := range hist {
		actions[h.Action]++
	}
	if actions["follow_up_moved"] != 1 || actions["follow_up_done"] != 1 || actions["follow_up_reassigned"] != 1 {
		t.Fatalf("audit actions = %v", actions)
	}

	// Clients not contacted recently show in the "going cold" list; Zambeef was just contacted.
	cold, err := svc.List(alice, clients.ListFilter{Attention: "cold", Page: httpx.Page{Limit: 25}})
	if err != nil || cold.Total != 1 || cold.Items[0].ID != other.ID {
		t.Fatalf("cold = %+v, %v", cold, err)
	}

	// No logging against a deactivated client.
	o, _ = svc.Get(alice, other.ID)
	if _, err := svc.Deactivate(manager, other.ID, o.Version, "Closed down"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LogInteraction(alice, other.ID, clients.InteractionInput{Kind: "call", Summary: "x"}); code(err) != "client_inactive" {
		t.Fatalf("inactive err = %v", err)
	}
}

func TestClientFollowUpReminders(t *testing.T) {
	pool, svc, us := setup(t)
	alice := actor(t, us, "alice@dos.test", auth.RoleSalesExecutive)
	manager := actor(t, us, "manager@dos.test", auth.RoleSalesManager)
	notes := notify.New(pool)
	ctx := context.Background()

	c, err := svc.Create(alice, clients.CreateInput{LegalName: "Konkola Copper Mines"})
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Africa/Lusaka")
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, loc)
	mk := func(what string, offset int) {
		it, err := svc.LogInteraction(alice, c.ID, clients.InteractionInput{Kind: "call", Summary: what, FollowUpOn: day(1)})
		if err != nil {
			t.Fatal(err)
		}
		// Put the follow-up where the test needs it relative to "now".
		if _, err := pool.Exec(ctx, `UPDATE client_interactions SET follow_up_on = $2 WHERE id = $1`,
			it.ID, now.AddDate(0, 0, offset).Format("2006-01-02")); err != nil {
			t.Fatal(err)
		}
	}
	mk("Due today", 0)
	mk("One day late", -1)
	mk("Five days late", -5)
	mk("Next week", 7)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := clients.NewReminders(pool, mail.New(mail.Config{}, pool, log), "http://app.test", loc, log)
	res, err := r.Run(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n := myNotices(t, notes, alice); n[clients.KindFollowUpDue] != 1 || n[clients.KindFollowUpOverdue] != 2 {
		t.Fatalf("owner notices = %v", n)
	}
	if n := myNotices(t, notes, manager); n[clients.KindFollowUpEscalated] != 1 || len(n) != 1 {
		t.Fatalf("manager notices = %v", n)
	}
	// Two follow-up emails, and the sales manager's team email: Alice has no manager set.
	if res.Notifications != 4 || res.Emails != 3 {
		t.Fatalf("first run = %+v", res)
	}
	var subjects []string
	rows, _ := pool.Query(ctx, `SELECT subject FROM email_log WHERE kind = 'client_followup_digest' ORDER BY subject`)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		subjects = append(subjects, s)
	}
	rows.Close()
	if len(subjects) != 2 || subjects[0] != "Client follow-ups needing attention (1)" ||
		subjects[1] != "Your client follow-ups: 1 client follow-up due today and 2 overdue client follow-ups" {
		t.Fatalf("emails = %q", subjects)
	}

	// Nothing repeats on the next run.
	if res, err = r.Run(ctx, now.Add(15*time.Minute)); err != nil || res.Notifications+res.Emails != 0 {
		t.Fatalf("second run = %+v, %v", res, err)
	}

	// Before 07:00 there are notices but no emails.
	if res, err = r.Run(ctx, now.AddDate(0, 0, 1).Add(-2*time.Hour)); err != nil || res.Emails != 0 {
		t.Fatalf("early run = %+v, %v", res, err)
	}
}

func TestEquipmentRegister(t *testing.T) {
	pool, svc, us := setup(t)
	alice := actor(t, us, "alice@dos.test", auth.RoleSalesExecutive)
	tech := actor(t, us, "tech@dos.test", auth.RoleTechnical)
	notes := notify.New(pool)
	ctx := context.Background()

	a, err := svc.Create(alice, clients.CreateInput{LegalName: "Bank of Zambia",
		Sites: []clients.SiteInput{{Name: "Head office", Address: "Cairo Road"}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Create(alice, clients.CreateInput{LegalName: "Zesco Limited"})
	if err != nil {
		t.Fatal(err)
	}
	site := a.Sites[0].ID

	// Mistakes are caught.
	_, err = svc.AddEquipment(tech, a.ID, clients.EquipmentInput{Category: "toaster", Contract: "service", SiteID: &b.ID})
	if code(err) != "validation_failed" {
		t.Fatalf("bad equipment err = %v", err)
	}

	cop, err := svc.AddEquipment(tech, a.ID, clients.EquipmentInput{
		SiteID: &site, Category: "copier", Make: "Canon", Model: "imageRUNNER ADVANCE C3226i", SerialNo: str("2ZK-01234"),
		AssetTag: str("BOZ-IT-0091"), InstalledOn: day(-300), WarrantyUntil: day(20), Contract: "service", ContractUntil: day(400),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cop.Site == nil || cop.Site.Name != "Head office" || cop.Cover != "contract" || cop.EndingSoon {
		t.Fatalf("copier = %+v", cop)
	}

	// The same serial (typed differently) can't be at two clients at once.
	_, err = svc.AddEquipment(tech, b.ID, clients.EquipmentInput{Category: "copier", Make: "Canon", Model: "C3226i", SerialNo: str("2zk 01234")})
	if code(err) != "validation_failed" {
		t.Fatalf("duplicate serial err = %v", err)
	}

	// Searching the client list by serial or asset tag finds the client.
	for _, q := range []string{"2ZK01234", "boz-it-0091"} {
		l, err := svc.List(alice, clients.ListFilter{Query: q, Page: httpx.Page{Limit: 25}})
		if err != nil || l.Total != 1 || l.Items[0].ID != a.ID {
			t.Fatalf("search %q = %+v, %v", q, l, err)
		}
	}

	// Updating checks the version; dropping the contract shows the warranty, which ends soon.
	up := clients.EquipmentInput{Version: cop.Version, SiteID: &site, Category: "copier", Make: "Canon", Model: "imageRUNNER ADVANCE C3226i",
		SerialNo: str("2ZK-01234"), WarrantyUntil: day(20), Contract: "none"}
	upd, err := svc.UpdateEquipment(tech, a.ID, cop.ID, up)
	if err != nil {
		t.Fatal(err)
	}
	if upd.Cover != "warranty" || !upd.EndingSoon || upd.Version != cop.Version+1 || upd.ContractUntil != nil {
		t.Fatalf("updated = %+v", upd)
	}
	if _, err := svc.UpdateEquipment(tech, a.ID, cop.ID, up); code(err) != "stale_version" {
		t.Fatalf("stale err = %v", err)
	}
	up.Version = upd.Version
	if _, err := svc.UpdateEquipment(tech, a.ID, cop.ID, up); code(err) != "no_change" {
		t.Fatalf("no change err = %v", err)
	}

	// The warranty ending soon is a renewal chance: the client's owner hears about it once.
	loc, _ := time.LoadLocation("Africa/Lusaka")
	r := clients.NewReminders(pool, nil, "http://app.test", loc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := r.Run(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := myNotices(t, notes, alice); n[clients.KindCoverEnding] != 1 {
		t.Fatalf("owner cover notices = %v", n)
	}
	sum, _ := svc.Summary(ctx, me(alice).UserID)
	if sum.CoverEndingSoon != 1 {
		t.Fatalf("cover ending soon = %d", sum.CoverEndingSoon)
	}

	// Removed, it moves to the other client.
	if _, err := svc.RemoveEquipment(tech, a.ID, cop.ID, upd.Version, ""); code(err) != "validation_failed" {
		t.Fatalf("remove without reason err = %v", err)
	}
	gone, err := svc.RemoveEquipment(tech, a.ID, cop.ID, upd.Version, "Moved to Zesco under new lease")
	if err != nil || gone.Status != "removed" || gone.Cover != "none" {
		t.Fatalf("removed = %+v, %v", gone, err)
	}
	if _, err := svc.AddEquipment(tech, b.ID, clients.EquipmentInput{Category: "copier", Make: "Canon", Model: "C3226i",
		SerialNo: str("2ZK-01234"), Contract: "lease", ContractUntil: day(700)}); err != nil {
		t.Fatalf("re-register after removal: %v", err)
	}
	list, _ := svc.Equipment(alice, a.ID)
	if len(list) != 1 || list[0].Status != "removed" {
		t.Fatalf("history kept = %+v", list)
	}
	hist, _ := svc.Activity(alice, a.ID)
	actions := map[string]int{}
	for _, h := range hist {
		actions[h.Action]++
	}
	if actions["equipment_added"] != 1 || actions["equipment_updated"] != 1 || actions["equipment_removed"] != 1 {
		t.Fatalf("audit = %v", actions)
	}

	// Who may do what.
	fin := actor(t, us, "fin@dos.test", auth.RoleFinance)
	if me(fin).Can(clients.PermEquipment) || !me(fin).Can(clients.PermLog) || !me(tech).Can(clients.PermEquipment) {
		t.Fatal("role permissions are wrong")
	}
}
