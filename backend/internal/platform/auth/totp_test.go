package auth

import (
	"testing"
	"time"
)

// RFC 6238 appendix B test vectors (SHA-1, last 6 digits).
func TestTOTPMatchesRFC6238(t *testing.T) {
	secret := []byte("12345678901234567890")
	for unix, want := range map[int64]string{
		59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037",
	} {
		if got := TOTPCode(secret, TOTPStep(time.Unix(unix, 0))); got != want {
			t.Errorf("t=%d: got %s, want %s", unix, got, want)
		}
	}
}

func TestTOTPRejectsReplayAndOldCodes(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Now()
	code := TOTPCode(secret, TOTPStep(now))
	step, ok := VerifyTOTP(secret, code, now, 0)
	if !ok {
		t.Fatal("current code rejected")
	}
	if _, ok := VerifyTOTP(secret, code, now, step); ok {
		t.Fatal("same code accepted twice")
	}
	old := TOTPCode(secret, TOTPStep(now)-3)
	if _, ok := VerifyTOTP(secret, old, now, 0); ok {
		t.Fatal("90-second-old code accepted")
	}
	if _, ok := VerifyTOTP(secret, "12345", now, 0); ok {
		t.Fatal("short code accepted")
	}
}
