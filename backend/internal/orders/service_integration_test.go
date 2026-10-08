package orders_test

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
	"dos-bpas/internal/orders"
	ordermig "dos-bpas/internal/orders/migrations"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/events"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/migrate"
	platmig "dos-bpas/internal/platform/migrations"
	"dos-bpas/internal/platform/notify"
	"dos-bpas/internal/platform/numbering"
	"dos-bpas/internal/quotes"
	quotemig "dos-bpas/internal/quotes/migrations"
	"dos-bpas/internal/users"
)

// Run with: TEST_DATABASE_URL=postgres://... go test -p 1 ./...

type env struct {
	pool    *pgxpool.Pool
	clients *clients.Service
	deals   *deals.Service
	quotes  *quotes.Service
	orders  *orders.Service
	users   *users.Service
	loc     *time.Location
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
		{Module: "deals", FS: dealmig.FS}, {Module: "quotes", FS: quotemig.FS}, {Module: "orders", FS: ordermig.FS},
	}); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Africa/Lusaka")
	nums, bus := numbering.New(loc), events.NewBus(log)
	cl := clients.NewService(pool, nums, bus, time.Now)
	dl := deals.NewService(pool, nums, bus, cl, time.Now)
	qs := quotes.NewService(pool, nums, cl, dl, mail.New(mail.Config{}, pool, log), time.Now, quotes.Options{ValidDays: 30, ApprovalDiscount: 10, Location: loc, Log: log})
	os := orders.NewService(pool, nums, cl, qs, dl, time.Now, orders.Options{Location: loc, Log: log})
	return env{pool: pool, clients: cl, deals: dl, quotes: qs, orders: os, users: users.NewService(pool, time.Now), loc: loc}
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

