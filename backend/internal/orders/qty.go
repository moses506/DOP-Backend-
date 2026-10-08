package orders

import (
	"errors"
	"strconv"
	"strings"
)

// qty is a non-negative quantity with two decimal places, stored ×100, so
// comparisons ("is everything delivered?") are exact.
type qty int64

var errNumber = errors.New("not a number")

func parseQty(s string) (qty, error) {
	s = strings.NewReplacer(",", "", " ", "").Replace(strings.TrimSpace(s))
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
	if err != nil || w > 1e10 {
		return 0, errNumber
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, errNumber
	}
	return qty(w*100 + f), nil
}

func mustQty(s string) qty {
	q, _ := parseQty(s)
	return q
}

// String renders 250 as "2.50" and 300 as "3".
func (q qty) String() string {
	w, f := int64(q)/100, int64(q)%100
	if f == 0 {
		return strconv.FormatInt(w, 10)
	}
	s := strconv.FormatInt(w, 10) + "." + strconv.FormatInt(f/10, 10) + strconv.FormatInt(f%10, 10)
	return strings.TrimSuffix(s, "0")
}

// progress works out how far along the lines are, as percentages of the
// total quantity, and the status that follows from it.
func progress(lines []Line) (readyPct, deliveredPct int, st Status) {
	var total, ready, delivered qty
	allReady, allDelivered, anyStarted, anyDelivered := true, true, false, false
	for _, l := range lines {
		q, r, d := mustQty(l.Quantity), mustQty(l.QtyReady), mustQty(l.QtyDelivered)
		if r < d {
			r = d // delivered goods were ready
		}
		total += q
		ready += r
		delivered += d
		if r < q {
			allReady = false
		}
		if d < q {
			allDelivered = false
		}
		if r > 0 || l.SupplierNote != nil {
			anyStarted = true
		}
		if d > 0 {
			anyDelivered = true
		}
	}
	if total > 0 {
		readyPct = int(int64(ready) * 100 / int64(total))
		deliveredPct = int(int64(delivered) * 100 / int64(total))
	}
	switch {
	case len(lines) == 0:
		st = StatusOpen
	case allDelivered:
		st = StatusDelivered
	case anyDelivered:
		st = StatusPartDelivered
	case allReady:
		st = StatusReady
	case anyStarted:
		st = StatusInProgress
	default:
		st = StatusOpen
	}
	return
}
