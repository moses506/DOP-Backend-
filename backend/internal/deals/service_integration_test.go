package deals_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"dos-bpas/internal/clients"
	clientmig "dos-bpas/internal/clients/migrations"
	"dos-bpas/internal/deals"
	dealmig "dos-bpas/internal/deals/migrations"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/events"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/migrate"
	platmig "dos-bpas/internal/platform/migrations"
	"dos-bpas/internal/platform/numbering"
	"dos-bpas/internal/users"
)

// Run with: TEST_DATABASE_URL=postgres://... go test -p 1 ./...
// The test database is wiped on every run.
type env struct {
	pool    *pgxpool.Pool
	clients *clients.Service
	deals   *deals.Service
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
		{Module: "platform", FS: platmig.FS}, {Module: "clients", FS: clientmig.FS}, {Module: "deals", FS: dealmig.FS},
	}); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Africa/Lusaka")
	nums, bus := numbering.New(loc), events.NewBus(log)
	cl := clients.NewService(pool, nums, bus, time.Now)
	return env{pool: pool, clients: cl, deals: deals.NewService(pool, nums, bus, cl, time.Now), users: users.NewService(pool, time.Now)}
}

func (e env) actor(t *testing.T, email string, role auth.Role) context.Context {
	t.Helper()
	u, err := e.users.Create(context.Background(), users.CreateInput{
		Email: email, FullName: "Test " + string(role), Position: "Tester", Department: "Sales",
		Role: role, Password: "correct-horse-battery",
	})
	if err != nil {
		t.Fatal(err)
	}
	return auth.WithPrincipal(context.Background(), u.Principal())
}

