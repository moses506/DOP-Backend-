package users_test

import (
	"context"
	"encoding/base32"
	"strings"
	"testing"
	"time"

	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/users"
)

func secretFromKey(t *testing.T, key string) []byte {
	t.Helper()
	b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ReplaceAll(key, " ", ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTwoStepLifecycle(t *testing.T) {
	svc := setup(t)
	ctx := context.Background()
	u, err := svc.Create(ctx, users.CreateInput{Email: "lisa@dos.test", FullName: "Lisa Tembo", Position: "Ops", Department: "Ops",
		Role: auth.RoleOperations, Password: "copper kettle sunrise"})
	if err != nil {
		t.Fatal(err)
	}
	if u.MFARequired {
		t.Fatal("operations shouldn't require two-step by default")
	}
	key, uri, err := svc.BeginMFASetup(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri, "otpauth://totp/DOS%20BPA:lisa@dos.test?") {
		t.Fatalf("uri = %s", uri)
	}
	secret := secretFromKey(t, key)

	// A wrong first code doesn't switch it on.
	if _, err := svc.EnableMFA(ctx, u.ID, "000000"); err == nil {
		t.Fatal("enabled with a wrong code")
	}
	now := time.Now()
	codes, err := svc.EnableMFA(ctx, u.ID, auth.TOTPCode(secret, auth.TOTPStep(now)))
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != users.RecoveryCodeCount {
		t.Fatalf("got %d recovery codes", len(codes))
	}
	// The secret is stored encrypted, not as the key the app shows.
	var stored []byte
	if err := svc.Pool().QueryRow(ctx, `SELECT mfa_secret FROM users WHERE id = $1`, u.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), string(secret)) {
		t.Fatal("secret stored in plain text")
	}

	// The code used to switch it on can't be used again (replay).
	if _, err := svc.VerifySecondFactor(ctx, u.ID, auth.TOTPCode(secret, auth.TOTPStep(now))); code(err) != "bad_code" {
		t.Fatalf("replayed code: %v", err)
	}
	// The next code works once.
	next := auth.TOTPCode(secret, auth.TOTPStep(now)+1)
	if _, err := svc.VerifySecondFactor(ctx, u.ID, next); err != nil {
		t.Fatalf("next code: %v", err)
	}
	// A recovery code works once, written any way.
	if used, err := svc.VerifySecondFactor(ctx, u.ID, strings.ToUpper(codes[0])); err != nil || !used {
		t.Fatalf("recovery code: used=%v err=%v", used, err)
	}
	if _, err := svc.VerifySecondFactor(ctx, u.ID, codes[0]); code(err) != "bad_code" {
		t.Fatalf("recovery code reused: %v", err)
	}
	if left, _ := svc.RecoveryCodesLeft(ctx, u.ID); left != users.RecoveryCodeCount-1 {
		t.Fatalf("codes left = %d", left)
	}
	// Turning it off needs the password and a code.
	if err := svc.DisableMFA(ctx, u.ID, "wrong password!!", codes[1]); err == nil {
		t.Fatal("disabled with wrong password")
	}
	if err := svc.DisableMFA(ctx, u.ID, "copper kettle sunrise", codes[1]); err != nil {
		t.Fatalf("disable: %v", err)
	}
}

func TestTwoStepRequiredAndReset(t *testing.T) {
	svc := setup(t)
	ctx := context.Background()
	admin, _ := svc.Create(ctx, users.CreateInput{Email: "boss@dos.test", FullName: "Chanda Boss", Position: "IT", Department: "IT",
		Role: auth.RoleAdmin, Password: "copper kettle sunrise"})
	mgr, _ := svc.Create(ctx, users.CreateInput{Email: "mgr@dos.test", FullName: "Jane Banda", Position: "Sales manager", Department: "Sales",
		Role: auth.RoleSalesManager, Password: "copper kettle sunrise"})
	if !mgr.MFARequired || !admin.MFARequired {
		t.Fatal("managers and admins must require two-step")
	}
	key, _, _ := svc.BeginMFASetup(ctx, mgr.ID)
	secret := secretFromKey(t, key)
	if _, err := svc.EnableMFA(ctx, mgr.ID, auth.TOTPCode(secret, auth.TOTPStep(time.Now()))); err != nil {
		t.Fatal(err)
	}
	// Required roles can't switch it off themselves...
	if err := svc.DisableMFA(ctx, mgr.ID, "copper kettle sunrise", auth.TOTPCode(secret, auth.TOTPStep(time.Now())+1)); code(err) != "mfa_required" {
		t.Fatalf("required role disabled two-step: %v", err)
	}
	// ...an administrator resets it for a lost phone, but nobody resets their own.
	asAdmin := auth.WithPrincipal(ctx, admin.Principal())
	asMgr := auth.WithPrincipal(ctx, mgr.Principal())
	if _, err := svc.ResetMFA(asMgr, mgr.ID); code(err) != "own_account" {
		t.Fatalf("self reset: %v", err)
	}
	u, err := svc.ResetMFA(asAdmin, mgr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u.MFAEnabled {
		t.Fatal("still enabled after reset")
	}
}

func TestWrongCodesLockTheAccount(t *testing.T) {
	svc := setup(t)
	ctx := context.Background()
	u, _ := svc.Create(ctx, users.CreateInput{Email: "kim@dos.test", FullName: "Kim Lungu", Position: "Ops", Department: "Ops",
		Role: auth.RoleOperations, Password: "copper kettle sunrise"})
	key, _, _ := svc.BeginMFASetup(ctx, u.ID)
	if _, err := svc.EnableMFA(ctx, u.ID, auth.TOTPCode(secretFromKey(t, key), auth.TOTPStep(time.Now()))); err != nil {
		t.Fatal(err)
	}
	// Someone with the password keeps getting the password right but the code wrong.
	// The right password must not reset the count, or codes could be guessed forever.
	for i := 0; i < users.MaxFailedLogins; i++ {
		if _, err := svc.Login(ctx, users.Attempt{Email: "kim@dos.test", Password: "copper kettle sunrise", IP: "10.1.1.1"}); err != nil {
			t.Fatalf("round %d password: %v", i, err)
		}
		svc.RecordBadCode(ctx, "kim@dos.test", "10.1.1.1")
	}
	if _, err := svc.Login(ctx, users.Attempt{Email: "kim@dos.test", Password: "copper kettle sunrise", IP: "10.1.1.1"}); code(err) != "too_many_attempts" {
		t.Fatalf("account not locked after wrong codes: %v", err)
	}
}
