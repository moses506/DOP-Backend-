package sourcing_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"dos-bpas/internal/clients"
	clientmig "dos-bpas/internal/clients/migrations"
	"dos-bpas/internal/deals"
	dealmig "dos-bpas/internal/deals/migrations"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/events"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/migrate"
	platmig "dos-bpas/internal/platform/migrations"
	"dos-bpas/internal/platform/notify"
	"dos-bpas/internal/platform/numbering"
	"dos-bpas/internal/sourcing"
	sourcemig "dos-bpas/internal/sourcing/migrations"
	"dos-bpas/internal/users"
)

// Run with: TEST_DATABASE_URL=postgres://... go test -p 1 ./...

type env struct {
	pool    *pgxpool.Pool
	clients *clients.Service
	svc     *sourcing.Service
	users   *users.Service
}

func setup(t *testing.T) env {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := migrate.Run(ctx, pool, log, []migrate.Source{
		{Module: "platform", FS: platmig.FS}, {Module: "clients", FS: clientmig.FS},
		{Module: "deals", FS: dealmig.FS}, {Module: "sourcing", FS: sourcemig.FS},
	}); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Africa/Lusaka")
	nums, bus := numbering.New(loc), events.NewBus(log)
	cl := clients.NewService(pool, nums, bus, time.Now)
	dl := deals.NewService(pool, nums, bus, cl, time.Now)
	svc := sourcing.NewService(pool, nums, cl, dl, mail.New(mail.Config{}, pool, log), time.Now, sourcing.Options{Location: loc, Log: log})
	return env{pool: pool, clients: cl, svc: svc, users: users.NewService(pool, time.Now)}
}

func (e env) actor(t *testing.T, email string, role auth.Role) context.Context {
	t.Helper()
	u, err := e.users.Create(context.Background(), users.CreateInput{
		Email: email, FullName: "Test " + strings.Split(email, "@")[0], Position: "Tester", Department: "Sales",
		Role: role, Password: "correct-horse-battery",
	})
	if err != nil {
		t.Fatal(err)
	}
	return auth.WithPrincipal(context.Background(), u.Principal())
}

