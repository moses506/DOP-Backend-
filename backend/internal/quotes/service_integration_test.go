package quotes_test

import (
	"bytes"
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
	"dos-bpas/internal/platform/config"
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

type env struct {
	pool    *pgxpool.Pool
	clients *clients.Service
	deals   *deals.Service
	quotes  *quotes.Service
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
		{Module: "deals", FS: dealmig.FS}, {Module: "quotes", FS: quotemig.FS},
	}); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Africa/Lusaka")
	nums, bus := numbering.New(loc), events.NewBus(log)
	cl := clients.NewService(pool, nums, bus, time.Now)
	dl := deals.NewService(pool, nums, bus, cl, time.Now)
	qs := quotes.NewService(pool, nums, cl, dl, mail.New(mail.Config{}, pool, log), time.Now, quotes.Options{
		Company: config.Company{Name: "Digital Office Solutions Limited", Tagline: "Business Solutions Beyond Borders", Address: "Lusaka, Zambia",
			Phone:   "+26 0211 258 696",
			BankZMW: "DIGITAL OFFICE SOLUTIONS LIMITED\nFIRST NATIONAL BANK\nACCOUNT NUMBER: 62242123686\nBRANCH CODE: 260001\nBRANCH: COMMERCIAL BRANCH\nSWIFT CODE: FIRNZMLX",
			BankUSD: "DIGITAL OFFICE SOLUTIONS LIMITED\nFIRST NATIONAL BANK\nACCOUNT NUMBER: 62569959649\nBRANCH CODE: 260001\nBRANCH: COMMERCIAL BRANCH\nSWIFT CODE: FIRNZMLX"},
		ApprovalDiscount: 10, ValidDays: 30, Location: loc, Log: log,
	})
	return env{pool: pool, clients: cl, deals: dl, quotes: qs, users: users.NewService(pool, time.Now)}
}

