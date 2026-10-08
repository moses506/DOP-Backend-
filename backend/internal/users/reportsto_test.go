package users_test

import (
	"context"
	"testing"

	"dos-bpas/internal/platform/auth"
	"dos-bpas/internal/users"
)

func TestReportsTo(t *testing.T) {
	svc := setup(t)
	ctx := context.Background()
	mk := func(email string, role auth.Role, mgr *users.User) users.User {
		t.Helper()
		in := users.CreateInput{Email: email, FullName: "Person " + email, Position: "Sales", Department: "Sales",
			Role: role, Password: "copper kettle sunrise", Phone: "+260 97 1234567"}
		if mgr != nil {
			in.ManagerID = &mgr.ID
		}
		u, err := svc.Create(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	boss := mk("boss@dos.test", auth.RoleSalesManager, nil)
	lead := mk("lead@dos.test", auth.RoleSalesExecutive, &boss)
	exec := mk("exec@dos.test", auth.RoleSalesExecutive, &lead)
	if exec.ManagerID == nil || *exec.ManagerID != lead.ID || exec.Phone == nil {
		t.Fatalf("exec = %+v", exec)
	}
	admin := mk("admin@dos.test", auth.RoleAdmin, nil)
	as := auth.WithPrincipal(ctx, admin.Principal())
	set := func(u users.User, mgr string) error {
		_, err := svc.Update(as, u.ID, users.UpdateInput{ManagerID: &mgr})
		return err
	}

	// No reporting to yourself, and no loops however deep.
	if err := set(boss, boss.ID.String()); code(err) != "validation_failed" {
		t.Fatalf("self err = %v", err)
	}
	if err := set(boss, lead.ID.String()); code(err) != "validation_failed" {
		t.Fatalf("2-loop err = %v", err)
	}
	if err := set(boss, exec.ID.String()); code(err) != "validation_failed" {
		t.Fatalf("3-loop err = %v", err)
	}
	// Moving exec straight under the boss is fine; clearing works; other fields are kept.
	if err := set(exec, boss.ID.String()); err != nil {
		t.Fatal(err)
	}
	u, err := svc.Update(as, exec.ID, users.UpdateInput{FullName: "Exec Renamed"})
	if err != nil || u.ManagerID == nil || *u.ManagerID != boss.ID {
		t.Fatalf("kept manager = %+v, %v", u.ManagerID, err)
	}
	if err := set(exec, ""); err != nil {
		t.Fatal(err)
	}
	if u, _ := svc.Get(ctx, exec.ID); u.ManagerID != nil {
		t.Fatalf("cleared manager = %v", u.ManagerID)
	}
	// Not to someone who has left, nor to nobody.
	if _, err := svc.SetActive(as, lead.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := set(exec, lead.ID.String()); code(err) != "validation_failed" {
		t.Fatalf("inactive manager err = %v", err)
	}
	if err := set(exec, "00000000-0000-0000-0000-000000000001"); code(err) != "validation_failed" {
		t.Fatalf("unknown manager err = %v", err)
	}
	if err := set(exec, "not-a-uuid"); code(err) != "validation_failed" {
		t.Fatalf("bad id err = %v", err)
	}
}