func notices(t *testing.T, e env, ctx context.Context) map[string]int {
	t.Helper()
	items, _, err := notify.New(e.pool).List(context.Background(), me(ctx), 100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, n := range items {
		out[n.Kind]++
	}
	return out
}

func (e env) day(n int) string { return time.Now().In(e.loc).AddDate(0, 0, n).Format("2006-01-02") }

// acceptedQuote makes a quotation for the client and takes it to accepted.
func (e env) acceptedQuote(t *testing.T, who context.Context, client uuid.UUID, po string) quotes.Quote {
	t.Helper()
	q, err := e.quotes.Create(who, quotes.QuoteInput{ClientID: client, Title: "Supply of 3 colour copiers", Lines: []quotes.LineInput{
		{Description: "Canon imageRUNNER ADVANCE C3226i", Quantity: "3", UnitPrice: "12500", VATRate: "16"},
		{Description: "Installation and user training", Quantity: "1", Unit: "job", UnitPrice: "1500", VATRate: "16"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if q, err = e.quotes.Submit(who, q.ID, q.Version); err != nil {
		t.Fatal(err)
	}
	res, err := e.quotes.Send(who, q.ID, quotes.SendInput{Version: q.Version, Via: "Hand delivered"})
	if err != nil {
		t.Fatal(err)
	}
	if q, err = e.quotes.Accept(who, q.ID, res.Quote.Version, po, ""); err != nil {
		t.Fatal(err)
	}
	return q
}

func lineIDs(o orders.Order) (copier, install uuid.UUID) {
	return o.Lines[0].ID, o.Lines[1].ID
}

func TestOrderJourney(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	alice := e.actor(t, "alice@dos.test", auth.RoleSalesExecutive)
	bob := e.actor(t, "bob@dos.test", auth.RoleSalesExecutive)
	ops := e.actor(t, "ops@dos.test", auth.RoleOperations)
	fin := e.actor(t, "fin@dos.test", auth.RoleFinance)
	manager := e.actor(t, "manager@dos.test", auth.RoleSalesManager)

	cl, err := e.clients.Create(alice, clients.CreateInput{LegalName: "Zambia National Commercial Bank",
		Sites:    []clients.SiteInput{{Name: "Head office", Address: "Cairo Road, Lusaka"}},
		Contacts: []clients.ContactInput{{Name: "Mutale Banda", IsPrimary: true}}})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := e.clients.Create(alice, clients.CreateInput{LegalName: "Other Co", Sites: []clients.SiteInput{{Name: "Ndola", Address: "Ndola"}}})
	q := e.acceptedQuote(t, alice, cl.ID, "LPO-778")

	// Only accepted quotations, and only the person who prepared them (or a manager).
	draft, _ := e.quotes.Create(alice, quotes.QuoteInput{ClientID: cl.ID, Title: "Toner", Lines: []quotes.LineInput{{Description: "Toner", Quantity: "1", UnitPrice: "100"}}})
	if _, err := e.orders.Create(alice, orders.CreateInput{QuoteID: draft.ID}); code(err) != "quote_not_accepted" {
		t.Fatalf("draft quote err = %v", err)
	}
	if _, err := e.orders.Create(bob, orders.CreateInput{QuoteID: q.ID}); code(err) != "not_your_quote" {
		t.Fatalf("bob err = %v", err)
	}
	// Mistakes are caught: a promised date in the past, another client's site.
	if _, err := e.orders.Create(alice, orders.CreateInput{QuoteID: q.ID, DeliveryDue: str(e.day(-1))}); code(err) != "validation_failed" {
		t.Fatalf("past due err = %v", err)
	}
	if _, err := e.orders.Create(alice, orders.CreateInput{QuoteID: q.ID, SiteID: &other.Sites[0].ID}); code(err) != "validation_failed" {
		t.Fatalf("other site err = %v", err)
	}

	due := e.day(10)
	o, err := e.orders.Create(alice, orders.CreateInput{QuoteID: q.ID, DeliveryDue: &due, SiteID: &cl.Sites[0].ID,
		DeliveryInstructions: str("Deliver to the 3rd floor, ask for Mutale")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(o.Reference, "DOS-SO-") || o.Status != orders.StatusOpen || o.ClientPO == nil || *o.ClientPO != "LPO-778" ||
		o.Total != q.Total || len(o.Lines) != 2 || o.Quote == nil || o.Quote.Number != q.Number || o.Site == nil {
		t.Fatalf("new order = %+v", o)
	}
	if o.Lines[0].Source != orders.SourceSupplier || o.Lines[1].Source != orders.SourceService {
		t.Fatalf("guessed sources = %s, %s", o.Lines[0].Source, o.Lines[1].Source)
	}
	if n := notices(t, e, ops); n[orders.KindNewOrder] != 1 {
		t.Fatalf("operations notices = %v", n)
	}
	// One order per quotation.
	if _, err := e.orders.Create(alice, orders.CreateInput{QuoteID: q.ID}); code(err) != "order_exists" {
		t.Fatalf("second order err = %v", err)
	}
	copier, install := lineIDs(o)

	// Progress: operations orders the copiers; Bob (not the owner) can't touch it.
	if _, err := e.orders.Progress(bob, o.ID, orders.ProgressInput{Version: o.Version, Lines: []orders.LineProgress{{ID: copier, QtyReady: str("1")}}}); code(err) != "not_your_order" {
		t.Fatalf("bob progress err = %v", err)
	}
	if _, err := e.orders.Progress(ops, o.ID, orders.ProgressInput{Version: o.Version, Lines: []orders.LineProgress{{ID: copier, QtyReady: str("4")}}}); code(err) != "validation_failed" {
		t.Fatalf("too many ready err = %v", err)
	}
	o, err = e.orders.Progress(ops, o.ID, orders.ProgressInput{Version: o.Version, Lines: []orders.LineProgress{
		{ID: copier, SupplierNote: str("Ordered from Canon SA, ETA 20 Oct"), QtyReady: str("2")}}})
	if err != nil || o.Status != orders.StatusInProgress || o.Lines[0].QtyReady != "2" || o.ReadyPct != 64 { // 2 of 3 copiers = 25,000 of 39,000 by value
		t.Fatalf("progress = %v %+v, %v", o.Status, o.Lines, err)
	}
	if n := notices(t, e, alice); n[orders.KindChanged] != 1 {
		t.Fatalf("owner told of progress = %v", n)
	}

	// Moving the promised date needs a reason.
	later := e.day(14)
	if _, err := e.orders.Update(alice, o.ID, orders.UpdateInput{Version: o.Version, DeliveryDue: &later}); code(err) != "validation_failed" {
		t.Fatalf("move without reason err = %v", err)
	}
	o, err = e.orders.Update(alice, o.ID, orders.UpdateInput{Version: o.Version, DeliveryDue: &later, Reason: "Canon shipment delayed"})
	if err != nil || o.DeliveryDue == nil || *o.DeliveryDue != later {
		t.Fatalf("move = %v, %v", o.DeliveryDue, err)
	}

	// Delivery of 2 copiers: part delivered; can't over-deliver or cancel any more.
	o, err = e.orders.RecordDelivery(ops, o.ID, orders.DeliveryInput{Version: o.Version, DeliveredOn: e.day(0), Reference: "DN 0042", ReceivedBy: "Mutale Banda",
		Lines: []struct {
			ID       uuid.UUID `json:"id"`
			Quantity string    `json:"quantity"`
		}{{copier, "2"}}})
	if err != nil || o.Status != orders.StatusPartDelivered || o.Lines[0].QtyDelivered != "2" || len(o.Deliveries) != 1 || o.Deliveries[0].Lines[0].Quantity != "2" {
		t.Fatalf("part delivery = %v %+v, %v", o.Status, o.Lines, err)
	}
	over := orders.DeliveryInput{Version: o.Version, DeliveredOn: e.day(0), Lines: []struct {
		ID       uuid.UUID `json:"id"`
		Quantity string    `json:"quantity"`
	}{{copier, "2"}}}
	if _, err := e.orders.RecordDelivery(ops, o.ID, over); code(err) != "validation_failed" {
		t.Fatalf("over-deliver err = %v", err)
	}
	if _, err := e.orders.Cancel(alice, o.ID, o.Version, "Changed their mind"); code(err) != "already_delivered" {
		t.Fatalf("cancel after delivery err = %v", err)
	}

	// On hold: nothing can be delivered until it's resumed.
	if o, err = e.orders.Hold(alice, o.ID, o.Version, "Waiting for the client's deposit"); err != nil || o.Status != orders.StatusOnHold {
		t.Fatalf("hold = %v, %v", o.Status, err)
	}
	over.Version = o.Version
	over.Lines[0].Quantity = "1"
	if _, err := e.orders.RecordDelivery(ops, o.ID, over); code(err) != "order_on_hold" {
		t.Fatalf("deliver on hold err = %v", err)
	}
	if o, err = e.orders.Resume(alice, o.ID, o.Version); err != nil || o.Status != orders.StatusPartDelivered {
		t.Fatalf("resume = %v, %v", o.Status, err)
	}

	// The rest: delivered, and finance hears it's ready to invoice.
	o, err = e.orders.RecordDelivery(ops, o.ID, orders.DeliveryInput{Version: o.Version, DeliveredOn: e.day(0), Lines: []struct {
		ID       uuid.UUID `json:"id"`
		Quantity string    `json:"quantity"`
	}{{copier, "1"}, {install, "1"}}})
	if err != nil || o.Status != orders.StatusDelivered || o.DeliveredPct != 100 || o.DeliveredAt == nil {
		t.Fatalf("full delivery = %v %d, %v", o.Status, o.DeliveredPct, err)
	}
	if n := notices(t, e, fin); n[orders.KindDelivered] != 1 {
		t.Fatalf("finance notices = %v", n)
	}
	sum, err := e.orders.Summary(ctx)
	if err != nil || sum.ToInvoice != 1 || sum.ToInvoiceValue != o.Total {
		t.Fatalf("summary = %+v, %v", sum, err)
	}

	// Invoicing: finance only; then the order is closed.
	if _, err := e.orders.MarkInvoiced(alice, o.ID, o.Version, "INV-1001", ""); code(err) != "forbidden" {
		t.Fatalf("alice invoice err = %v", err)
	}
	if _, err := e.orders.MarkInvoiced(fin, o.ID, o.Version, "", ""); code(err) != "validation_failed" {
		t.Fatalf("no invoice number err = %v", err)
	}
	if o, err = e.orders.MarkInvoiced(fin, o.ID, o.Version, "INV-1001", ""); err != nil || o.Status != orders.StatusInvoiced || o.InvoicedOn == nil {
		t.Fatalf("invoiced = %v, %v", o.Status, err)
	}
	if _, err := e.orders.Update(manager, o.ID, orders.UpdateInput{Version: o.Version, ClientPO: str("x")}); code(err) != "order_closed" {
		t.Fatalf("edit closed err = %v", err)
	}

	// The history tells the story.
	evs, _ := e.orders.Activity(ctx, o.ID)
	actions := map[string]int{}
	for _, ev := range evs {
		actions[ev.Action]++
	}
	for _, a := range []string{"created", "progress", "delivery_date_moved", "delivered", "held", "resumed", "invoiced"} {
		if actions[a] == 0 {
			t.Fatalf("activity missing %q: %v", a, actions)
		}
	}

	// A cancelled order frees its quotation for a new one.
	q2 := e.acceptedQuote(t, alice, cl.ID, "")
	o2, err := e.orders.Create(manager, orders.CreateInput{QuoteID: q2.ID})
	if err != nil || o2.Owner.ID != me(alice) {
		t.Fatalf("manager-created order = %+v, %v", o2.Owner, err)
	}
	if _, err := e.orders.Cancel(alice, o2.ID, o2.Version, ""); code(err) != "validation_failed" {
		t.Fatalf("cancel without reason err = %v", err)
	}
	if o2, err = e.orders.Cancel(alice, o2.ID, o2.Version, "Client re-tendered"); err != nil || o2.Status != orders.StatusCancelled {
		t.Fatalf("cancel = %v, %v", o2.Status, err)
	}
	if _, err := e.orders.Create(alice, orders.CreateInput{QuoteID: q2.ID}); err != nil {
		t.Fatalf("re-order after cancel: %v", err)
	}

	// Lists.
	live, _ := e.orders.List(ctx, orders.ListFilter{Status: "live", Limit: 50})
	all, _ := e.orders.List(ctx, orders.ListFilter{ClientID: &cl.ID, Limit: 50})
	byQuote, _ := e.orders.List(ctx, orders.ListFilter{QuoteID: &q.ID, Limit: 50})
	found, _ := e.orders.List(ctx, orders.ListFilter{Query: "LPO-778", Limit: 50})
	if live.Total != 1 || all.Total != 3 || byQuote.Total != 1 || found.Total != 1 {
		t.Fatalf("lists live=%d all=%d quote=%d search=%d", live.Total, all.Total, byQuote.Total, found.Total)
	}
}

func TestOrderReminders(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	alice := e.actor(t, "alice@dos.test", auth.RoleSalesExecutive)
	lead := e.actor(t, "lead@dos.test", auth.RoleSalesExecutive)
	manager := e.actor(t, "manager@dos.test", auth.RoleSalesManager)
	if _, err := e.pool.Exec(ctx, `UPDATE users SET manager_id = $2 WHERE id = $1`, me(alice), me(lead)); err != nil {
		t.Fatal(err)
	}
	cl, _ := e.clients.Create(alice, clients.CreateInput{LegalName: "Lafarge Zambia"})
	soon := e.acceptedQuote(t, alice, cl.ID, "")
	late := e.acceptedQuote(t, alice, cl.ID, "")
	d := e.day(1)
	if _, err := e.orders.Create(alice, orders.CreateInput{QuoteID: soon.ID, DeliveryDue: &d}); err != nil {
		t.Fatal(err)
	}
	ol, err := e.orders.Create(alice, orders.CreateInput{QuoteID: late.ID, DeliveryDue: &d})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE customer_orders SET delivery_due = $2 WHERE id = $1`, ol.ID, e.day(-4)); err != nil {
		t.Fatal(err)
	}

	r := orders.NewReminders(e.orders)
	n, err := r.Run(ctx, time.Now())
	if err != nil || n != 3 {
		t.Fatalf("first run = %d, %v", n, err)
	}
	if got := notices(t, e, alice); got[orders.KindDueSoon] != 1 || got[orders.KindOverdue] != 1 {
		t.Fatalf("owner notices = %v", got)
	}
	// Four days late goes to Alice's own manager, not the sales managers.
	if got := notices(t, e, lead); got[orders.KindEscalated] != 1 {
		t.Fatalf("lead notices = %v", got)
	}
	if got := notices(t, e, manager); got[orders.KindEscalated] != 0 {
		t.Fatalf("sales manager notices = %v", got)
	}
	if n, _ := r.Run(ctx, time.Now()); n != 0 {
		t.Fatalf("repeat run = %d", n)
	}
}
