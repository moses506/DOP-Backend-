// Command admin runs maintenance tasks.
//
// Create a user (the password is read from ADMIN_PASSWORD or prompted for, so it
// never appears in shell history):
//
//	go run ./cmd/admin create-user -email admin@dos.co.zm -name "System Admin" \
//	    -position "Systems Developer" -department "IT" -role admin
//
// Import clients prepared by scripts/import/clean_client_list.py:
//
//	go run ./cmd/admin import-clients -file clients-import.json -as admin@dos.co.zm [-dry-run]
//
// Import suppliers from the sourcing log (scripts/import/suppliers-import.json):
//
//	go run ./cmd/admin import-suppliers -file scripts/import/suppliers-import.json -as admin@dos.co.zm [-dry-run]
//
// The import goes through the normal clients service, so every client gets a
// server-allocated reference, duplicate checking and an audit entry. Rows that
// look like an existing client are skipped, which makes re-running safe.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dos-bpas/internal/app"
	"dos-bpas/internal/clients"
	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/config"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/events"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/mail"
	"dos-bpas/internal/platform/numbering"
	"dos-bpas/internal/sourcing"
	"dos-bpas/internal/users"
)

const usage = `usage:
  admin create-user -email E -name N -position P -department D -role R
  admin import-clients -file clients-import.json -as ADMIN_EMAIL [-dry-run]
  admin import-suppliers -file scripts/import/suppliers-import.json -as ADMIN_EMAIL [-dry-run]`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "create-user":
		err = runCreateUser(os.Args[2:])
	case "import-suppliers":
		err = runImportSuppliers(os.Args[2:])
	case "import-clients":
		err = runImportClients(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", describe(err))
		os.Exit(1)
	}
}

// describe adds field-level details to validation errors so the cause is visible.
func describe(err error) string {
	var he *httpx.Error
	if errors.As(err, &he) {
		if fe, ok := he.Details.(httpx.FieldErrors); ok && len(fe) > 0 {
			keys := make([]string, 0, len(fe))
			for k := range fe {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, k+": "+fe[k])
			}
			return he.Message + " (" + strings.Join(parts, "; ") + ")"
		}
		return he.Message
	}
	return err.Error()
}

func connect(ctx context.Context) (config.Config, *pgxpool.Pool, *slog.Logger, error) {
	cfg, err := config.Load()
	if err != nil {
		return cfg, nil, nil, err
	}
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return cfg, nil, nil, err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := app.New(cfg, pool, log).Migrate(ctx); err != nil {
		pool.Close()
		return cfg, nil, nil, err
	}
	return cfg, pool, log, nil
}

// ---------------------------------------------------------------- create-user

