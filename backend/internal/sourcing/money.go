package sourcing

import (
	"errors"
	"math/big"
	"strconv"
	"strings"
)

// Amounts and quantities are kept in whole hundredths so totals are exact.
// Exchange rates have up to six decimal places; converted amounts are rounded
// once, half up, to the ngwee.

type cents int64

var errNumber = errors.New("not a number")

func parseMoney(s string) (cents, error) {
	s = strings.NewReplacer(",", "", " ", "", " ", "").Replace(strings.TrimSpace(s))
	s = strings.TrimLeft(s, "RrK$£")
	if s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		return 0, errNumber
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	if len(frac) > 2 {
		return 0, errNumber
	}
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > 1e12 {
		return 0, errNumber
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, errNumber
	}
	return cents(w*100 + f), nil
}

func (c cents) String() string {
	neg := c < 0
	if neg {
		c = -c
	}
	s := strconv.FormatInt(int64(c)/100, 10) + "." + pad2(int64(c)%100)
	if neg {
		s = "-" + s
	}
	return s
}

// Trimmed renders quantities: 300 → "3", 250 → "2.5".
func (c cents) Trimmed() string {
	s := c.String()
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

func pad2(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}

// parseRate reads an exchange rate like "1.48" or "26.8512" (at most six decimals).
func parseRate(s string) (string, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() <= 0 {
		return "", errNumber
	}
	if _, frac, _ := strings.Cut(s, "."); len(frac) > 6 {
		return "", errNumber
	}
	if r.Cmp(big.NewRat(1_000_000, 1)) > 0 {
		return "", errNumber
	}
	return r.FloatString(6), nil
}

// lineTotal is quantity × unit cost, both in hundredths, rounded half up.
func lineTotal(qty, unit cents) cents {
	return divRound(big.NewInt(int64(qty)*int64(unit)), big.NewInt(100))
}

// toZMW converts an amount with a rate (kwacha per unit), rounded half up.
func toZMW(amount cents, rate string) (cents, bool) {
	if rate == "" {
		return 0, false
	}
	r, ok := new(big.Rat).SetString(rate)
	if !ok {
		return 0, false
	}
	v := new(big.Rat).Mul(new(big.Rat).SetInt64(int64(amount)), r)
	return divRound(v.Num(), v.Denom()), true
}

func divRound(num, den *big.Int) cents {
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Mul(m, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	return cents(q.Int64())
}