func (e env) actor(t *testing.T, email string, role auth.Role) context.Context {
	t.Helper()
	u, err := e.users.Create(context.Background(), users.CreateInput{
		Email: email, FullName: "Test " + string(role) + " " + strings.Split(email, "@")[0], Position: "Tester", Department: "Sales",
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

func TestQuotationJourney(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	alice := e.actor(t, "alice@dos.test", auth.RoleSalesExecutive)
	bob := e.actor(t, "bob@dos.test", auth.RoleSalesExecutive)
	manager := e.actor(t, "manager@dos.test", auth.RoleSalesManager)

	// The price list.
	copier, err := e.quotes.SaveProduct(manager, nil, quotes.ProductInput{SKU: str("CAN-C3226I"), Name: "Canon imageRUNNER ADVANCE C3226i",
		Category: "copier", UnitPrice: "12,500", VATRate: "16"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.quotes.SaveProduct(manager, nil, quotes.ProductInput{SKU: str("can-c3226i"), Name: "Dup", UnitPrice: "1"}); code(err) != "validation_failed" {
		t.Fatalf("duplicate code err = %v", err)
	}
	if found, _ := e.quotes.Products(ctx, "c3226", false, 10); len(found) != 1 {
		t.Fatalf("search = %d", len(found))
	}

	cl, err := e.clients.Create(alice, clients.CreateInput{LegalName: "Zambia National Commercial Bank",
		BillingAddress: str("Cairo Road\nLusaka"), TPIN: str("1001234567"),
		Contacts: []clients.ContactInput{{Name: "Mutale Banda", Position: str("Procurement"), Email: str("mutale@zanaco.test"), IsPrimary: true}}})
	if err != nil {
		t.Fatal(err)
	}
	deal, err := e.deals.Create(alice, deals.CreateInput{ClientID: cl.ID, Title: "Branch copier refresh"})
	if err != nil {
		t.Fatal(err)
	}

	lines := []quotes.LineInput{
		{ProductID: &copier.ID, SKU: copier.SKU, Description: copier.Name, Quantity: "3", UnitPrice: "12500", DiscountPct: "5", VATRate: "16"},
		{Description: "Installation and user training", Quantity: "1", Unit: "job", UnitPrice: "1500"},
	}
	in := quotes.QuoteInput{ClientID: cl.ID, DealID: &deal.ID, ContactID: &cl.Contacts[0].ID, Title: "Supply of 3 colour copiers", Lines: lines}

	// Mistakes are caught.
	bad := in
	bad.Lines = []quotes.LineInput{{Description: "", Quantity: "0", UnitPrice: "x", DiscountPct: "120"}}
	bad.ValidUntil = "2001-01-01"
	if _, err := e.quotes.Create(alice, bad); code(err) != "validation_failed" {
		t.Fatalf("bad quote err = %v", err)
	}

	q, err := e.quotes.Create(alice, in)
	if err != nil {
		t.Fatal(err)
	}
	// 3 × 12,500 − 5% = 35,625 + 1,500 = 37,125 net; VAT 5,940; total 43,065.
	if q.Subtotal != "39000.00" || q.DiscountTotal != "1875.00" || q.NetTotal != "37125.00" || q.VATTotal != "5940.00" || q.Total != "43065.00" {
		t.Fatalf("totals = %s %s %s %s %s", q.Subtotal, q.DiscountTotal, q.NetTotal, q.VATTotal, q.Total)
	}
	if q.Status != quotes.StatusDraft || q.Currency != "ZMW" || q.PaymentTerms == nil || *q.PaymentTerms != "30 days from date of invoice" || q.Contact == nil || q.Deal == nil || len(q.Lines) != 2 {
		t.Fatalf("new quote = %+v", q)
	}
	if !strings.HasPrefix(q.Number, "DOS-Q-") || q.NeedsApproval {
		t.Fatalf("number %q approval %v", q.Number, q.NeedsApproval)
	}

	// Bob can't touch Alice's quotation.
	if _, err := e.quotes.Submit(bob, q.ID, q.Version); code(err) != "not_your_quote" {
		t.Fatalf("bob err = %v", err)
	}
	// Within the discount limit it's ready straight away; then it can't be edited.
	q, err = e.quotes.Submit(alice, q.ID, q.Version)
	if err != nil || q.Status != quotes.StatusReady {
		t.Fatalf("submit = %v, %v", q.Status, err)
	}
	in.Version = q.Version
	if _, err := e.quotes.Update(alice, q.ID, in); code(err) != "wrong_status" {
		t.Fatalf("edit ready err = %v", err)
	}

	// Sending: email goes with the PDF, the deal moves to Proposal at the quotation's value.
	res, err := e.quotes.Send(alice, q.ID, quotes.SendInput{Version: q.Version, Email: true, CopyMe: true, Message: "Good to see you on Tuesday."})
	if err != nil {
		t.Fatal(err)
	}
	q = res.Quote
	if q.Status != quotes.StatusSent || q.SentTo == nil || *q.SentTo != "mutale@zanaco.test" || res.Emailed {
		t.Fatalf("sent = %v %v emailed=%v", q.Status, q.SentTo, res.Emailed)
	}
	var subject string
	if err := e.pool.QueryRow(ctx, `SELECT subject FROM email_log WHERE kind='quotation'`).Scan(&subject); err != nil || !strings.Contains(subject, q.Number) {
		t.Fatalf("email = %q, %v", subject, err)
	}
	d, _ := e.deals.Get(ctx, deal.ID)
	if d.Stage != deals.StageProposal || d.Value == nil || *d.Value != "43065.00" {
		t.Fatalf("deal after send = %v %v", d.Stage, d.Value)
	}

	// The PDF.
	pdf, name, err := e.quotes.PDF(ctx, q.ID)
	if err != nil || !bytes.HasPrefix(pdf, []byte("%PDF-1.4")) || name != q.Number+".pdf" {
		t.Fatalf("pdf %s, %v", name, err)
	}
	if out := os.Getenv("QUOTE_PDF_OUT"); out != "" {
		_ = os.WriteFile(out, pdf, 0o644)
	}

	// The client asks for a change: revision 2 replaces it.
	r2, err := e.quotes.Revise(alice, q.ID, q.Version)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Revision != 2 || r2.Number != q.Number+"-R2" || r2.Status != quotes.StatusDraft || len(r2.Lines) != 2 || len(r2.Revisions) != 2 {
		t.Fatalf("revision = %+v", r2)
	}
	old, _ := e.quotes.Get(ctx, q.ID)
	if old.Status != quotes.StatusSuperseded {
		t.Fatalf("old status = %s", old.Status)
	}
	if _, err := e.quotes.Revise(alice, old.ID, old.Version); code(err) != "wrong_status" {
		t.Fatalf("revise old err = %v", err)
	}

	// A bigger discount needs a manager.
	in.Version = r2.Version
	in.Lines[0].DiscountPct = "15"
	r2, err = e.quotes.Update(alice, r2.ID, in)
	if err != nil || !r2.NeedsApproval {
		t.Fatalf("update = %v approval=%v", err, r2.NeedsApproval)
	}
	r2, err = e.quotes.Submit(alice, r2.ID, r2.Version)
	if err != nil || r2.Status != quotes.StatusPendingApproval {
		t.Fatalf("submit big discount = %v %v", r2.Status, err)
	}
	if n := notices(t, e, manager); n[quotes.KindApprovalRequested] != 1 {
		t.Fatalf("manager notices = %v", n)
	}
	if _, err := e.quotes.Send(alice, r2.ID, quotes.SendInput{Version: r2.Version, Via: "Hand delivered"}); code(err) != "needs_approval" {
		t.Fatalf("send unapproved err = %v", err)
	}
	if _, err := e.quotes.Decide(alice, r2.ID, r2.Version, true, ""); code(err) != "forbidden" {
		t.Fatalf("self approve err = %v", err)
	}
	if _, err := e.quotes.Decide(manager, r2.ID, r2.Version, false, ""); code(err) != "validation_failed" {
		t.Fatalf("return without note err = %v", err)
	}
	r2, err = e.quotes.Decide(manager, r2.ID, r2.Version, false, "Keep it to 12% please")
	if err != nil || r2.Status != quotes.StatusDraft {
		t.Fatalf("returned = %v %v", r2.Status, err)
	}
	if n := notices(t, e, alice); n[quotes.KindReturned] != 1 {
		t.Fatalf("alice notices = %v", n)
	}
	in.Version, in.Lines[0].DiscountPct = r2.Version, "12"
	if r2, err = e.quotes.Update(alice, r2.ID, in); err != nil {
		t.Fatal(err)
	}
	if r2, err = e.quotes.Submit(alice, r2.ID, r2.Version); err != nil {
		t.Fatal(err)
	}
	r2, err = e.quotes.Decide(manager, r2.ID, r2.Version, true, "OK for a 3-unit order")
	if err != nil || r2.Status != quotes.StatusReady || r2.ApprovedBy == nil {
		t.Fatalf("approved = %v %v", r2.Status, err)
	}

	// Delivered by hand, then accepted: the deal is won at the quotation's value.
	res, err = e.quotes.Send(alice, r2.ID, quotes.SendInput{Version: r2.Version, Via: "Hand delivered to procurement"})
	if err != nil {
		t.Fatal(err)
	}
	r2 = res.Quote
	if _, err := e.quotes.Accept(bob, r2.ID, r2.Version, "", ""); code(err) != "not_your_quote" {
		t.Fatalf("bob accept err = %v", err)
	}
	r2, err = e.quotes.Accept(manager, r2.ID, r2.Version, "ZNC-PO-7781", "Signed copy received")
	if err != nil || r2.Status != quotes.StatusAccepted || r2.ClientPO == nil {
		t.Fatalf("accepted = %v %v", r2.Status, err)
	}
	d, _ = e.deals.Get(ctx, deal.ID)
	if d.Stage != deals.StageWon || *d.Value != r2.Total {
		t.Fatalf("deal after accept = %v %v (quote %s)", d.Stage, *d.Value, r2.Total)
	}
	if n := notices(t, e, alice); n[quotes.KindDecided] != 1 {
		t.Fatalf("alice decided notices = %v", n)
	}
	hist, _ := e.quotes.Activity(ctx, r2.ID)
	actions := map[string]int{}
	for _, h := range hist {
		actions[h.Action]++
	}
	if actions["sent"] != 2 || actions["superseded"] != 1 || actions["returned"] != 1 || actions["approved"] != 1 || actions["accepted"] != 1 {
		t.Fatalf("history = %v", actions)
	}

	// Lists and summary.
	list, err := e.quotes.List(ctx, quotes.ListFilter{Limit: 25})
	if err != nil || list.Total != 1 || list.Items[0].Client.LegalName != cl.LegalName {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if l, _ := e.quotes.List(ctx, quotes.ListFilter{Query: "zanaco national", Limit: 25}); l.Total != 0 {
		t.Fatalf("odd search total = %d", l.Total)
	}
	if l, _ := e.quotes.List(ctx, quotes.ListFilter{Query: "commercial bank", Limit: 25}); l.Total != 1 {
		t.Fatalf("client search total = %d", l.Total)
	}
	sum, err := e.quotes.Summary(manager)
	if err != nil || sum.AcceptedThisMonth != 1 || sum.AcceptedValue["ZMW"] != r2.Total || sum.WinRate != 100 {
		t.Fatalf("summary = %+v, %v", sum, err)
	}
}

func TestDeclineCancelAndExpiry(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	alice := e.actor(t, "alice@dos.test", auth.RoleSalesExecutive)
	cl, err := e.clients.Create(alice, clients.CreateInput{LegalName: "Konkola Copper Mines"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(title string) quotes.Quote {
		q, err := e.quotes.Create(alice, quotes.QuoteInput{ClientID: cl.ID, Title: title,
			Lines: []quotes.LineInput{{Description: "Toner", Quantity: "10", UnitPrice: "850"}}})
		if err != nil {
			t.Fatal(err)
		}
		if q, err = e.quotes.Submit(alice, q.ID, q.Version); err != nil {
			t.Fatal(err)
		}
		return q
	}

	// Cancel needs a reason; a sent one can't be cancelled.
	c := mk("Cancelled one")
	if _, err := e.quotes.Cancel(alice, c.ID, c.Version, ""); code(err) != "validation_failed" {
		t.Fatalf("cancel err = %v", err)
	}
	if c, err = e.quotes.Cancel(alice, c.ID, c.Version, "Client changed their mind"); err != nil || c.Status != quotes.StatusCancelled {
		t.Fatalf("cancel = %v %v", c.Status, err)
	}

	// Decline.
	dq := mk("Declined one")
	res, err := e.quotes.Send(alice, dq.ID, quotes.SendInput{Version: dq.Version, Via: "WhatsApp"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.quotes.Cancel(alice, dq.ID, res.Quote.Version, "x"); code(err) != "wrong_status" {
		t.Fatalf("cancel sent err = %v", err)
	}
	if q, err := e.quotes.Decline(alice, dq.ID, res.Quote.Version, "Went with a competitor"); err != nil || q.Status != quotes.StatusDeclined {
		t.Fatalf("decline = %v", err)
	}

	// Expiry: a sent quotation past its date expires and the preparer hears about it; one ending soon gets a reminder.
	ex := mk("Expiring one")
	res, _ = e.quotes.Send(alice, ex.ID, quotes.SendInput{Version: ex.Version, Via: "Email from Outlook"})
	soon := mk("Ending soon")
	res2, _ := e.quotes.Send(alice, soon.ID, quotes.SendInput{Version: soon.Version, Via: "Email from Outlook"})
	if _, err := e.pool.Exec(ctx, `UPDATE quotes SET quote_date = current_date - 40, valid_until = current_date - 1 WHERE id = $1`, ex.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE quotes SET valid_until = current_date + 2 WHERE id = $1`, soon.ID); err != nil {
		t.Fatal(err)
	}
	_ = res2
	r := quotes.NewReminders(e.quotes)
	out, err := r.Run(ctx, time.Now())
	if err != nil || out.Expired != 1 || out.Notifications != 2 {
		t.Fatalf("run = %+v, %v", out, err)
	}
	if out, _ = r.Run(ctx, time.Now()); out.Expired+out.Notifications != 0 {
		t.Fatalf("second run = %+v", out)
	}
	got, _ := e.quotes.Get(ctx, ex.ID)
	if got.Status != quotes.StatusExpired {
		t.Fatalf("expired status = %s", got.Status)
	}
	if n := notices(t, e, alice); n[quotes.KindExpired] != 1 || n[quotes.KindExpiring] != 1 {
		t.Fatalf("notices = %v", n)
	}
	// An expired quotation can still be accepted if the client comes back, or revised.
	if r2, err := e.quotes.Revise(alice, got.ID, got.Version); err != nil || r2.Revision != 2 {
		t.Fatalf("revise expired = %v", err)
	}
	if again, _ := e.quotes.Get(ctx, ex.ID); again.Status != quotes.StatusExpired {
		t.Fatalf("expired should stay expired after revision, got %s", again.Status)
	}
}
