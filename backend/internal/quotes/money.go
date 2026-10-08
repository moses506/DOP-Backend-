package quotes

import (
	"errors"
	"math/big"
	"strconv"
	"strings"
)

// Amounts are worked out in whole hundredths (ngwee or cents for money,
// hundredths for quantities and percentages) so totals never drift the way
// floating-point numbers do. Each line is rounded once, half up, and the
// quotation totals are the sums of the rounded lines, so the printed lines
// always add up to the printed total.

// hundredths is a non-negative number with two decimal places, stored ×100.
type hundredths int64

var errNumber = errors.New("not a number")

// parseHundredths reads "1,250", "1250.5" or "1 250.50". More than two decimal places is an error.
func parseHundredths(s string) (hundredths, error) {
	s = strings.NewReplacer(",", "", " ", "", " ", "").Replace(strings.TrimSpace(s))
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
	return hundredths(w*100 + f), nil
}

// String renders 125050 as "1250.50".
func (h hundredths) String() string {
	neg := h < 0
	if neg {
		h = -h
	}
	s := strconv.FormatInt(int64(h)/100, 10) + "." + pad2(int64(h)%100)
	if neg {
		return "-" + s
	}
	return s
}

// Grouped renders 125050 as "1,250.50", for documents.
func (h hundredths) Grouped() string {
	s := h.String()
	whole, frac, _ := strings.Cut(s, ".")
	neg := strings.HasPrefix(whole, "-")
	whole = strings.TrimPrefix(whole, "-")
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	out := b.String() + "." + frac
	if neg {
		return "-" + out
	}
	return out
}

// Trimmed renders a quantity or percentage without needless zeros: 2.00 → "2", 12.50 → "12.5".
func (h hundredths) Trimmed() string {
	s := h.String()
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

func pad2(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}

// divRound divides num by den, rounding half up (both positive).
func divRound(num, den *big.Int) hundredths {
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Mul(r, big.NewInt(2)).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	return hundredths(q.Int64())
}

// lineAmounts works out one line: gross (before discount), net, VAT and total, all in hundredths of the currency.
//
//	gross = quantity × price
//	net   = gross × (1 − discount%)
//	vat   = net × VAT%
func lineAmounts(qty, price, discountPct, vatPct hundredths) (gross, net, vat, total hundredths) {
	q, p := big.NewInt(int64(qty)), big.NewInt(int64(price))
	qp := new(big.Int).Mul(q, p) // ×100 ×100
	gross = divRound(qp, big.NewInt(100))
	keep := big.NewInt(10000 - int64(discountPct)) // percentage ×100
	net = divRound(new(big.Int).Mul(qp, keep), big.NewInt(100*10000))
	vat = divRound(new(big.Int).Mul(big.NewInt(int64(net)), big.NewInt(int64(vatPct))), big.NewInt(10000))
	return gross, net, vat, net + vat
}

// percentOf is part ÷ whole as a percentage in hundredths (e.g. 12.5% → 1250).
func percentOf(part, whole hundredths) hundredths {
	if whole <= 0 || part <= 0 {
		return 0
	}
	return divRound(new(big.Int).Mul(big.NewInt(int64(part)), big.NewInt(10000)), big.NewInt(int64(whole)))
}