func runCreateUser(args []string) error {
	fs := flag.NewFlagSet("create-user", flag.ExitOnError)
	email := fs.String("email", "", "email address")
	name := fs.String("name", "", "full name")
	position := fs.String("position", "", "job title")
	dept := fs.String("department", "", "department")
	role := fs.String("role", "admin", "role: ceo|sales_manager|sales_executive|operations|technical|finance|admin")
	_ = fs.Parse(args)

	ctx := context.Background()
	_, pool, _, err := connect(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	pw := os.Getenv("ADMIN_PASSWORD")
	if pw == "" {
		fmt.Print("Password (min 10 chars): ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		pw = strings.TrimSpace(line)
	}
	u, err := users.NewService(pool, nil).Create(ctx, users.CreateInput{
		Email: *email, FullName: *name, Position: *position, Department: *dept, Role: auth.Role(*role), Password: pw,
	})
	if err != nil {
		return err
	}
	fmt.Printf("created %s (%s) id=%s\n", u.Email, u.Role, u.ID)
	return nil
}

// ------------------------------------------------------------- import-clients

type importFile struct {
	Source string `json:"source"`
	Sheet  string `json:"sheet"`
	Owners []struct {
		Key   string `json:"key"`
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"owners"`
	Clients []struct {
		Row    int                 `json:"row"`
		Owner  string              `json:"owner"`
		Client clients.CreateInput `json:"client"`
	} `json:"clients"`
}

func runImportClients(args []string) error {
	fs := flag.NewFlagSet("import-clients", flag.ExitOnError)
	file := fs.String("file", "clients-import.json", "import file from clean_client_list.py")
	as := fs.String("as", "", "email of the admin user recorded as creator in the audit log")
	dryRun := fs.Bool("dry-run", false, "check everything and report, but write nothing")
	_ = fs.Parse(args)
	if *as == "" {
		return errors.New("-as is required (the admin user the import runs as)")
	}

	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var in importFile
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("read %s: %w", *file, err)
	}

	ctx := context.Background()
	cfg, pool, log, err := connect(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	userSvc := users.NewService(pool, time.Now)
	admin, err := userByEmail(ctx, pool, userSvc, *as)
	if err != nil {
		return fmt.Errorf("-as %s: %w", *as, err)
	}
	if !admin.Principal().Can(clients.PermCreate) {
		return fmt.Errorf("%s (%s) cannot create clients", admin.Email, admin.Role)
	}
	ctx = auth.WithPrincipal(ctx, admin.Principal())

	// Sales executives named in the file become the clients' responsible people.
	owners := map[string]uuid.UUID{}
	for _, o := range in.Owners {
		u, err := userByEmail(ctx, pool, userSvc, o.Email)
		switch {
		case err == nil:
			owners[o.Key] = u.ID
		case errors.Is(err, pgx.ErrNoRows) && *dryRun:
			fmt.Printf("would create sales executive %s <%s>\n", o.Name, o.Email)
			owners[o.Key] = admin.ID
		case errors.Is(err, pgx.ErrNoRows):
			pw := randomPassword()
			u, err := userSvc.Create(ctx, users.CreateInput{
				Email: o.Email, FullName: o.Name, Position: "Sales Executive",
				Department: "Sales", Role: auth.RoleSalesExecutive, Password: pw,
			})
			if err != nil {
				return fmt.Errorf("create user %s: %s", o.Email, describe(err))
			}
			fmt.Printf("created sales executive %-14s %-28s password: %s\n", o.Name, o.Email, pw)
			owners[o.Key] = u.ID
		default:
			return err
		}
	}

	svc := clients.NewService(pool, numbering.New(cfg.Location), events.NewBus(log), time.Now)
	var created, skippedDup, failed int
	fmt.Printf("\nImporting %d clients from %s (%s)%s\n\n", len(in.Clients), in.Source, in.Sheet,
		map[bool]string{true: " – DRY RUN, nothing is saved", false: ""}[*dryRun])

	for _, rec := range in.Clients {
		c := rec.Client
		label := fmt.Sprintf("row %-4d %-36.36s", rec.Row, c.LegalName)
		if id, ok := owners[rec.Owner]; ok {
			c.OwnerID = &id
		}

		dups, err := svc.CheckDuplicates(ctx, c.LegalName, c.TPIN, c.RegistrationNo)
		if err != nil {
			return err
		}
		if len(dups) > 0 {
			skippedDup++
			fmt.Printf("SKIP  %s already exists as %s – %s\n", label, dups[0].Reference, dups[0].LegalName)
			continue
		}
		if *dryRun {
			created++
			fmt.Printf("OK    %s %d contacts\n", label, len(c.Contacts))
			continue
		}

		got, err := svc.Create(ctx, c)
		if err != nil {
			failed++
			fmt.Printf("FAIL  %s %s\n", label, describe(err))
			continue
		}
		created++
		fmt.Printf("OK    %s %s, %d contacts\n", label, got.Reference, len(got.Contacts))
	}

	verb := "created"
	if *dryRun {
		verb = "would be created"
	}
	fmt.Printf("\n%d %s, %d skipped as already registered, %d failed\n", created, verb, skippedDup, failed)
	if failed > 0 {
		return fmt.Errorf("%d clients failed – fix them in %s and run again (imported ones are skipped)", failed, *file)
	}
	return nil
}

func userByEmail(ctx context.Context, pool *pgxpool.Pool, svc *users.Service, email string) (users.User, error) {
	var id uuid.UUID
	err := pool.QueryRow(ctx, `SELECT id FROM users WHERE email = lower($1)`, strings.TrimSpace(email)).Scan(&id)
	if err != nil {
		return users.User{}, err
	}
	return svc.Get(ctx, id)
}

func randomPassword() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// ------------------------------------------------------------- import-suppliers

type supplierImport struct {
	Source    string `json:"source"`
	Suppliers []struct {
		sourcing.SupplierInput
		Accounts []struct {
			sourcing.AccountInput
			SourceRow int `json:"source_row"`
		} `json:"accounts"`
	} `json:"suppliers"`
}

func runImportSuppliers(args []string) error {
	fs := flag.NewFlagSet("import-suppliers", flag.ExitOnError)
	file := fs.String("file", "scripts/import/suppliers-import.json", "supplier import file")
	as := fs.String("as", "", "email of the admin user recorded as creator in the audit log")
	dryRun := fs.Bool("dry-run", false, "check everything and report, but write nothing")
	_ = fs.Parse(args)
	if *as == "" {
		return errors.New("-as is required (the admin user the import runs as)")
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var in supplierImport
	if err := json.Unmarshal(raw, &in); err != nil {
		return fmt.Errorf("read %s: %w", *file, err)
	}
	ctx := context.Background()
	cfg, pool, log, err := connect(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	userSvc := users.NewService(pool, time.Now)
	admin, err := userByEmail(ctx, pool, userSvc, *as)
	if err != nil {
		return fmt.Errorf("-as %s: %w", *as, err)
	}
	if !admin.Principal().Can(sourcing.PermSuppliersManage) {
		return fmt.Errorf("%s (%s) cannot manage suppliers", admin.Email, admin.Role)
	}
	ctx = auth.WithPrincipal(ctx, admin.Principal())
	svc := sourcing.NewService(pool, numbering.New(cfg.Location), nil, nil, mail.New(mail.Config{}, pool, log), time.Now,
		sourcing.Options{Location: cfg.Location, Log: log})

	fmt.Printf("Importing %d suppliers from %s%s\n\n", len(in.Suppliers), in.Source,
		map[bool]string{true: " – DRY RUN, nothing is saved", false: ""}[*dryRun])
	var created, skipped, failed, accounts int
	for _, rec := range in.Suppliers {
		x := rec.SupplierInput
		x.Accounts = nil
		for _, a := range rec.Accounts {
			x.Accounts = append(x.Accounts, a.AccountInput)
		}
		label := fmt.Sprintf("%-42.42s", x.Name)
		var exists string
		_ = pool.QueryRow(ctx, `SELECT reference FROM suppliers WHERE name_key = $1`, sourcing.NameKey(x.Name)).Scan(&exists)
		if exists != "" {
			skipped++
			fmt.Printf("SKIP  %s already registered as %s\n", label, exists)
			continue
		}
		if *dryRun {
			created++
			accounts += len(x.Accounts)
			fmt.Printf("OK    %s %d account(s)\n", label, len(x.Accounts))
			continue
		}
		got, err := svc.CreateSupplier(ctx, x)
		if err != nil {
			failed++
			fmt.Printf("FAIL  %s %s\n", label, describe(err))
			continue
		}
		created++
		accounts += len(got.Accounts)
		fmt.Printf("OK    %s %s, %d account(s)\n", label, got.Reference, len(got.Accounts))
	}
	verb := "created"
	if *dryRun {
		verb = "would be created"
	}
	fmt.Printf("\n%d suppliers (%d accounts) %s, %d skipped as already registered, %d failed\n", created, accounts, verb, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d suppliers failed – fix them in %s and run again (imported ones are skipped)", failed, *file)
	}
	return nil
}
