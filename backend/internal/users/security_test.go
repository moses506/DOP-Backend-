package users_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/platform/db"
	"dos-bpas/internal/platform/httpx"
	"dos-bpas/internal/platform/migrate"
	platmig "dos-bpas/internal/platform/migrations"
	"dos-bpas/internal/users"
)

func TestPasswordRules(t *testing.T) {
	cases := map[string]bool{ // password -> acceptable (for Mary Phiri, mary.phiri@dos.test)
		"short1!":                 false,
		"Password2026!":           false, // common word with decoration
		"Lusaka@12345":            false, // local common word
		"1234567890":              false, // numbers only
		"qwertyuiop1":             false, // keyboard run
		"aaaaaaaaaaaa":            false,
		"MaryPhiri2026":           false, // own name
		"mary.phiri99":            false, // own email
		"copper kettle sunrise":   true,
		"Tr4ctor-Blue-Ndola-77x":  true,
		"maryland-harbour-lights": true, // contains "mary" but far more besides
	}
	for pw, ok := range cases {
		got := users.PasswordProblem(pw, "mary.phiri@dos.test", "Mary Phiri")
		if (got == "") != ok {
			t.Errorf("%q: acceptable=%v, problem=%q", pw, got == "", got)
		}
	}
}

func setup(t *testing.T) *users.Service {
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
	if err := migrate.Run(ctx, pool, log, []migrate.Source{{Module: "platform", FS: platmig.FS}}); err != nil {
		t.Fatal(err)
	}
	return users.NewService(pool, time.Now)
}

func code(err error) string {
	var e *httpx.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestLoginLockout(t *testing.T) {
	svc := setup(t)
	ctx := context.Background()
	if _, err := svc.Create(ctx, users.CreateInput{Email: "grace@dos.test", FullName: "Grace Mwale", Position: "Sales", Department: "Sales",
		Role: auth.RoleSalesExecutive, Password: "copper kettle sunrise"}); err != nil {
		t.Fatal(err)
	}
	try := func(email, pw, ip string) error {
		_, err := svc.Login(ctx, users.Attempt{Email: email, Password: pw, IP: ip})
		return err
	}

	// Four wrong passwords are just wrong; the fifth pauses the account...
	for i := 1; i <= 4; i++ {
		if c := code(try("grace@dos.test", "wrong password!", "10.0.0.1")); c != "bad_credentials" {
			t.Fatalf("attempt %d: %s", i, c)
		}
	}
	if c := code(try("grace@dos.test", "wrong password!", "10.0.0.1")); c != "too_many_attempts" {
		t.Fatalf("5th attempt: %s", c)
	}
	// ...even the right password is refused while paused, from any address.
	if c := code(try("Grace@DOS.test", "copper kettle sunrise", "10.0.0.2")); c != "too_many_attempts" {
		t.Fatalf("right password while locked: %s", c)
	}
	// An email with no account behaves identically, so locks don't reveal who has an account.
	for i := 1; i <= 4; i++ {
		if c := code(try("nobody@dos.test", "whatever pass", "10.0.0.3")); c != "bad_credentials" {
			t.Fatalf("unknown attempt %d: %s", i, c)
		}
	}
	if c := code(try("nobody@dos.test", "whatever pass", "10.0.0.3")); c != "too_many_attempts" {
		t.Fatalf("unknown 5th: %s", c)
	}
	// An administrator's reset (or a set-up link) lifts the pause.
	svc.Unlock(ctx, "grace@dos.test")
	if err := try("grace@dos.test", "copper kettle sunrise", "10.0.0.2"); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
}

func TestNetworkLimit(t *testing.T) {
	svc := setup(t)
	ctx := context.Background()
	// One address trying many different accounts is stopped too.
	var last error
	for i := 0; i < users.NetworkMaxFailures+1; i++ {
		_, last = svc.Login(ctx, users.Attempt{Email: "user" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "@x.test", Password: "nope nope nope", IP: "203.0.113.9"})
	}
	if code(last) != "too_many_attempts" {
		t.Fatalf("network limit not applied: %v", last)
	}
}

func TestSessions(t *testing.T) {
	svc := setup(t)
	ctx := context.Background()
	u, err := svc.Create(ctx, users.CreateInput{Email: "joe@dos.test", FullName: "Joe Banda", Position: "Ops", Department: "Ops",
		Role: auth.RoleOperations, Password: "copper kettle sunrise"})
	if err != nil {
		t.Fatal(err)
	}
	open := func() auth.Principal {
		sid, _, err := svc.NewSession(ctx, u.ID, "10.0.0.1", "test", time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		p := u.Principal()
		p.SessionID, p.Active, p.IssuedAt = sid, true, time.Now().Add(time.Second)
		return p
	}

	a, b := open(), open()
	if err := svc.CheckSession(ctx, &a); err != nil {
		t.Fatalf("fresh session rejected: %v", err)
	}
	// Signing out ends that session only.
	if err := svc.RevokeSession(ctx, a.SessionID, "signed_out"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(svc.CheckSession(ctx, &a), auth.ErrSessionRevoked) {
		t.Fatal("signed-out session still accepted")
	}
	if err := svc.CheckSession(ctx, &b); err != nil {
		t.Fatalf("other session affected: %v", err)
	}
	// A token without a server-side session is refused.
	stray := u.Principal()
	if !errors.Is(svc.CheckSession(ctx, &stray), auth.ErrSessionRevoked) {
		t.Fatal("token without session accepted")
	}
	// "Sign out everywhere" ends all of them.
	if err := svc.RevokeAllSessions(ctx, u.ID, "signed_out_everywhere"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(svc.CheckSession(ctx, &b), auth.ErrSessionRevoked) {
		t.Fatal("session survived sign out everywhere")
	}
}

func TestIdleSessionEnds(t *testing.T) {
	svc := setup(t)
	ctx := context.Background()
	u, err := svc.Create(ctx, users.CreateInput{Email: "ann@dos.test", FullName: "Ann Zulu", Position: "Ops", Department: "Ops",
		Role: auth.RoleOperations, Password: "copper kettle sunrise"})
	if err != nil {
		t.Fatal(err)
	}
	sid, _, err := svc.NewSession(ctx, u.ID, "", "", time.Hour*12)
	if err != nil {
		t.Fatal(err)
	}
	p := u.Principal()
	p.SessionID = sid
	// Two hours on, with only background requests in between, the session has ended.
	svcLater := svc.WithClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	if !errors.Is(svcLater.CheckSession(ctx, &p), auth.ErrSessionRevoked) {
		t.Fatal("idle session still accepted")
	}
}
