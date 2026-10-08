package clients_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dos-bpas/internal/clients"
	clientmig "dos-bpas/internal/clients/migrations"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/events"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/migrate"
	platmig "dos-bpas/internal/platform/migrations"
	"dos-bpas/internal/platform/numbering"
	"dos-bpas/internal/users"
)

// Run with: TEST_DATABASE_URL=postgres://... go test ./...
// The test database is wiped on every run.
func setup(t *testing.T) (*pgxpool.Pool, *clients.Service, *users.Service) {
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
	}); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("Africa/Lusaka")
	return pool, clients.NewService(pool, numbering.New(loc), events.NewBus(log), time.Now), users.NewService(pool, time.Now)
}

func actor(t *testing.T, us *users.Service, email string, role auth.Role) context.Context {
	t.Helper()
	u, err := us.Create(context.Background(), users.CreateInput{
		Email: email, FullName: "Test " + string(role), Position: "Tester", Department: "QA",
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

func TestClientLifecycle(t *testing.T) {
	_, svc, us := setup(t)
	sales := actor(t, us, "sales@dos.test", auth.RoleSalesExecutive)
	manager := actor(t, us, "manager@dos.test", auth.RoleSalesManager)

	c, err := svc.Create(sales, clients.CreateInput{
		LegalName: "Kansanshi Mining PLC", TPIN: str("1001234567"),
		Contacts: []clients.ContactInput{{Name: "Jane Phiri", Email: str("jane@kansanshi.test"), IsPrimary: true}},
		Sites:    []clients.SiteInput{{Name: "Main stores", Address: "Solwezi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Reference != "DOS-CL-00001" || c.CreditTermsDays != 30 || c.DefaultCurrency != "ZMW" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if len(c.Contacts) != 1 || len(c.Sites) != 1 || c.Owner.Position != "Tester" {
		t.Fatalf("children/owner not returned: %+v", c)
	}

	// Same company written differently is flagged.
	_, err = svc.Create(sales, clients.CreateInput{LegalName: "kansanshi mining limited"})
	if code(err) != "possible_duplicate" {
		t.Fatalf("want possible_duplicate, got %v", err)
	}
	// Same TPIN under another name is flagged.
	_, err = svc.Create(sales, clients.CreateInput{LegalName: "Other Name", TPIN: str("100-123-4567")})
	if code(err) != "possible_duplicate" {
		t.Fatalf("want possible_duplicate on TPIN, got %v", err)
	}
	// A sales executive cannot override; a manager can, with a reason.
	_, err = svc.Create(sales, clients.CreateInput{LegalName: "Kansanshi Mining Ltd", AllowDuplicate: true, DuplicateReason: "x"})
	if code(err) != "forbidden" {
		t.Fatalf("want forbidden, got %v", err)
	}
	_, err = svc.Create(manager, clients.CreateInput{LegalName: "Kansanshi Mining Ltd", AllowDuplicate: true})
	if code(err) != "validation_failed" {
		t.Fatalf("want validation_failed without reason, got %v", err)
	}
	c2, err := svc.Create(manager, clients.CreateInput{LegalName: "Kansanshi Mining Ltd", AllowDuplicate: true,
		DuplicateReason: "Separate legal entity for the smelter"})
	if err != nil || c2.Reference != "DOS-CL-00002" {
		t.Fatalf("override create: %v %s", err, c2.Reference)
	}

	// Optimistic locking.
	up, err := svc.Update(sales, c.ID, clients.UpdateInput{Version: c.Version, CreditTermsDays: intp(45)})
	if err != nil || up.Version != c.Version+1 || up.CreditTermsDays != 45 {
		t.Fatalf("update: %v %+v", err, up)
	}
	_, err = svc.Update(sales, c.ID, clients.UpdateInput{Version: c.Version, Notes: str("stale")})
	if code(err) != "stale_version" {
		t.Fatalf("want stale_version, got %v", err)
	}

	// TPIN may not be reused on edit.
	_, err = svc.Update(sales, c2.ID, clients.UpdateInput{Version: c2.Version, TPIN: str("1001234567")})
	if code(err) != "validation_failed" {
		t.Fatalf("want TPIN clash, got %v", err)
	}

	// Handover requires a reason.
	_, err = svc.Update(sales, c.ID, clients.UpdateInput{Version: up.Version, OwnerID: &c2.Owner.ID})
	if code(err) != "validation_failed" {
		t.Fatalf("want reason required, got %v", err)
	}
	ho, err := svc.Update(sales, c.ID, clients.UpdateInput{Version: up.Version, OwnerID: &c2.Owner.ID, Reason: "Moved to key accounts"})
	if err != nil || ho.Owner.ID != c2.Owner.ID {
		t.Fatalf("handover: %v", err)
	}

	// Deactivate, then block document issue and child edits.
	d, err := svc.Deactivate(manager, c.ID, ho.Version, "Company closed account")
	if err != nil || d.Status != clients.StatusInactive {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := svc.RequireActive(sales, c.ID); code(err) != "client_inactive" {
		t.Fatalf("want client_inactive, got %v", err)
	}
	if _, err := svc.AddSite(sales, c.ID, clients.SiteInput{Name: "x", Address: "y"}); code(err) != "client_inactive" {
		t.Fatalf("want client_inactive for site, got %v", err)
	}

	// Timeline has every step, newest first.
	evs, err := svc.Activity(sales, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"deactivated", "reassigned", "updated", "created"}
	if len(evs) != len(want) {
		t.Fatalf("timeline has %d events, want %d", len(evs), len(want))
	}
	for i, w := range want {
		if evs[i].Action != w {
			t.Errorf("event %d = %s, want %s", i, evs[i].Action, w)
		}
	}
	if evs[1].Reason != "Moved to key accounts" {
		t.Errorf("handover reason not recorded: %q", evs[1].Reason)
	}

	// Search by name fragment and by TPIN.
	l, err := svc.List(sales, clients.ListFilter{Query: "kansan", Page: httpx.Page{Limit: 10}})
	if err != nil || l.Total != 2 {
		t.Fatalf("search: %v total=%d", err, l.Total)
	}
	l, _ = svc.List(sales, clients.ListFilter{Query: "1001234567", Page: httpx.Page{Limit: 10}})
	if l.Total != 1 {
		t.Fatalf("tpin search total=%d", l.Total)
	}
}

func TestNumbersNeverDuplicateUnderConcurrency(t *testing.T) {
	pool, _, _ := setup(t)
	loc, _ := time.LoadLocation("Africa/Lusaka")
	n := numbering.New(loc)
	ctx := context.Background()

	const workers = 40
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var num string
			err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
				var err error
				if num, err = n.Next(ctx, tx, numbering.Invoice); err != nil {
					return err
				}
				if i%5 == 0 {
					return errors.New("simulated failure") // rolled back: number released for reuse
				}
				return nil
			})
			if err != nil {
				if err.Error() != "simulated failure" {
					t.Error(err)
				}
				return
			}
			// Only committed numbers count: these must all be unique.
			mu.Lock()
			defer mu.Unlock()
			if seen[num] {
				t.Errorf("duplicate committed number %s", num)
			}
			seen[num] = true
		}(i)
	}
	wg.Wait()

	var last int64
	year := time.Now().In(loc).Year()
	if err := pool.QueryRow(ctx, `SELECT last_value FROM number_sequences WHERE doc_type='invoice' AND period=$1`, year).Scan(&last); err != nil {
		t.Fatal(err)
	}
	if len(seen) != workers-workers/5 {
		t.Fatalf("committed %d numbers, want %d", len(seen), workers-workers/5)
	}
	if last != workers-workers/5 {
		t.Fatalf("committed counter = %d, want %d (rolled-back numbers must be released)", last, workers-workers/5)
	}
}

func intp(i int) *int { return &i }