func (e env) client(t *testing.T, ctx context.Context, name string) clients.Client {
	t.Helper()
	c, err := e.clients.Create(ctx, clients.CreateInput{LegalName: name})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func code(err error) string {
	var e *httpx.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func str(s string) *string { return &s }

func TestDealLifecycle(t *testing.T) {
	e := setup(t)
	sales := e.actor(t, "sales@dos.test", auth.RoleSalesExecutive)
	cl := e.client(t, sales, "Zambia Sugar Plc")

	d, err := e.deals.Create(sales, deals.CreateInput{ClientID: cl.ID, Title: "Copiers for Nakambala", Value: str("1,250,000")})
	if err != nil {
		t.Fatal(err)
	}
	if d.Stage != deals.StageLead || d.Probability != 10 || *d.Value != "1250000.00" || d.Currency != "ZMW" {
		t.Fatalf("unexpected defaults: %+v", d)
	}
	if d.Client.LegalName != "Zambia Sugar Plc" || d.Reference == "" {
		t.Fatalf("client or reference missing: %+v", d.Client)
	}

	// Moving stage applies that stage's probability.
	d, err = e.deals.Move(sales, d.ID, deals.MoveInput{Version: d.Version, Stage: deals.StageProposal})
	if err != nil {
		t.Fatal(err)
	}
	if d.Probability != 50 {
		t.Fatalf("probability = %d, want 50", d.Probability)
	}

	// Losing needs a reason; then the deal is closed to edits.
	if _, err := e.deals.Move(sales, d.ID, deals.MoveInput{Version: d.Version, Stage: deals.StageLost}); code(err) != "validation_failed" {
		t.Fatalf("lost without reason: %v", err)
	}
	d, err = e.deals.Move(sales, d.ID, deals.MoveInput{Version: d.Version, Stage: deals.StageLost, Reason: "Price"})
	if err != nil {
		t.Fatal(err)
	}
	if d.ClosedAt == nil || d.LostReason == nil || *d.LostReason != "Price" || d.Probability != 0 {
		t.Fatalf("lost deal not closed properly: %+v", d)
	}
	if _, err := e.deals.Update(sales, d.ID, deals.UpdateInput{Version: d.Version, Title: str("x")}); code(err) != "deal_closed" {
		t.Fatalf("edit of closed deal: %v", err)
	}

	// Reopening needs a reason and clears the closing details.
	if _, err := e.deals.Move(sales, d.ID, deals.MoveInput{Version: d.Version, Stage: deals.StageNegotiation}); code(err) != "validation_failed" {
		t.Fatalf("reopen without reason: %v", err)
	}
	d, err = e.deals.Move(sales, d.ID, deals.MoveInput{Version: d.Version, Stage: deals.StageNegotiation, Reason: "Client came back"})
	if err != nil {
		t.Fatal(err)
	}
	if d.ClosedAt != nil || d.LostReason != nil {
		t.Fatalf("reopened deal still closed: %+v", d)
	}

	// A stale copy can't overwrite a newer change.
	if _, err := e.deals.Update(sales, d.ID, deals.UpdateInput{Version: d.Version - 1, Title: str("x")}); code(err) != "stale_version" {
		t.Fatalf("stale update: %v", err)
	}

	// Winning closes it and counts in this month's summary.
	if d, err = e.deals.Move(sales, d.ID, deals.MoveInput{Version: d.Version, Stage: deals.StageWon}); err != nil {
		t.Fatal(err)
	}
	sum, err := e.deals.Summary(sales, uuid0(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.WonThisMonth != 1 || sum.WonValueThisMonth["ZMW"] != "1250000.00" || sum.OpenCount != 0 {
		t.Fatalf("summary: %+v", sum)
	}

	// The history shows every step.
	ev, err := e.deals.Activity(sales, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 5 { // created, proposal, lost, reopened, won
		t.Fatalf("activity entries = %d, want 5", len(ev))
	}
}

func TestDealOwnership(t *testing.T) {
	e := setup(t)
	alice := e.actor(t, "alice@dos.test", auth.RoleSalesExecutive)
	bob := e.actor(t, "bob@dos.test", auth.RoleSalesExecutive)
	manager := e.actor(t, "manager@dos.test", auth.RoleSalesManager)
	cl := e.client(t, alice, "Copperbelt Energy")

	d, err := e.deals.Create(alice, deals.CreateInput{ClientID: cl.ID, Title: "Server room upgrade"})
	if err != nil {
		t.Fatal(err)
	}
	// A colleague can log a note but can't change someone else's deal.
	if _, err := e.deals.AddNote(bob, d.ID, deals.NoteInput{Kind: "call", Body: "Covered a call while Alice was away"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deals.Move(bob, d.ID, deals.MoveInput{Version: d.Version, Stage: deals.StageQualified}); code(err) != "not_your_deal" {
		t.Fatalf("other exec moved the deal: %v", err)
	}
	// Only a manager hands a deal over, and needs a reason.
	bobID := principal(bob).UserID
	if _, err := e.deals.Update(alice, d.ID, deals.UpdateInput{Version: d.Version, OwnerID: &bobID, Reason: "leave"}); code(err) != "forbidden" {
		t.Fatalf("exec reassigned: %v", err)
	}
	if _, err := e.deals.Update(manager, d.ID, deals.UpdateInput{Version: d.Version, OwnerID: &bobID}); code(err) != "validation_failed" {
		t.Fatalf("handover without reason: %v", err)
	}
	d, err = e.deals.Update(manager, d.ID, deals.UpdateInput{Version: d.Version, OwnerID: &bobID, Reason: "Alice on leave"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Owner.ID != bobID {
		t.Fatalf("owner = %v", d.Owner.ID)
	}
	// An exec can't open a deal in someone else's name.
	aliceID := principal(alice).UserID
	if _, err := e.deals.Create(bob, deals.CreateInput{ClientID: cl.ID, Title: "x", OwnerID: &aliceID}); code(err) != "validation_failed" {
		t.Fatalf("exec created for someone else: %v", err)
	}
}

func TestDealNeedsActiveClient(t *testing.T) {
	e := setup(t)
	manager := e.actor(t, "manager@dos.test", auth.RoleSalesManager)
	cl := e.client(t, manager, "Old Client Ltd")
	if _, err := e.clients.Deactivate(manager, cl.ID, cl.Version, "Closed down"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deals.Create(manager, deals.CreateInput{ClientID: cl.ID, Title: "x"}); code(err) != "client_inactive" {
		t.Fatalf("deal for inactive client: %v", err)
	}
}

func TestDealSearchFindsClientName(t *testing.T) {
	e := setup(t)
	sales := e.actor(t, "sales@dos.test", auth.RoleSalesExecutive)
	cl := e.client(t, sales, "Lusaka Water Supply")
	if _, err := e.deals.Create(sales, deals.CreateInput{ClientID: cl.ID, Title: "Meter readers"}); err != nil {
		t.Fatal(err)
	}
	out, err := e.deals.List(sales, deals.ListFilter{Query: "water", Page: httpx.Page{Limit: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Total != 1 || out.Items[0].Client.LegalName != "Lusaka Water Supply" {
		t.Fatalf("search by client name: %+v", out)
	}
}

func principal(ctx context.Context) auth.Principal {
	p, _ := auth.FromContext(ctx)
	return p
}

func uuid0() (id [16]byte) { return }
