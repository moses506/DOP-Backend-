package clients

import (
	"strings"
	"unicode"
)

// Words dropped when comparing company names, so "ABC Mining Ltd." and
// "abc mining limited" are recognised as the same client.
var nameNoise = map[string]bool{
	"the": true, "limited": true, "ltd": true, "plc": true, "inc": true,
	"company": true, "co": true, "corporation": true, "corp": true,
}

// NameKey returns the normalised form of a company name used for duplicate
// detection.
func NameKey(name string) string {
	name = strings.ToLower(strings.ReplaceAll(name, "&", " and "))
	words := strings.FieldsFunc(name, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(words))
	for _, w := range words {
		if !nameNoise[w] {
			out = append(out, w)
		}
	}
	if len(out) == 0 { // name was only noise words; keep them
		return strings.Join(words, " ")
	}
	return strings.Join(out, " ")
}

// normID strips spaces and dashes from registration and tax numbers.
func normID(s *string) *string {
	if s == nil {
		return nil
	}
	v := strings.ToUpper(strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '/' {
			return -1
		}
		return r
	}, *s))
	if v == "" {
		return nil
	}
	return &v
}
