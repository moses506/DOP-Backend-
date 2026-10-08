package auth

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("a-long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(h, "a-long-enough-password") {
		t.Fatal("correct password rejected")
	}
	if CheckPassword(h, "wrong-password-123") {
		t.Fatal("wrong password accepted")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}
}

func TestRolePermissions(t *testing.T) {
	cases := []struct {
		role Role
		perm string
		want bool
	}{
		{RoleAdmin, "anything:at_all", true},
		{RoleCEO, "invoices:read", true},
		{RoleCEO, "invoices:issue", false},
		{RoleSalesExecutive, "clients:create", true},
		{RoleSalesExecutive, "clients:deactivate", false},
		{RoleSalesExecutive, "clients:override_duplicate", false},
		{RoleSalesManager, "clients:override_duplicate", true},
		{RoleTechnical, "clients:update", false},
	}
	for _, c := range cases {
		if got := c.role.Can(c.perm); got != c.want {
			t.Errorf("%s.Can(%s) = %v, want %v", c.role, c.perm, got, c.want)
		}
	}
}

func TestTokens(t *testing.T) {
	tk := NewTokens([]byte("0123456789abcdef0123456789abcdef"), time.Hour)
	p := Principal{UserID: uuid.New(), Name: "Jane", Position: "Sales Executive", Role: RoleSalesExecutive}
	raw, _, err := tk.Issue(p, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, err := tk.Parse(raw)
	if err != nil || got != p {
		t.Fatalf("parse: %v %+v", err, got)
	}
	expired, _, _ := tk.Issue(p, time.Now().Add(-2*time.Hour))
	if _, err := tk.Parse(expired); err == nil {
		t.Fatal("expired token accepted")
	}
	other := NewTokens([]byte("ffffffffffffffffffffffffffffffff"), time.Hour)
	if _, err := other.Parse(raw); err == nil {
		t.Fatal("token signed with another key accepted")
	}
}
