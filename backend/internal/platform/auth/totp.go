package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Time-based one-time codes (TOTP, RFC 6238): the 6-digit codes shown by
// authenticator apps such as Google or Microsoft Authenticator. SHA-1,
// 30-second steps and 6 digits are the settings every app supports.
const (
	totpStep   = 30
	totpDigits = 6
)

// NewTOTPSecret returns a random 160-bit secret.
func NewTOTPSecret() []byte {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return b
}

// TOTPKey is the secret as people type it into an app: base32, in groups of four.
func TOTPKey(secret []byte) string {
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	var parts []string
	for len(s) > 4 {
		parts = append(parts, s[:4])
		s = s[4:]
	}
	return strings.Join(append(parts, s), " ")
}

// TOTPURI is what the QR code holds, so the app can add the account in one scan.
func TOTPURI(secret []byte, issuer, account string) string {
	key := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret)
	v := url.Values{}
	v.Set("secret", key)
	v.Set("issuer", issuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", fmt.Sprint(totpDigits))
	v.Set("period", fmt.Sprint(totpStep))
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + v.Encode()
}

// TOTPCode returns the code for a given step.
func TOTPCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", totpDigits, n%1_000_000)
}

func TOTPStep(t time.Time) int64 { return t.Unix() / totpStep }

// VerifyTOTP checks a code against the current step and one step either side
// (for clock drift). It returns the matched step, which must be greater than
// lastStep so a code can't be replayed.
func VerifyTOTP(secret []byte, code string, now time.Time, lastStep int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != totpDigits {
		return 0, false
	}
	cur := TOTPStep(now)
	for _, s := range []int64{cur, cur - 1, cur + 1} {
		if s <= lastStep {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(TOTPCode(secret, s)), []byte(code)) == 1 {
			return s, true
		}
	}
	return 0, false
}
