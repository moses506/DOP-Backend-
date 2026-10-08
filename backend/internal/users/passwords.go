package users

import (
	"strings"
	"unicode"
)

// Password rules (in line with NIST SP 800-63B): long enough, not a common or
// guessable password, and not built from the person's own name or email.
// No forced mixes of symbols — length and uniqueness matter far more.
const (
	MinPasswordLength = 10
	// MaxPasswordLength stops absurdly long input from tying up the server.
	MaxPasswordLength = 128
)

// commonWords are the cores of passwords attackers try first, including
// local ones. A password whose letters reduce to one of these is refused,
// so "Password2026!" and "Lusaka@12345" are caught too.
var commonWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`
		password passw0rd pass passwd secret letmein welcome welcomeback admin administrator root user login
		qwerty qwertyuiop asdf asdfgh asdfghjkl zxcvbnm abc abcdef abcdefg abcdefgh qwe qwer iloveyou love
		monkey dragon football soccer baseball sunshine princess master shadow superman batman michael jesus
		god blessed blessing trustno changeme default temp temporary test testing guest hello freedom whatever
		starwars computer internet samsung iphone android google microsoft office company business
		dos doslimited dosltd digitaloffice digitalofficesolutions dosworkflow workflow
		zambia lusaka kitwe ndola livingstone copperbelt kwacha zesco
		january february march april may june july august september october november december
		monday tuesday wednesday thursday friday saturday sunday summer winter spring autumn`) {
		commonWords[w] = true
	}
}

// PasswordProblem explains what's wrong with a password, or returns "" if it's acceptable.
func PasswordProblem(pw, email, fullName string) string {
	if len([]rune(pw)) < MinPasswordLength {
		return "Use at least 10 characters. A short phrase of three or four words works well."
	}
	if len(pw) > MaxPasswordLength {
		return "Use at most 128 characters."
	}
	lower := strings.ToLower(pw)
	letters := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, pw)
	switch {
	case letters == "":
		return "Use some letters too — a password made only of numbers or symbols is easy to guess."
	case allSame(lower):
		return "Don't repeat one character — choose something harder to guess."
	case commonWords[letters] || commonWords[strings.TrimRight(letters, "s")]:
		return "That password is too common and is one of the first that attackers try. Choose something less predictable."
	case isSequence(lower):
		return "Avoid keyboard or number sequences like 1234567890 or qwertyuiop."
	}
	// Not built around the person's own name or email.
	var personal []string
	if local, _, ok := strings.Cut(strings.ToLower(email), "@"); ok {
		personal = append(personal, strings.FieldsFunc(local, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })...)
		personal = append(personal, local)
	}
	personal = append(personal, strings.Fields(strings.ToLower(fullName))...)
	// Take every name or email part out; if hardly any letters are left, the
	// password is really just the person's own details.
	rest, found := letters, false
	for _, p := range personal {
		p = strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) {
				return r
			}
			return -1
		}, p)
		if len([]rune(p)) >= 3 && strings.Contains(rest, p) {
			rest, found = strings.ReplaceAll(rest, p, ""), true
		}
	}
	if found && len([]rune(rest)) < 4 {
		return "Don't base your password on your name or email address."
	}
	return ""
}

func allSame(s string) bool {
	r := []rune(s)
	for _, c := range r[1:] {
		if c != r[0] {
			return false
		}
	}
	return true
}

// isSequence spots passwords that are mostly a run along a keyboard row or
// the digits, e.g. "1234567890", "qwertyuiop1".
func isSequence(s string) bool {
	rows := []string{"0123456789012", "9876543210987", "qwertyuiop", "poiuytrewq", "asdfghjkl", "lkjhgfdsa", "zxcvbnm", "mnbvcxz", "abcdefghijklmnopqrstuvwxyz"}
	for _, row := range rows {
		for n := len(s); n >= 8; n-- {
			for i := 0; i+n <= len(s); i++ {
				if strings.Contains(row, s[i:i+n]) && n >= len(s)-2 {
					return true
				}
			}
		}
	}
	return false
}