func code(err error) string {
	var e *httpx.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func str(s string) *string { return &s }

func me(ctx context.Context) uuid.UUID {
	p, _ := auth.FromContext(ctx)
	return p.UserID
}

func TestSupplierRegister(t *testing.T) {
	e := setup(t)
	ops := e.actor(t, "ops@dos.test", auth.RoleOperations)

	// Mistakes are caught: no accounts, a bad email, two accounts in one region, a limit on a COD account.
	bad := []sourcing.SupplierInput{
		{Name: "Axiz"},
		{Name: "Axiz", Accounts: []sourcing.AccountInput{{Region: "ZA", Currency: "ZAR", ContactEmail: "nope"}}},
		{Name: "Axiz", Accounts: []sourcing.AccountInput{{Region: "ZA", Currency: "ZAR", ContactEmail: "a@axiz.com"}, {Region: "ZA", Currency: "ZAR", ContactEmail: "b@axiz.com"}}},
		{Name: "Axiz", Accounts: []sourcing.AccountInput{{Region: "ZA", Currency: "ZAR", ContactEmail: "a@axiz.com", Terms: str("cod"), CreditLimit: str("1000")}}},
	}
	for i, in := range bad {
		if _, err := e.svc.CreateSupplier(ops, in); code(err) != "validation_failed" {
			t.Fatalf("bad supplier %d err = %v", i, err)
		}
	}

	axiz, err := e.svc.CreateSupplier(ops, sourcing.SupplierInput{Name: "Axiz", Website: str("https://www.axiz.com"), Brands: str("HP, Lenovo, Microsoft"),
		Accounts: []sourcing.AccountInput{
			{Region: "ZA", AccountNumber: str("SJSA54"), Terms: str("credit"), CreditLimit: str("R450,000.00"), Currency: "ZAR",
				ContactEmail: "channel@axiz.com", OtherEmails: []string{"Marinda.Sanders@axiz.com; Juanita.VanStaden@axiz.com"}},
			{Region: "ZM", AccountNumber: str("SJD8085"), Terms: str("cod"), Currency: "USD", ContactName: str("Julian Hattingh"), ContactEmail: "Julian.Hattingh@axiz.com"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	za := axiz.Accounts[0]
	if !strings.HasPrefix(axiz.Reference, "DOS-SUP-") || len(axiz.Accounts) != 2 || za.Region != "ZA" || za.CreditLimit == nil || *za.CreditLimit != "450000.00" ||
		len(za.OtherEmails) != 2 || za.RegionName != "South Africa" {
		t.Fatalf("supplier = %+v", axiz)
	}
	// "AXIZ (Pty) Ltd" is the same supplier.
	if _, err := e.svc.CreateSupplier(ops, sourcing.SupplierInput{Name: "AXIZ (Pty) Ltd", Accounts: []sourcing.AccountInput{{Region: "ZA", Currency: "ZAR", ContactEmail: "x@axiz.com"}}}); code(err) != "duplicate_supplier" {
		t.Fatalf("duplicate err = %v", err)
	}

	// Dropping an account retires it; it stays on record.
	upd := sourcing.SupplierInput{Version: axiz.Version, Name: "Axiz", Brands: axiz.Brands, Accounts: []sourcing.AccountInput{{ID: &za.ID, Region: "ZA", AccountNumber: za.AccountNumber, Terms: str("credit"),
		CreditLimit: str("500000"), Currency: "ZAR", ContactEmail: "channel@axiz.com"}}}
	axiz, err = e.svc.UpdateSupplier(ops, axiz.ID, upd)
	if err != nil || len(axiz.Accounts) != 1 || *axiz.Accounts[0].CreditLimit != "500000.00" {
		t.Fatalf("update = %+v, %v", axiz.Accounts, err)
	}
	var retired int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM supplier_accounts WHERE supplier_id=$1 AND NOT active`, axiz.ID).Scan(&retired)
	if retired != 1 {
		t.Fatalf("retired accounts = %d", retired)
	}
	if _, err := e.svc.UpdateSupplier(ops, axiz.ID, upd); code(err) != "stale_version" {
		t.Fatalf("stale err = %v", err)
	}

	// Search and filters.
	for q, want := range map[string]int{"lenovo": 1, "SJSA54": 1, "channel@": 1, "nothing": 0} {
		l, _ := e.svc.ListSuppliers(context.Background(), sourcing.SupplierFilter{Query: q, Limit: 10})
		if l.Total != want {
			t.Fatalf("search %q = %d", q, l.Total)
		}
	}
	if l, _ := e.svc.ListSuppliers(context.Background(), sourcing.SupplierFilter{Region: "ZM", Limit: 10}); l.Total != 0 {
		t.Fatalf("retired region still listed")
	}
	if _, err := e.svc.SetSupplierActive(ops, axiz.ID, axiz.Version, false, ""); code(err) != "validation_failed" {
		t.Fatalf("deactivate without reason err = %v", err)
	}
	if x, err := e.svc.SetSupplierActive(ops, axiz.ID, axiz.Version, false, "Stopped trading"); err != nil || x.Status != "inactive" {
		t.Fatalf("deactivate = %v, %v", x.Status, err)
	}
}

func TestPriceRequestJourney(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	alice := e.actor(t, "alice@dos.test", auth.RoleSalesExecutive)
	bob := e.actor(t, "bob@dos.test", auth.RoleSalesExecutive)

	// Suppliers are added by operations; sales executives ask them for prices.
	ops := e.actor(t, "ops@dos.test", auth.RoleOperations)
	mk := func(name, region, cur, email string) sourcing.Supplier {
		x, err := e.svc.CreateSupplier(ops, sourcing.SupplierInput{Name: name, Accounts: []sourcing.AccountInput{
			{Region: region, Currency: cur, ContactName: str("Sam Sales"), ContactEmail: email, OtherEmails: []string{"orders@" + strings.Split(email, "@")[1]}, AccountNumber: str("DOS1")}}})
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	tarsus := mk("Tarsus", "ZA", "ZAR", "sales@tarsus.test")
	mustek := mk("Mustek", "ZA", "ZAR", "sales@mustek.test")
	uk := mk("Sourcing IT", "GB", "USD", "jack@sourcing-it.test")

	cl, _ := e.clients.Create(alice, clients.CreateInput{LegalName: "Zambia Sugar"})
	// Mistakes are caught.
	if _, err := e.svc.CreateRequest(alice, sourcing.RequestInput{Title: "", Items: []sourcing.ItemInput{{Description: "", Quantity: "0"}}}); code(err) != "validation_failed" {
		t.Fatalf("bad request err = %v", err)
	}
	needed := time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	r, err := e.svc.CreateRequest(alice, sourcing.RequestInput{Title: "Laptops for Zambia Sugar", NeededBy: &needed, ClientID: &cl.ID,
		Notes: str("Delivery to Lusaka."),
		Items: []sourcing.ItemInput{
			{Description: "HP ProBook 450 G10", Brand: str("HP"), PartNo: str("85B70EA"), Quantity: "10"},
			{Description: "HP USB-C dock G5", Brand: str("HP"), Quantity: "10"},
		}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r.Reference, "DOS-RFQ-") || r.Status != sourcing.StatusDraft || len(r.Items) != 2 || r.Client == nil || r.Client.Name != "Zambia Sugar" {
		t.Fatalf("new request = %+v", r)
	}

	// Bob can't touch it.
	if _, err := e.svc.Send(bob, r.ID, sourcing.SendInput{Version: r.Version, AccountIDs: []uuid.UUID{tarsus.Accounts[0].ID}, Email: true}); code(err) != "not_your_request" {
		t.Fatalf("bob send err = %v", err)
	}
	// Email the two SA distributors (with their other addresses copied) and record the UK one as asked on WhatsApp.
	res, err := e.svc.Send(alice, r.ID, sourcing.SendInput{Version: r.Version, AccountIDs: []uuid.UUID{tarsus.Accounts[0].ID, mustek.Accounts[0].ID},
		Email: true, CopyOthers: true, Message: "Urgent please."})
	if err != nil || len(res.Sent) != 2 || len(res.Failed) != 0 || res.Request.Status != sourcing.StatusSent {
		t.Fatalf("send = %+v, %v", res, err)
	}
	var subj string
	var n int
	_ = e.pool.QueryRow(ctx, `SELECT count(*), max(subject) FROM email_log WHERE kind='price_request'`).Scan(&n, &subj)
	if n != 2 || !strings.Contains(subj, r.Reference) {
		t.Fatalf("emails = %d %q", n, subj)
	}
	r = res.Request
	res, err = e.svc.Send(alice, r.ID, sourcing.SendInput{Version: r.Version, AccountIDs: []uuid.UUID{uk.Accounts[0].ID, tarsus.Accounts[0].ID}, Via: "WhatsApp"})
	if err != nil || len(res.Sent) != 1 || len(res.Request.Invites) != 3 {
		t.Fatalf("second send (tarsus skipped) = %+v, %v", res.Sent, err)
	}
	r = res.Request
	inv := map[string]uuid.UUID{}
	for _, v := range r.Invites {
		inv[v.Supplier.Name] = v.ID
	}
	laptop, dock := r.Items[0].ID, r.Items[1].ID

	reply := func(name, cur string, prices map[uuid.UUID]string) {
		t.Helper()
		in := sourcing.ReplyInput{Version: r.Version, Currency: cur, LeadTime: str("In stock")}
		for id, p := range prices {
			in.Prices = append(in.Prices, struct {
				ItemID       uuid.UUID `json:"item_id"`
				UnitCost     *string   `json:"unit_cost"`
				Availability *string   `json:"availability"`
			}{ItemID: id, UnitCost: str(p)})
		}
		var err error
		if r, err = e.svc.RecordReply(alice, r.ID, inv[name], in); err != nil {
			t.Fatalf("reply %s: %v", name, err)
		}
	}
	reply("Tarsus", "ZAR", map[uuid.UUID]string{laptop: "15,200.00", dock: "2,900"})
	reply("Mustek", "ZAR", map[uuid.UUID]string{laptop: "14,950", dock: ""}) // can't do the dock
	reply("Sourcing IT", "USD", map[uuid.UUID]string{laptop: "790", dock: "150"})

	// Items can't change after replies.
	in := sourcing.RequestInput{Version: r.Version, Title: r.Title, Items: []sourcing.ItemInput{{Description: "Something else", Quantity: "1"}}}
	if _, err := e.svc.UpdateRequest(alice, r.ID, in); code(err) != "has_replies" {
		t.Fatalf("edit items after reply err = %v", err)
	}

	// Without rates, ZAR and USD can't be compared in kwacha.
	find := func(name string) sourcing.Invite {
		for _, v := range r.Invites {
			if v.Supplier.Name == name {
				return v
			}
		}
		t.Fatalf("no invite %s", name)
		return sourcing.Invite{}
	}
	if v := find("Tarsus"); v.Total == nil || *v.Total != "181000.00" || v.TotalZMW != nil || !v.Complete {
		t.Fatalf("tarsus no rates = %+v", v)
	}
	if len(r.Rates) != 2 || r.Rates[0].RateToZMW != "" {
		t.Fatalf("rates needed = %+v", r.Rates)
	}
	// With rates: ZAR 1.48, USD 26.85.
	if _, err := e.svc.SetRate(alice, "ZAR", "1.48"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetRate(alice, "USD", "26.85"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.SetRate(alice, "ZMW", "1"); code(err) != "validation_failed" {
		t.Fatalf("ZMW rate err = %v", err)
	}
	r, _ = e.svc.GetRequest(ctx, r.ID)
	// Tarsus 181,000 ZAR × 1.48 = 267,880; Sourcing IT 9,400 USD × 26.85 = 252,390 → cheapest complete.
	// Per item: laptop cheapest is Sourcing IT (790 × 26.85 = 21,211.50 each vs Mustek 14,950 × 1.48 = 22,126).
	t1, uk1 := find("Tarsus"), find("Sourcing IT")
	if *t1.TotalZMW != "267880.00" || *uk1.TotalZMW != "252390.00" || !uk1.Cheapest || t1.Cheapest || find("Mustek").Complete {
		t.Fatalf("compare = tarsus %v uk %v (cheapest %v)", *t1.TotalZMW, *uk1.TotalZMW, uk1.Cheapest)
	}
	if r.Items[0].BestInviteID == nil || *r.Items[0].BestInviteID != inv["Sourcing IT"] {
		t.Fatalf("best for laptop = %v", r.Items[0].BestInviteID)
	}

	// Choose: Mustek can't be chosen for the dock it didn't price.
	choice := func(item uuid.UUID, invite uuid.UUID) struct {
		ItemID   uuid.UUID  `json:"item_id"`
		InviteID *uuid.UUID `json:"invite_id"`
	} {
		return struct {
			ItemID   uuid.UUID  `json:"item_id"`
			InviteID *uuid.UUID `json:"invite_id"`
		}{item, &invite}
	}
	bad := sourcing.DecideInput{Version: r.Version}
	bad.Choices = append(bad.Choices, choice(dock, inv["Mustek"]))
	if _, err := e.svc.Decide(alice, r.ID, bad); code(err) != "validation_failed" {
		t.Fatalf("choose unpriced err = %v", err)
	}
	ok := sourcing.DecideInput{Version: r.Version, Note: "Mustek is local stock"}
	ok.Choices = append(ok.Choices, choice(laptop, inv["Mustek"]), choice(dock, inv["Tarsus"]))
	r, err = e.svc.Decide(alice, r.ID, ok)
	if err != nil || r.Status != sourcing.StatusDecided || r.Chosen != 2 || r.ItemsToPick != 0 {
		t.Fatalf("decide = %v %d, %v", r.Status, r.Chosen, err)
	}
	// If Tarsus later says it can't, its choice is cleared.
	r, err = e.svc.RecordReply(alice, r.ID, inv["Tarsus"], sourcing.ReplyInput{Version: r.Version, Declined: true, Note: str("Out of stock")})
	if err != nil || r.Chosen != 1 {
		t.Fatalf("decline clears choice = %d, %v", r.Chosen, err)
	}

	// The supplier list shows how often they were asked and answered.
	sp, _ := e.svc.GetSupplier(ctx, uk.ID)
	if sp.Asked != 1 || sp.Replied != 1 {
		t.Fatalf("stats = %d/%d", sp.Asked, sp.Replied)
	}
	evs, _ := e.svc.RequestActivity(ctx, r.ID)
	if len(evs) < 7 {
		t.Fatalf("activity = %d events", len(evs))
	}
	if _, err := e.svc.Cancel(alice, r.ID, r.Version, ""); code(err) != "validation_failed" {
		t.Fatalf("cancel no reason err = %v", err)
	}
}

func TestPriceRequestReminders(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	alice := e.actor(t, "alice@dos.test", auth.RoleSalesExecutive)
	ops := e.actor(t, "ops@dos.test", auth.RoleOperations)
	sp, err := e.svc.CreateSupplier(ops, sourcing.SupplierInput{Name: "Rectron", Accounts: []sourcing.AccountInput{{Region: "ZA", Currency: "ZAR", ContactEmail: "s@rectron.test"}}})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := e.svc.CreateRequest(alice, sourcing.RequestInput{Title: "Toner", Items: []sourcing.ItemInput{{Description: "HP 59A toner", Quantity: "5"}}})
	if _, err := e.svc.Send(alice, r.ID, sourcing.SendInput{Version: r.Version, AccountIDs: []uuid.UUID{sp.Accounts[0].ID}, Email: true}); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.svc.Remind(ctx, time.Now()); n != 0 {
		t.Fatalf("too early = %d", n)
	}
	later := time.Now().Add(3 * 24 * time.Hour)
	if n, err := e.svc.Remind(ctx, later); err != nil || n != 1 {
		t.Fatalf("reminders = %d, %v", n, err)
	}
	if n, _ := e.svc.Remind(ctx, later); n != 0 {
		t.Fatalf("repeat = %d", n)
	}
	items, _, _ := notify.New(e.pool).List(ctx, me(alice), 10)
	if len(items) != 1 || items[0].Kind != sourcing.KindNoReply {
		t.Fatalf("notices = %+v", items)
	}
}
